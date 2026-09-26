//go:build integration

package main

// The routing stage composed against the real adapters and a real PostgreSQL
// — the other half of what the adapter tree cannot host. The rules are the
// admission suite's, stated there and kept here: the composition mirrors
// bind()'s statement for statement, the fixture protocol is the shared
// compose project and the `dataplane` database, unique-per-run accounts keep
// the shared schema untruncated, and this file carries the compact fixture
// helpers it needs beside the admission suite's, reusing them where they are
// exactly what it means.
//
// What these tests prove is the walk's behaviour over the engine's
// guarantees — that an unroutable admission is released whole in one
// transaction, that a walk that exhausted every candidate releases under the
// walk's own no-candidate name with its attempts standing behind it, that a
// fall-through leaves both attempts recorded and settles
// on the survivor, that a surfaced refusal is released under its failure
// reason and replays as that refusal, and that a mid-stream death settles on
// what was delivered with the hold as its ceiling. The walk's own judgment —
// eligibility, order, the disposition tables — is the domain and application
// suites' business; this file is what the endings add over real storage.
//
// Run (from apps/dataplane):
//
//	POSTGRES_TEST_ADMIN_DSN='postgres://gateway:gateway-dev-only@127.0.0.1:5432/dataplane?sslmode=disable' \
//	  go test -tags=integration ./cmd/dataplane -run 'TestIntegrationRouting'

import (
	"context"
	"database/sql"
	"encoding/json"
	stdhttp "net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/adapters/outbound/postgres"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/config"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/catalog"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/execution"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/dataplane/internal/ports/outbound/executors"
)

// routingFixtureAliasName is this suite's own alias name — distinct from the
// admission suite's and from the landed harness's, so the suites never
// converge on one alias's rows.
const routingFixtureAliasName = "b9c4-route-alias"

// routingFixture is one test's world: the admission fixture it admits
// through — pool, store, admission, the residue reads — with the routing
// stage composed over the same repositories, and the executor registry the
// test scripts.
type routingFixture struct {
	*admissionFixture

	routeAliasID  catalog.AliasID
	routePriceRev string
	executorOf    map[catalog.BackendID]executors.Executor
	routing       *application.ChatRouting
}

func newRoutingFixture(t *testing.T) *routingFixture {
	t.Helper()
	base := newAdmissionFixture(t, 8)
	f := &routingFixture{admissionFixture: base}
	f.routeAliasID, _, _ = admissionSeedCatalog(t, base.store, routingFixtureAliasName)
	f.routePriceRev = admissionSeedPrice(t, context.Background(), base.store, f.routeAliasID, 1_000_000, 2_000_000)
	f.executorOf = map[catalog.BackendID]executors.Executor{}
	f.rewire(t)
	return f
}

// rewire composes the routing stage over the repositories the fixture holds
// and the registry's current entries — called again by tests that change the
// registry, because the registry is written once at construction.
func (f *routingFixture) rewire(t *testing.T) {
	t.Helper()
	entries := make(map[catalog.BackendID]executors.Executor, len(f.executorOf))
	for id, executor := range f.executorOf {
		entries[id] = executor
	}
	f.routing = application.NewChatRouting(
		f.store,
		postgres.NewModelAliases(f.store),
		postgres.NewBackends(f.store),
		postgres.NewRequestRepository(f.store),
		postgres.NewAttemptRepository(f.store),
		postgres.NewIntakeRepository(f.store),
		postgres.NewReservationRepository(f.store),
		postgres.NewQuotaProjectionRepository(f.store),
		postgres.NewFactRepository(f.store),
		executors.NewRegistry(entries),
		application.ExecutionConfig{MaxDuration: 30 * time.Second, LeaseTTL: time.Hour},
		f.admission,
	)
}

// seedBackend lays one backend row down and registers the executor the test
// scripted for it, under the row's own identity — the id the catalog hands
// the walk and the registry is keyed by. A nil executor leaves the backend
// unregistered — the eligibility-leg shape, a candidate the walk must skip.
func (f *routingFixture) seedBackend(t *testing.T, name string, executor *routingFakeExecutor) catalog.BackendID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id := string(identity.NewRequestID())
	if _, err := f.store.Querier(ctx).ExecContext(ctx, `INSERT INTO backends (id, adapter_type, endpoint, state, created_at, updated_at)
VALUES ($1, 'openai-compatible', 'https://b9c4.test.local/v1', 'active', transaction_timestamp(), transaction_timestamp())`, id); err != nil {
		t.Fatalf("seeding backend %s: %v", name, err)
	}
	backendID := catalog.BackendID(id)
	if executor != nil {
		f.executorOf[backendID] = executor
		f.rewire(t)
	}
	return backendID
}

