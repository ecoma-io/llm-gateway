// The executor registry and its refresher: which backends this process can
// call, and how.
//
// The registry is the composition root's answer to a catalog that lives in a
// database while provider calls must not read one. It holds a snapshot — one
// frozen executor per callable backend, everything resolved at build time —
// and replaces the snapshot whole when the catalog changes. The walk's lookup
// (`For`) is a map read behind an atomic pointer: I/O-free, never blocking,
// and identical on every call between two refreshes, so an attempt row's
// meaning never shifts under a request in flight. A refresh whose read fails
// changes nothing: the last good snapshot keeps serving until a read
// succeeds, because the alternative — an empty registry — would turn one
// bad read into every answer being no_candidate.
//
// The resolution rules are this file's because this is the module's one place
// an adapter is chosen (the file above says so, and the architecture tests
// hold it): which adapter types get executors, how a backend row's references
// become a credential closure and an egress dial, and what happens to a row
// that cannot be resolved — it is left out of the snapshot and named in the
// log, never guessed into callability.
package main

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/adapters/outbound/egress"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/adapters/outbound/openaicompatible"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/config"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/catalog"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/credentials"
	egressport "github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/egress"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/executors"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/persistence"
)

const (
	// openaiCompatibleAdapter is the adapter type this build registers
	// executors for. A row of any other type is real catalog data — the
	// adapter roster is expected to grow — and it simply has no executor
	// here: candidates pointing at it resolve no executor and the walk
	// treats them as unavailable, exactly the shape of a backend that does
	// not exist yet.
	openaiCompatibleAdapter = "openai-compatible"

	// chatCompletionsPath is the path appended to a backend endpoint that
	// does not already name it. The catalog stores the provider's base URL;
	// the one wire this executor speaks has one path.
	chatCompletionsPath = "/chat/completions"

	// egressCooldown is the base pause a route serves after failing to
	// establish, before the policy prefers it again. It doubles per
	// consecutive failure and saturates inside the egress layer; the value
	// is a policy decision and lives here, beside the only code that turns
	// configured policies into dialers. Five seconds: a failed hop is
	// retried soon, but not within the same provider call's own failover.
	egressCooldown = 5 * time.Second
)

// executorRegistry is the runtime's snapshot of its executors, replaced whole
// on refresh. The pointer swap is the whole of the concurrency story: a reader
// sees either the old snapshot or the new one, and each is complete.
type executorRegistry struct {
	snapshot atomic.Pointer[map[catalog.BackendID]executors.Executor]
}

// Compile-time proof the registry satisfies the port the routing stage reads.
var _ executors.Registry = (*executorRegistry)(nil)

// For resolves one backend's executor from the current snapshot. A backend
// with no executor — absent, another adapter type, or unresolvable at the
// last build — reports false, which the walk reads as "not callable now".
func (r *executorRegistry) For(backendID catalog.BackendID) (executors.Executor, bool) {
	entries := r.snapshot.Load()
	if entries == nil {
		return nil, false
	}
	executor, ok := (*entries)[backendID]
	return executor, ok
}

// store replaces the snapshot whole.
func (r *executorRegistry) store(entries map[catalog.BackendID]executors.Executor) {
	r.snapshot.Store(&entries)
}

// buildExecutors resolves one snapshot from backend rows: one frozen executor
// per row this build can serve. Rows that do not resolve are left out and
// logged — a backend the operator named but that cannot be called is worth a
// line, not a silent absence and not a fabricated executor.
func buildExecutors(rows []catalog.Backend, cfg config.Config) map[catalog.BackendID]executors.Executor {
	entries := make(map[catalog.BackendID]executors.Executor, len(rows))
	for _, row := range rows {
		if row.AdapterType != openaiCompatibleAdapter {
			continue
		}
		target, err := resolveTarget(row, cfg)
		if err != nil {
			log.Printf("dataplane executor registry: backend %s is left out of the snapshot, it cannot be called: %v", row.ID, err)
			continue
		}
		entries[row.ID] = openaicompatible.New(target)
	}
	return entries
}

