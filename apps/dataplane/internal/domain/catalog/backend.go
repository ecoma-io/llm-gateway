package catalog

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// BackendState is a backend's position on its lifecycle: active or disabled,
// and deliberately reversible. Disabling a backend is an operational act —
// drain a provider, wait out an incident — and its undo is the same act in
// the other direction. Nothing about a backend is terminal: the row that
// candidates reference by id stays meaningful for as long as the catalog
// names it.
type BackendState string

const (
	// BackendActive: candidate selection may route to this backend.
	BackendActive BackendState = "active"
	// BackendDisabled: the backend is skipped — selection treats it as if
	// it were not there. Its row and its references stay intact.
	BackendDisabled BackendState = "disabled"
)

// maxAdapterTypeLen and maxRefLen bound the operator-supplied strings. The
// adapter type is a lowercase-kebab token (openai-compatible, anthropic, a
// future gcp-vertex …); the references are opaque pointers whose length is
// bounded because some string has to be, not because their contents mean
// anything here.
const (
	maxAdapterTypeLen = 64
	maxEndpointLen    = 2048
	maxRefLen         = 512
)

// Backend is one configured instance of an adapter (ADR 0002): an endpoint,
// the adapter type that picks the driver, and two opaque references the
// provider phase resolves when calls go out. It is its own aggregate (ADR
// 0001, rule 3) — candidates reference it by id, never the other way round,
// and its lifecycle is independent of every alias that uses it.
//
// Two fields are worth explaining by what they are NOT. adapterType is
// immutable after construction — there is no setter, because the adapter
// type is what decides which driver reads the endpoint, and swapping it
// under a surviving id would rewrite what every historical attempt row means
// by "this backend". credentialsRef and egressPolicyRef are pointers this
// domain never resolves: where provider credentials actually live is the
// provider phase's decision (recorded in issue #49), and until then the
// catalog's whole obligation is to carry the reference intact — a
// credentials_ref NULL is legitimate, because some adapters need no
// credential at all.
type Backend struct {
	ID              BackendID
	AdapterType     string
	Endpoint        string
	CredentialsRef  string
	EgressPolicyRef string
	State           BackendState
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// NewBackend returns a backend in its birth state, active. Validation rules:
// the adapter type is non-blank lowercase kebab, the endpoint is an http(s)
// URL of bounded length, and each reference, when present, is non-blank and
// bounded. Blank credentials and egress references are the zero string, not
// a sentinel: their absence is data, not an error.
func NewBackend(id BackendID, adapterType, endpoint, credentialsRef, egressPolicyRef string, now time.Time) (*Backend, error) {
	adapterType = trimSpace(adapterType)
	endpoint = trimSpace(endpoint)
	credentialsRef = trimSpace(credentialsRef)
	egressPolicyRef = trimSpace(egressPolicyRef)
	if err := validateAdapterType(adapterType); err != nil {
		return nil, err
	}
	if err := validateEndpoint(endpoint); err != nil {
		return nil, err
	}
	if err := validateRef(credentialsRef, "credentials"); err != nil {
		return nil, err
	}
	if err := validateRef(egressPolicyRef, "egress policy"); err != nil {
		return nil, err
	}
	if id == "" {
		// A blank id is a programming error upstream (NewBackendID cannot
		// produce one), not a domain rule violation — no sentinel.
		return nil, fmt.Errorf("catalog: new backend: blank id")
	}
	now = now.UTC()
	return &Backend{
		ID:              id,
		AdapterType:     adapterType,
		Endpoint:        endpoint,
		CredentialsRef:  credentialsRef,
		EgressPolicyRef: egressPolicyRef,
		State:           BackendActive,
		CreatedAt:       now,
		UpdatedAt:       now,
	}, nil
}

// Disable skips the backend in candidate selection. Disabling an already-
// disabled backend is a no-op — the operator's intent is already recorded.
func (b *Backend) Disable(now time.Time) error {
	switch b.State {
	case BackendDisabled:
		return nil
	case BackendActive:
		b.State = BackendDisabled
		b.UpdatedAt = now.UTC()
		return nil
	default:
		return fmt.Errorf("catalog: disable backend %s: %w: unknown state %q", b.ID, ErrInvalidTransition, b.State)
	}
}

// Enable returns a disabled backend to selection. Enabling an active backend
// is a no-op. This is the lifecycle's only way back — and it exists because
// the machine has no terminal state to protect.
func (b *Backend) Enable(now time.Time) error {
	switch b.State {
	case BackendActive:
		return nil
	case BackendDisabled:
		b.State = BackendActive
		b.UpdatedAt = now.UTC()
		return nil
	default:
		return fmt.Errorf("catalog: enable backend %s: %w: unknown state %q", b.ID, ErrInvalidTransition, b.State)
	}
}

// UpdateTarget re-points the backend at a new endpoint, or new credential and
// egress references. The adapter type is not a parameter on purpose — it is
// the backend's identity as a driver selection, and its immutability is
// enforced by this signature before any persistence could disagree. Target
// edits are legal in both states: a backend may be re-pointed while drained,
// and enabling it afterwards is the operator's separate act.
func (b *Backend) UpdateTarget(endpoint, credentialsRef, egressPolicyRef string, now time.Time) error {
	endpoint = trimSpace(endpoint)
	credentialsRef = trimSpace(credentialsRef)
	egressPolicyRef = trimSpace(egressPolicyRef)
	if err := validateEndpoint(endpoint); err != nil {
		return err
	}
	if err := validateRef(credentialsRef, "credentials"); err != nil {
		return err
	}
	if err := validateRef(egressPolicyRef, "egress policy"); err != nil {
		return err
	}
	b.Endpoint = endpoint
	b.CredentialsRef = credentialsRef
	b.EgressPolicyRef = egressPolicyRef
	b.UpdatedAt = now.UTC()
	return nil
}

// validateAdapterType enforces the one grammar the adapter type carries: a
// non-blank lowercase kebab token. The set is deliberately open — a new
// provider is a row, not a migration — so the check is shape, never
// membership.
func validateAdapterType(adapterType string) error {
	if adapterType == "" {
		return fmt.Errorf("catalog: new backend: %w: adapter type is blank", ErrInvalidBackendTarget)
	}
	for i := 0; i < len(adapterType); i++ {
		c := adapterType[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return fmt.Errorf("catalog: new backend: %w: adapter type %q is not a lowercase kebab token", ErrInvalidBackendTarget, adapterType)
		}
	}
	if adapterType[0] == '-' || adapterType[len(adapterType)-1] == '-' ||
		strings.Contains(adapterType, "--") || len(adapterType) > maxAdapterTypeLen {
		// The three refusals spell the schema's regex
		// ^[a-z0-9]+(-[a-z0-9]+)*$ in prose: no edge dashes, no empty
		// segment. The domain refuses first so the check constraint is a
		// second opinion and never the first report a caller sees.
		return fmt.Errorf("catalog: new backend: %w: adapter type %q is not a lowercase kebab token", ErrInvalidBackendTarget, adapterType)
	}
	return nil
}

// validateEndpoint enforces the endpoint's shape: an http or https URL with
// something after the scheme. The bar is deliberately low — the gateway does
// not resolve the host here — because a URL that parses as text but points
// nowhere fails at call time, where the retry and fallback machinery lives,
// which is the right place for it to fail.
func validateEndpoint(endpoint string) error {
	const minLen = len("https://") + 1
	if len(endpoint) < minLen || len(endpoint) > maxEndpointLen {
		return fmt.Errorf("catalog: new backend: %w: endpoint length %d is outside %d..%d", ErrInvalidBackendTarget, len(endpoint), minLen, maxEndpointLen)
	}
	if !hasPrefix(endpoint, "http://") && !hasPrefix(endpoint, "https://") {
		return fmt.Errorf("catalog: new backend: %w: endpoint is not an http(s) URL", ErrInvalidBackendTarget)
	}
	return nil
}

// validateRef enforces the shape of an optional reference: bounded. The
// aggregates trim before this runs, so absence arrives as the zero string —
// absence is data — and a present reference's only further obligation is
// its length.
func validateRef(ref, what string) error {
	if ref == "" {
		return nil
	}
	if n := utf8.RuneCountInString(ref); n > maxRefLen {
		return fmt.Errorf("catalog: new backend: %w: %s reference exceeds %d runes", ErrInvalidBackendTarget, what, maxRefLen)
	}
	return nil
}

// hasPrefix is strings.HasPrefix spelled locally so the endpoint grammar
// reads in one place.
func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