// seedCandidate pins one position on the suite's alias to a backend.
func (f *routingFixture) seedCandidate(t *testing.T, position int, backendID catalog.BackendID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := f.store.Querier(ctx).ExecContext(ctx, `INSERT INTO model_candidates (id, alias_id, position, backend_id, provider_model, created_at)
VALUES ($1, $2, $3, $4, $5, transaction_timestamp())
ON CONFLICT (alias_id, position) DO UPDATE SET backend_id = EXCLUDED.backend_id, provider_model = EXCLUDED.provider_model`,
		string(identity.NewRequestID()), string(f.routeAliasID), position, string(backendID), "model-"+string(backendID[len(backendID)-8:])); err != nil {
		t.Fatalf("seeding candidate at position %d: %v", position, err)
	}
}

// routeAccount mints one account and funds it through the wildcard scope, so
// the request's alias is servable without the base fixture's named group
// (which names the admission suite's alias, not this one).
func (f *routingFixture) routeAccount(t *testing.T) (account, bucket string) {
	t.Helper()
	account = admissionAccount(t)
	bucket = "b9c4-bucket-" + account[len(account)-8:]
	f.seedGrant(t, context.Background(), account, bucket, false, time.Time{}, 10_000)
	return account, bucket
}

// routeServe admits and routes one arrival: the full pipeline over real
// storage, answering with the outcome and leaving the residue to the reads.
func (f *routingFixture) routeServe(t *testing.T, account, key, content string) (application.ChatOutcome, *routingReply) {
	t.Helper()
	body := admissionBody(t, routingFixtureAliasName, 64, content)
	in := admissionInput(identity.NewRequestID(), account, admissionKeyID(t), key, admissionServeState("active"), body)
	reply := &routingReply{}
	in.Reply = reply
	outcome, err := f.routing.Serve(context.Background(), in)
	if err != nil {
		t.Fatalf("routing.Serve(%s) error = %v — a decision reaches the caller with a nil error, always", in.RequestID, err)
	}
	return outcome, reply
}

// The integration reply: what the transport builds, minus the wire. It
// records which endings the walk reached and keeps the content bytes, so a
// settlement's basis can be asserted against what was actually delivered.
type routingReply struct {
	opened      bool
	stream      bool
	committed   bool
	buf         []byte
	served      int
	midStream   int
	surfaced    []execution.FailureReason
	noCandidate int
}

func (r *routingReply) Open(stream bool) { r.opened, r.stream = true, stream }

func (r *routingReply) Content(chunk []byte) error {
	r.committed = true
	r.buf = append(r.buf, chunk...)
	return nil
}

func (r *routingReply) Committed() bool   { return r.committed }
func (r *routingReply) Delivered() []byte { return r.buf }
func (r *routingReply) ServeSucceeded()   { r.served++ }

func (r *routingReply) ServeMidStreamFailure() { r.midStream++ }

func (r *routingReply) ServeSurfaced(failure execution.FailureReason) {
	r.surfaced = append(r.surfaced, failure)
}

func (r *routingReply) ServeNoCandidate() { r.noCandidate++ }

// routingFakeExecutor is the scripted executor: results consumed in order,
// the last repeating, chunks written into the sink before the answer. The
// script is the test's only policy — the executor makes no routing decision
// of its own, exactly as the port demands.
type routingFakeExecutor struct {
	mu      sync.Mutex
	results []executors.Result
	chunks  []string
	calls   []executors.AttemptSpec
}