// resolveTarget turns one backend row into one frozen executor's wiring: the
// chat-completions endpoint, the credential closure one call resolves
// through, and the dial its bytes travel on.
//
// The header timeout is passed as zero on purpose. The obvious reading —
// bound the provider's time to first response headers — is wrong for this
// wire: on a chat completion the first body byte IS the answer, and the one
// budget that bounds a provider's silence is the walk's execution ceiling on
// the call's context. A header timeout here would be a second, smaller clock
// whose expiry the vocabulary would read as a stalled provider.
func resolveTarget(row catalog.Backend, cfg config.Config) (openaicompatible.Target, error) {
	if err := validateEndpoint(row.Endpoint); err != nil {
		return openaicompatible.Target{}, err
	}
	dial, err := resolveDial(row.EgressPolicyRef, cfg)
	if err != nil {
		return openaicompatible.Target{}, err
	}
	credential, err := resolveCredential(row.CredentialsRef)
	if err != nil {
		return openaicompatible.Target{}, err
	}
	endpoint := strings.TrimSuffix(row.Endpoint, "/")
	if !strings.HasSuffix(endpoint, chatCompletionsPath) {
		endpoint += chatCompletionsPath
	}
	return openaicompatible.Target{
		Endpoint:      endpoint,
		Credential:    credential,
		Dial:          dial,
		HeaderTimeout: 0, // see above: the call's context carries the one budget
	}, nil
}

// validateEndpoint applies the endpoint rule the adapter enforces by panic: a
// usable provider URL names http or https, carries a host, and carries no
// userinfo. The catalog's schema CHECK accepts a wider grammar — any
// `https?://` prefix — so a row the store happily holds can still fail here,
// and the rule lives in both places on purpose: the adapter's check is the
// contract's backstop for every builder that ever exists, while this one is
// the reading that turns a schema-legal row the build cannot call into a
// skip-and-log — the snapshot's law for an unresolvable row — instead of a
// panic in the goroutine that builds the snapshot.
func validateEndpoint(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil {
		return fmt.Errorf("endpoint %q is not a usable provider URL (scheme, host, no userinfo)", endpoint)
	}
	return nil
}

// resolveCredential turns a backend row's credential reference into the
// closure one call resolves its bearer material through. The grammar is the
// credentials package's one scheme — `env:NAME` — and the resolution happens
// at use, so a rotated environment variable is picked up by the next call
// rather than the next restart.
//
// An absent reference is legitimate catalog data, not a defect: its closure
// always reports absence, and every call then fails as authentication before
// anything leaves the process. That is the fail-closed reading — a backend
// wired without material must not quietly vanish from the walk's answers,
// and must not send an unauthenticated call either.
func resolveCredential(ref string) (func() (string, bool), error) {
	if ref == "" {
		return func() (string, bool) { return "", false }, nil
	}
	parsed, err := credentials.ParseRef(ref)
	if err != nil {
		return nil, err
	}
	return func() (string, bool) {
		secret, ok := parsed.Resolve(os.LookupEnv)
		if !ok {
			return "", false
		}
		return secret.Material(), true
	}, nil
}

