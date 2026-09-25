package catalog

import "errors"

// The package's distinguishable outcomes. Callers branch on these with
// errors.Is; everything else arrives wrapped with context. The texts are
// echoed to consoles, logs and, eventually, management responses — none of
// them carries anything secret, because nothing in the catalog is secret.

var (
	// ErrInvalidAliasName reports an alias name that is blank, too long, or
	// outside the grammar — an alnum head followed by alnum, dot, underscore,
	// slash or dash. The name is what clients send, so the grammar is the
	// request surface's vocabulary, not an internal nicety.
	ErrInvalidAliasName = errors.New("catalog: invalid alias name")

	// ErrInvalidBounds reports an alias bound — the output token limit or
	// the reservation cap — that is zero or negative. Both bound real
	// admission arithmetic downstream; a non-positive bound would either
	// reject every request or cap nothing, so neither is a state the
	// catalog may store.
	ErrInvalidBounds = errors.New("catalog: alias bounds must be positive")

	// ErrInvalidCandidates reports a candidate list whose shape is wrong:
	// empty, a candidate with a blank provider model or unknown backend
	// reference, or the same (backend, provider model) target listed twice.
	// The domain owns the list's shape — positions are assigned 1..n in
	// slice order — so every ill-formed list dies here rather than in a
	// constraint the schema would report one row at a time.
	ErrInvalidCandidates = errors.New("catalog: invalid candidate list")

	// ErrInvalidParameterOverrides reports overrides that are neither
	// absent nor a JSON object. They are passed through to the provider at
	// execution, so the one shape the gateway owes the pipeline is "this is
	// a JSON object"; anything deeper is the provider's business.
	ErrInvalidParameterOverrides = errors.New("catalog: parameter overrides must be a JSON object")

	// ErrInvalidTransition reports a lifecycle move the state machines
	// forbid: leaving a retired alias (retirement is one-way), or a move
	// the backend machine does not carry.
	ErrInvalidTransition = errors.New("catalog: invalid lifecycle transition")

	// ErrAliasRetired reports a configuration write against a retired
	// alias. Retirement freezes the aggregate: the row stays as history
	// exactly as its clients used it, and candidates or bounds never
	// change again.
	ErrAliasRetired = errors.New("catalog: alias is retired")

	// ErrAliasNameTaken reports a define-alias whose name collides with an
	// existing alias — active or retired. Names are never reused (ADR
	// 0003's ledgers and entitlement scopes must keep resolving to the
	// identity they named), so the collision is total and permanent; the
	// schema's total unique index is the final guard and this sentinel is
	// its application-facing form.
	ErrAliasNameTaken = errors.New("catalog: alias name is already taken")

	// ErrInvalidBackendTarget reports a backend whose configuration is not
	// usable: an adapter type outside the lowercase-kebab grammar, an
	// endpoint that is not an http(s) URL, or a reference that is present
	// but empty or oversized. The adapter type is validated at construction
	// and never again — it is immutable, because changing it would change
	// what driver reads the endpoint under the same id.
	ErrInvalidBackendTarget = errors.New("catalog: invalid backend target")

	// ErrInvalidGroupName reports a group name that is neither the reserved
	// wildcard nor inside the alias-name grammar.
	ErrInvalidGroupName = errors.New("catalog: invalid group name")

	// ErrInvalidGroupMembers reports a named group version whose member set
	// is empty or carries duplicates. An empty snapshot is meaningless —
	// the wildcard is the one group whose membership is "everything", and
	// it is not built through this path — and duplicates would make one
	// alias's containment ambiguous.
	ErrInvalidGroupMembers = errors.New("catalog: invalid group members")

	// ErrGroupVersionExists reports an open-version whose (group, version)
	// pair already exists. Inside a version race the use case treats this
	// sentinel as its signal to re-read and retry, because no caller ever
	// supplies the version number; reaching a caller means contention
	// outlasted the retry budget.
	ErrGroupVersionExists = errors.New("catalog: group version already exists")
)