func (e *routingFakeExecutor) Execute(_ context.Context, spec executors.AttemptSpec, sink executors.Sink) executors.Result {
	e.mu.Lock()
	e.calls = append(e.calls, spec)
	var result executors.Result
	if len(e.results) == 0 {
		result = executors.Failure{Class: execution.ErrorInvalidUpstreamResponse}
	} else {
		result = e.results[0]
		if len(e.results) > 1 {
			e.results = e.results[1:]
		}
	}
	chunks := e.chunks
	e.mu.Unlock()

	for _, chunk := range chunks {
		if err := sink.Content([]byte(chunk)); err != nil {
			return executors.Failure{Class: execution.ErrorStreamAfterCommitment}
		}
	}
	return result
}

// The residue reads the routing endings add: one request's row, its
// attempts, and the fact it ended under.

func (f *routingFixture) requestRow(t *testing.T, requestID string) (status, failure, rejection, committedAttempt sql.NullString) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.db.QueryRowContext(ctx,
		`SELECT status, failure_reason, rejection_reason, committed_attempt_id FROM public.requests WHERE id = $1`,
		requestID).Scan(&status, &failure, &rejection, &committedAttempt); err != nil {
		t.Fatalf("reading the request row of %s: %v", requestID, err)
	}
	return status, failure, rejection, committedAttempt
}

type attemptRow struct {
	position   int
	outcome    string
	errorClass sql.NullString
	backend    string
}