// resolveDial turns a backend row's egress reference into the dial its calls
// travel on. The empty reference and the reserved word `direct` are the plain
// dialer; anything else names a policy in the process configuration, whose
// ordered route list becomes the policy's preference order, each hop failed
// establishment cooling before it is preferred again.
//
// A name the configuration does not define is an operator's unfinished
// wiring: the backend is skipped and named in the log, because choosing a
// route for a backend that named one is how traffic leaves by a road nobody
// chose.
func resolveDial(ref string, cfg config.Config) (egressport.DialFunc, error) {
	if ref == "" || ref == "direct" {
		// The reserved word is config's `reservedEgressDirectName`; spelled
		// here as the literal the configuration's own doctrine names.
		return egress.Direct(), nil
	}
	policy, ok := cfg.Egress.Policies[ref]
	if !ok {
		return nil, fmt.Errorf("egress policy %q is not defined in DATAPLANE_EGRESS_POLICIES", ref)
	}
	if len(policy.Routes) == 0 {
		// The loader refuses an empty policy; the guard is the honest
		// reading of the row that would otherwise dial nowhere.
		return nil, fmt.Errorf("egress policy %q has no routes", ref)
	}
	routes := make([]egressport.DialFunc, 0, len(policy.Routes))
	for _, route := range policy.Routes {
		switch route.Type {
		case "direct":
			routes = append(routes, egress.Direct())
		case "http-connect":
			routes = append(routes, egress.Connect(route.Addr))
		case "socks5":
			routes = append(routes, egress.SOCKS5(route.Addr, false))
		case "socks5h":
			routes = append(routes, egress.SOCKS5(route.Addr, true))
		default:
			// The loader refuses an unknown type; the guard names the
			// row rather than dialing a word the adapter never spoke.
			return nil, fmt.Errorf("egress route type %q is not one the runtime dials", route.Type)
		}
	}
	return egress.NewPolicy(routes, egressCooldown).Dial, nil
}

// snapshotSignature renders the rows a snapshot was built from as one
// comparable string. A refresh whose rows read the same as the last build's
// changes nothing: the executors are frozen, so an identical catalog would
// only rebuild identical executors and retire transports that may hold live
// streams. UpdatedAt rides in the signature, so an operator edit that
// happened to produce identical text is still a new snapshot.
func snapshotSignature(rows []catalog.Backend) string {
	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		parts = append(parts, fmt.Sprintf("%s|%s|%s|%s|%s|%s|%d",
			row.ID, row.AdapterType, row.Endpoint, row.CredentialsRef, row.EgressPolicyRef,
			row.State, row.UpdatedAt.UnixNano()))
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}

// refreshExecutors keeps the registry's snapshot current for the life of the
// context it is given: one whole read of the backend catalog per interval, a
// swap only when the catalog actually changed. The signature argument is the
// boot snapshot's, so the first comparison runs against what is actually
// serving — a first refresh over an unchanged catalog swaps nothing, and a
// first refresh over a genuinely emptied catalog still performs the one swap
// it owes. The loop exits when the context does, which is the process
// stopping; the registry needs no close, because a snapshot is data, not a
// resource.
func refreshExecutors(ctx context.Context, backends persistence.Backends, registry *executorRegistry, cfg config.Config, signature string) {
	ticker := time.NewTicker(cfg.ExecutionRegistryRefresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			signature = refreshOnce(ctx, backends, registry, cfg, signature)
		}
	}
}

// refreshOnce is one tick's whole work: one bounded read, one swap when the
// rows changed, and the signature the next tick compares against — the last
// good one when the read failed, the given one when the rows read the same,
// the new one when a snapshot was built. The read carries its own budget of
// one refresh interval, so a database that cannot answer cannot stack reads
// or hold the loop past its own cadence.
func refreshOnce(ctx context.Context, backends persistence.Backends, registry *executorRegistry, cfg config.Config, signature string) string {
	readCtx, cancel := context.WithTimeout(ctx, cfg.ExecutionRegistryRefresh)
	defer cancel()
	rows, err := backends.List(readCtx)
	if err != nil {
		log.Printf("dataplane executor registry: refresh read failed, the last good snapshot keeps serving: %v", err)
		return signature
	}
	if next := snapshotSignature(rows); next == signature {
		return signature
	} else {
		signature = next
	}
	entries := buildExecutors(rows, cfg)
	registry.store(entries)
	log.Printf("dataplane executor registry: snapshot refreshed, %d callable backend(s) of %d row(s)", len(entries), len(rows))
	return signature
}