func (f *routingFixture) attemptsOf(t *testing.T, requestID string) []attemptRow {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := f.db.QueryContext(ctx,
		`SELECT candidate_position, outcome, error_class, backend_id FROM public.request_attempts
		 WHERE request_id = $1 ORDER BY candidate_position, retry_sequence`, requestID)
	if err != nil {
		t.Fatalf("reading the attempts of %s: %v", requestID, err)
	}
	defer rows.Close()
	var found []attemptRow
	for rows.Next() {
		var row attemptRow
		if err := rows.Scan(&row.position, &row.outcome, &row.errorClass, &row.backend); err != nil {
			t.Fatalf("scanning an attempt row of %s: %v", requestID, err)
		}
		found = append(found, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("walking the attempts of %s: %v", requestID, err)
	}
	return found
}

func (f *routingFixture) latestFact(t *testing.T, requestID string) (kind, capture sql.NullString, providerInput, providerOutput, delivery, amount sql.NullInt64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.db.QueryRowContext(ctx,
		`SELECT kind, capture_method, provider_input_tokens, provider_output_tokens, delivery_tokens, settled_amount
		 FROM public.usage_events WHERE request_id = $1 ORDER BY append_seq DESC LIMIT 1`, requestID).
		Scan(&kind, &capture, &providerInput, &providerOutput, &delivery, &amount); err != nil {
		t.Fatalf("reading the fact of %s: %v", requestID, err)
	}
	return kind, capture, providerInput, providerOutput, delivery, amount
}

// TestIntegrationRoutingServesThroughARegistryBuiltFromTheCatalog is the
// wiring test the fake-executor scenarios cannot be: the registry here is
// not a test's map but the composition root's own build — the rows read
// back from the real catalog through the same List the refresh loop makes,
// turned into executors by the same buildExecutors bind() calls — and the
// executor at the end of it is the real openai-compatible one, calling a
// real (httptest) provider over the wire. It proves the chain a serving
// request actually travels: row → resolved target → one translated POST →
// the verbatim answer delivered and committed → settled on the provider's
// own report.
func TestIntegrationRoutingServesThroughARegistryBuiltFromTheCatalog(t *testing.T) {
	f := newRoutingFixture(t)
	account, _ := f.routeAccount(t)

	// The provider: one non-streaming completion, whose answer carries the
	// usage report the settlement reads. It records what it was called
	// with, so the test can hold the translation and the credential
	// closure to their contracts.
	var calls int32
	var sawAuth, sawModel, sawStream atomic.Value
	providerAnswer := []byte(`{"id":"chatcmpl-b10-1","object":"chat.completion","created":1,` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"a settled answer"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`)
	provider := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		atomic.AddInt32(&calls, 1)
		if r.URL.Path != "/chat/completions" {
			t.Errorf("the provider was called at %s, want the path the builder appends", r.URL.Path)
		}
		sawAuth.Store(r.Header.Get("Authorization"))
		var translated map[string]any
		if err := json.NewDecoder(r.Body).Decode(&translated); err != nil {
			t.Errorf("decoding the translated request: %v", err)
			w.WriteHeader(stdhttp.StatusBadRequest)
			return
		}
		sawModel.Store(translated["model"])
		// The raw value is stored, not a coerced one: an absent key decodes
		// as untyped nil, and the assertion below reads the difference
		// between "false" and "not stated" — the stream switch is one of the
		// two fields the gateway owns on the wire, so it must be stated.
		sawStream.Store(translated["stream"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(providerAnswer)
	}))
	defer provider.Close()

	// The catalog rows the registry will be built from: one openai-compatible
	// backend pointing at the provider, a credential reference in the one
	// grammar the resolver speaks, and the candidate that names it.
	const credentialEnv = "B10_TEST_PROVIDER_CREDENTIAL"
	t.Setenv(credentialEnv, "test-only-provider-material")
	backendID := string(identity.NewRequestID())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := f.store.Querier(ctx).ExecContext(ctx, `INSERT INTO backends (id, adapter_type, endpoint, credentials_ref, state, created_at, updated_at)
VALUES ($1, 'openai-compatible', $2, $3, 'active', transaction_timestamp(), transaction_timestamp())`,
		backendID, provider.URL, "env:"+credentialEnv); err != nil {
		t.Fatalf("seeding the provider backend: %v", err)
	}
	providerModel := "model-" + backendID[len(backendID)-8:]
	f.seedCandidate(t, 1, catalog.BackendID(backendID))

	// The composition root's own build, over the rows as the refresh loop
	// reads them — not a test-scripted map.
	rows, err := postgres.NewBackends(f.store).List(ctx)
	if err != nil {
		t.Fatalf("reading the backend catalog: %v", err)
	}
	entries := buildExecutors(rows, config.Config{})
	// The catalog is shared and accumulates rows from every suite and run,
	// so the honest assertion is this test's row resolving, not the
	// snapshot's size — unresolvable rows belong to whomever seeded them.
	if _, ok := entries[catalog.BackendID(backendID)]; !ok {
		t.Fatalf("the builder left this test's backend out of the snapshot; it resolved %d callable row(s)", len(entries))
	}
	for id, executor := range entries {
		f.executorOf[id] = executor
	}
	f.rewire(t)

	outcome, reply := f.routeServe(t, account, "b9c4-key-registry", "b8c3")

	if outcome.Kind != application.OutcomeServed {
		t.Fatalf("outcome = %s/%s, want served through the built registry", outcome.Kind, outcome.Reason)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("the provider was called %d times, want exactly one — the single-shot law holds over the real wire", n)
	}
	if auth, _ := sawAuth.Load().(string); auth != "Bearer test-only-provider-material" {
		t.Errorf("the provider saw Authorization %q, want the material the environment resolved at use", auth)
	}
	if model, _ := sawModel.Load().(string); model != providerModel {
		t.Errorf("the provider was asked for model %v, want the candidate's provider model %s", model, providerModel)
	}
	if stream, ok := sawStream.Load().(bool); !ok || stream {
		t.Errorf("the translated request carried stream = %v (stated: %t), want the gateway's own switch, stated and false", sawStream.Load(), ok)
	}
	if !reply.committed || string(reply.buf) != string(providerAnswer) {
		t.Errorf("reply = committed:%t body:%q, want the provider's answer delivered verbatim and committed", reply.committed, reply.buf)
	}

	requestID := string(outcome.RuntimeRequestID)
	if rows := f.attemptsOf(t, requestID); len(rows) != 1 || rows[0].outcome != "succeeded" {
		t.Errorf("attempts = %+v, want the one succeeded attempt behind the committed answer", rows)
	}
	kind, capture, providerInput, providerOutput, _, amount := f.latestFact(t, requestID)
	if kind.String != "settled" || capture.String != "reported" {
		t.Errorf("fact = %s/%s, want settled/reported — both figures inside the basis stand as given", kind.String, capture.String)
	}
	if providerInput.Int64 != 3 || providerOutput.Int64 != 5 {
		t.Errorf("provider figures = %d/%d, want the report 3/5", providerInput.Int64, providerOutput.Int64)
	}
	// ceil((3·1 + 5·2) minor units at the seeded millionth-scale prices) = 13
	// — the report priced, not the hold's 132.
	if amount.Int64 != 13 {
		t.Errorf("settled amount = %d, want the report's 13", amount.Int64)
	}
}

func TestIntegrationRoutingReleasesAnAdmittedRequestWhenNoExecutorExists(t *testing.T) {
	f := newRoutingFixture(t)
	account, bucket := f.routeAccount(t)

	outcome, reply := f.routeServe(t, account, "b9c4-key-empty-registry", "b8c3")

	if outcome.Kind != application.OutcomeRejected || outcome.Reason != execution.RejectedNoCandidate {
		t.Fatalf("outcome = %s/%s, want rejected/no_candidate", outcome.Kind, outcome.Reason)
	}
	if reply.noCandidate != 1 {
		t.Errorf("the reply was told no_candidate %d times, want exactly once", reply.noCandidate)
	}
	if reply.opened {
		t.Error("the reply was opened for an answer that was never a candidate's to give")
	}

	requestID := string(outcome.RuntimeRequestID)
	status, failure, rejection, committed := f.requestRow(t, requestID)
	if status.String != "rejected" || rejection.String != string(execution.RejectedNoCandidate) {
		t.Errorf("request row = %s/%s, want rejected/no_candidate", status.String, rejection.String)
	}
	if failure.Valid || committed.Valid {
		t.Errorf("a released request names failure %v or attempt %v; it never ran one", failure, committed)
	}
	if rows := f.attemptsOf(t, requestID); len(rows) != 0 {
		t.Errorf("a released request carries %d attempt rows, want none", len(rows))
	}
	intakeRequestID, finalStatus, _ := f.intakeOf(t, account, "b9c4-key-empty-registry")
	if intakeRequestID != requestID || finalStatus.String != "rejected" {
		t.Errorf("intake = (%s, %s), want (%s, rejected)", intakeRequestID, finalStatus.String, requestID)
	}

	// The hold went back whole: the balance is byte-identical, and the fact
	// that reports it is the only one.
	available, limit := f.balance(t, bucket)
	if available != limit {
		t.Errorf("available = %d with limit %d; a release restores the draw exactly", available, limit)
	}
	if n := f.factCount(t, requestID); n != 1 {
		t.Errorf("the release appended %d facts, want exactly one", n)
	}
	kind, _, _, _, _, _ := f.latestFact(t, requestID)
	if kind.String != "released" {
		t.Errorf("the fact's kind = %s, want released", kind.String)
	}
}

// TestIntegrationRoutingExhaustsTheWalkAndReleasesAsNoCandidateSucceeded: the
// other no-candidate ending — not an empty registry but a walk that ran every
// eligible candidate and watched each one fail with a fault another candidate
// might have cleared. The answer names the walk, not any one provider's bad
// day; the attempts stand as the observation that the walk really happened;
// and the reply is told no_candidate after having been opened — the framing
// was armed before the first candidate ran, and the ending arrived too late
// to be anything but an error body written into it.
func TestIntegrationRoutingExhaustsTheWalkAndReleasesAsNoCandidateSucceeded(t *testing.T) {
	f := newRoutingFixture(t)
	account, bucket := f.routeAccount(t)
	first := f.seedBackend(t, "first-failure", &routingFakeExecutor{
		results: []executors.Result{executors.Failure{Class: execution.ErrorRateLimited, ProviderRequestID: "prov-req-5"}},
	})
	f.seedCandidate(t, 1, first)
	second := f.seedBackend(t, "second-failure", &routingFakeExecutor{
		results: []executors.Result{executors.Failure{Class: execution.ErrorProviderUnavailable, ProviderRequestID: "prov-req-6"}},
	})
	f.seedCandidate(t, 2, second)

	outcome, reply := f.routeServe(t, account, "b9c4-key-exhaustion", "b8c3")

	if outcome.Kind != application.OutcomeRejected || outcome.Reason != execution.RejectedNoCandidateSucceeded {
		t.Fatalf("outcome = %s/%s, want rejected/no_candidate_succeeded", outcome.Kind, outcome.Reason)
	}
	if reply.noCandidate != 1 {
		t.Errorf("the reply was told no_candidate %d times, want exactly once", reply.noCandidate)
	}
	if !reply.opened || reply.committed {
		t.Errorf("reply opened=%t committed=%t, want opened but never committed — the ending is an error body, not a stream", reply.opened, reply.committed)
	}

	requestID := string(outcome.RuntimeRequestID)
	status, failure, rejection, committed := f.requestRow(t, requestID)
	if status.String != "rejected" || rejection.String != string(execution.RejectedNoCandidateSucceeded) {
		t.Errorf("request row = %s/%s, want rejected/no_candidate_succeeded", status.String, rejection.String)
	}
	if failure.Valid || committed.Valid {
		t.Errorf("an exhausted walk names failure %v or attempt %v; it committed neither", failure, committed)
	}
	rows := f.attemptsOf(t, requestID)
	if len(rows) != 2 {
		t.Fatalf("the walk recorded %d attempts, want one per candidate it really tried", len(rows))
	}
	if rows[0].position != 0 || rows[0].errorClass.String != "rate_limited" {
		t.Errorf("attempt 1 = %+v, want position 0/rate_limited", rows[0])
	}
	if rows[1].position != 1 || rows[1].errorClass.String != "provider_unavailable" {
		t.Errorf("attempt 2 = %+v, want position 1/provider_unavailable", rows[1])
	}
	intakeRequestID, finalStatus, _ := f.intakeOf(t, account, "b9c4-key-exhaustion")
	if intakeRequestID != requestID || finalStatus.String != "rejected" {
		t.Errorf("intake = (%s, %s), want (%s, rejected)", intakeRequestID, finalStatus.String, requestID)
	}

	available, limit := f.balance(t, bucket)
	if available != limit {
		t.Errorf("available = %d with limit %d; an exhausted walk releases the hold whole", available, limit)
	}
	kind, _, _, _, _, _ := f.latestFact(t, requestID)
	if kind.String != "released" {
		t.Errorf("the fact's kind = %s, want released", kind.String)
	}
}

func TestIntegrationRoutingFallsThroughToTheSecondCandidateAndSettles(t *testing.T) {
	f := newRoutingFixture(t)
	account, bucket := f.routeAccount(t)
	primary := f.seedBackend(t, "primary", &routingFakeExecutor{
		results: []executors.Result{executors.Failure{Class: execution.ErrorRateLimited, ProviderRequestID: "prov-req-1"}},
	})
	f.seedCandidate(t, 1, primary)
	secondary := f.seedBackend(t, "secondary", &routingFakeExecutor{
		chunks:  []string{`{"answer":true}`},
		results: []executors.Result{executors.Success{Usage: executors.Usage{InputTokens: ptrInt64(101), OutputTokens: ptrInt64(55)}, ProviderRequestID: "prov-req-2"}},
	})
	f.seedCandidate(t, 2, secondary)

	outcome, reply := f.routeServe(t, account, "b9c4-key-fallthrough", "b8c3")

	if outcome.Kind != application.OutcomeServed {
		t.Fatalf("outcome = %s/%s, want served", outcome.Kind, outcome.Reason)
	}
	if reply.served != 1 || !reply.committed || string(reply.buf) != `{"answer":true}` {
		t.Errorf("reply = served:%d committed:%t body:%q, want one served committed answer", reply.served, reply.committed, reply.buf)
	}

	requestID := string(outcome.RuntimeRequestID)
	rows := f.attemptsOf(t, requestID)
	if len(rows) != 2 {
		t.Fatalf("the walk recorded %d attempts, want one per candidate", len(rows))
	}
	if rows[0].position != 0 || rows[0].outcome != "failed_before_commitment" || rows[0].errorClass.String != "rate_limited" {
		t.Errorf("attempt 1 = %+v, want position 0 failed_before_commitment/rate_limited", rows[0])
	}
	if rows[1].position != 1 || rows[1].outcome != "succeeded" || rows[1].errorClass.Valid {
		t.Errorf("attempt 2 = %+v, want position 1 succeeded with no class", rows[1])
	}
	status, failure, rejection, _ := f.requestRow(t, requestID)
	if status.String != "succeeded" || failure.Valid || rejection.Valid {
		t.Errorf("request row = %s/%s/%s, want succeeded with neither reason", status.String, failure.String, rejection.String)
	}
	// The committed attempt pointer names a succeeded attempt OF THIS
	// REQUEST and no other — the composite FK's guarantee, read back joined.
	var named int
	if err := f.db.QueryRowContext(mustCtx(t),
		`SELECT count(*) FROM public.requests rq JOIN public.request_attempts a ON (a.request_id, a.id) = (rq.id, rq.committed_attempt_id)
		 WHERE rq.id = $1 AND a.outcome = 'succeeded'`, requestID).Scan(&named); err != nil || named != 1 {
		t.Errorf("the committed attempt pointer names %d succeeded attempts of this request, want exactly 1 (err=%v)", named, err)
	}

	kind, capture, providerInput, providerOutput, delivery, amount := f.latestFact(t, requestID)
	// The report's input (101) ran past the count the hold was priced from —
	// the whole body's byte length under the canonical tokenizer, the
	// envelope included — so the settlement clamps it to that basis and
	// labels the fact reservation_floor. The report's output (55) sat inside
	// the output basis (routeServe's max_tokens of 64), so the report stands
	// as given.
	if kind.String != "settled" || capture.String != "reservation_floor" {
		t.Errorf("fact = %s/%s, want settled/reservation_floor", kind.String, capture.String)
	}
	if providerInput.Int64 != int64(len(admissionBody(t, routingFixtureAliasName, 64, "b8c3"))) {
		t.Errorf("provider input = %d, want the basis the hold was priced from — the clamp, not the report", providerInput.Int64)
	}
	if providerOutput.Int64 != 55 {
		t.Errorf("provider output = %d, want the report's 55, which sat inside the basis", providerOutput.Int64)
	}
	if delivery.Int64 != int64(len(`{"answer":true}`)) {
		t.Errorf("delivery = %d, want the gateway's own count of what was delivered", delivery.Int64)
	}
	if amount.Int64 <= 0 {
		t.Errorf("settled amount = %d, want the priced product of the report", amount.Int64)
	}

	// Settlement returns nothing to the projection: the hold stays drawn
	// until the Control Plane's settlement publishes — the released-only
	// return is what keeps capacity from being minted.
	available, limit := f.balance(t, bucket)
	if available >= limit {
		t.Errorf("available = %d with limit %d; a settled hold stays drawn", available, limit)
	}
}

func TestIntegrationRoutingSurfacesARefusalAndReplaysIt(t *testing.T) {
	f := newRoutingFixture(t)
	account, bucket := f.routeAccount(t)
	only := f.seedBackend(t, "refusing", &routingFakeExecutor{
		results: []executors.Result{executors.Failure{Class: execution.ErrorContextTooLarge, ProviderRequestID: "prov-req-3"}},
	})
	f.seedCandidate(t, 1, only)

	outcome, reply := f.routeServe(t, account, "b9c4-key-refusal", "b8c3")

	if outcome.Kind != application.OutcomeRefused || outcome.Failure != execution.FailedContextTooLarge {
		t.Fatalf("outcome = %s/%s, want refused/context_too_large", outcome.Kind, outcome.Failure)
	}
	if len(reply.surfaced) != 1 || reply.surfaced[0] != execution.FailedContextTooLarge {
		t.Errorf("the reply was told %v, want one context_too_large refusal", reply.surfaced)
	}
	if reply.committed {
		t.Error("a surfaced refusal is pre-commitment; the reply must not have crossed the gate")
	}

	requestID := string(outcome.RuntimeRequestID)
	status, failure, rejection, committed := f.requestRow(t, requestID)
	if status.String != "failed" || failure.String != string(execution.FailedContextTooLarge) || rejection.Valid {
		t.Errorf("request row = %s/%s/%s, want failed/context_too_large", status.String, failure.String, rejection.String)
	}
	if committed.Valid {
		t.Errorf("a pre-commitment failure names attempt %v; none was committed", committed)
	}
	if rows := f.attemptsOf(t, requestID); len(rows) != 1 || rows[0].errorClass.String != "context_too_large" {
		t.Errorf("attempts = %+v, want the one classified attempt", rows)
	}
	available, limit := f.balance(t, bucket)
	if available != limit {
		t.Errorf("available = %d with limit %d; a surfaced refusal releases the hold whole", available, limit)
	}

	// The replay: the same key and body re-answers from the record — the
	// refusal again, under the original's identity, with no second walk.
	body := admissionBody(t, routingFixtureAliasName, 64, "b8c3")
	replayIn := admissionInput(identity.NewRequestID(), account, admissionKeyID(t), "b9c4-key-refusal", admissionServeState("active"), body)
	replayReply := &routingReply{}
	replayIn.Reply = replayReply
	replay, err := f.routing.Serve(context.Background(), replayIn)
	if err != nil {
		t.Fatalf("replay Serve error = %v", err)
	}
	if replay.Kind != application.OutcomeReplay || replay.Failure != execution.FailedContextTooLarge {
		t.Fatalf("replay = %s/%s, want replay/context_too_large", replay.Kind, replay.Failure)
	}
	if replay.Original != outcome.RuntimeRequestID {
		t.Errorf("replay original = %s, want the first arrival %s", replay.Original, outcome.RuntimeRequestID)
	}
	if n := f.factCount(t, requestID); n != 1 {
		t.Errorf("the replay left %d facts behind, want the original's one", n)
	}
}

func TestIntegrationRoutingSettlesAMidStreamFailure(t *testing.T) {
	f := newRoutingFixture(t)
	account, _ := f.routeAccount(t)
	only := f.seedBackend(t, "dying", &routingFakeExecutor{
		chunks: []string{"data: partial"},
		results: []executors.Result{executors.Failure{
			Class:             execution.ErrorProviderUnavailable,
			ProviderRequestID: "prov-req-4",
		}},
	})
	f.seedCandidate(t, 1, only)

	outcome, reply := f.routeServe(t, account, "b9c4-key-midstream", "b8c3")

	if outcome.Kind != application.OutcomeServed {
		t.Fatalf("outcome = %s/%s, want served — the failure frame already travelled", outcome.Kind, outcome.Reason)
	}
	if reply.midStream != 1 {
		t.Errorf("the reply was told mid_stream_failure %d times, want once", reply.midStream)
	}

	requestID := string(outcome.RuntimeRequestID)
	rows := f.attemptsOf(t, requestID)
	if len(rows) != 1 || rows[0].outcome != "failed_after_commitment" || rows[0].errorClass.String != "stream_failed_after_commitment" {
		t.Fatalf("attempts = %+v, want the one post-commitment attempt, classed by the gate", rows)
	}
	status, failure, _, committed := f.requestRow(t, requestID)
	if status.String != "failed" || failure.String != string(execution.FailedStreamAfterCommitment) {
		t.Errorf("request row = %s/%s, want failed/stream_failed_after_commitment", status.String, failure.String)
	}
	if !committed.Valid {
		t.Fatal("a post-commitment failure must name the attempt that was committed")
	}

	kind, capture, providerInput, providerOutput, delivery, amount := f.latestFact(t, requestID)
	// The dying provider reported nothing, so the settlement prices what the
	// delivery boundary can stand behind: input falls back to the count the
	// hold was priced from — the whole body's byte length, because the
	// provider read the whole prompt either way — and output, on a stream
	// that died after commitment, is the delivered tokens and nothing else;
	// the provider's figure for work the client never read is attempt
	// telemetry, never a bill. Both settled figures are the gateway's own
	// counts and the reservation defends neither, so the label is
	// gateway_observed. Delivery is that same delivered count, priced by
	// nothing.
	if kind.String != "settled" || capture.String != "gateway_observed" {
		t.Errorf("fact = %s/%s, want settled/gateway_observed", kind.String, capture.String)
	}
	if !providerInput.Valid || providerInput.Int64 != int64(len(admissionBody(t, routingFixtureAliasName, 64, "b8c3"))) { // the admission count
		t.Errorf("provider input = %v, want the admission count the settlement fell back to", providerInput)
	}
	if providerOutput.Int64 != int64(len("data: partial")) {
		t.Errorf("provider output = %d, want the delivered tokens the delivery boundary bills — the hold's output basis bounds them, never replaces them", providerOutput.Int64)
	}
	if delivery.Int64 != int64(len("data: partial")) {
		t.Errorf("delivery = %d, want what the client received", delivery.Int64)
	}
	if amount.Int64 <= 0 {
		t.Errorf("settled amount = %d, want the delivered answer priced", amount.Int64)
	}
}

func ptrInt64(v int64) *int64 { return &v }

func mustCtx(t testing.TB) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}
