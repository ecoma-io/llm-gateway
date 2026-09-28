package http

import (
	"encoding/json"
	"time"
)

// The ten read operations' wire shapes, mirroring api/openapi/shared/console.yaml
// field for field. They are plain strings, plain integers and plain booleans
// because the import rule in internal/arch/imports_test.go forbids this package
// from reaching internal/domain: an inbound surface that imported the domain
// would be handed aggregates to render rather than writing down the shape the
// contract promises. The application returns plain fields across the seam for
// the same reason, and the ten DTOs below are where those fields get the field
// names a client reads.
//
// Three rules run through every one of them.
//
//   - The account is never an input. It is the session's, resolveSession
//     resolves it, and it crosses the seam as a plain id on the request. There
//     is no account field in any of these requests a caller could set, and the
//     one path parameter on the whole surface is a funding bucket — which
//     reaches the application beside the account, and is authorized against it
//     inside the same statement, so a bucket the account does not own is a row
//     the query never returned rather than a 403 a filter turned into one.
//
//   - A page carries exactly three facts. `items`, `next_cursor` and `has_more`,
//     and there is deliberately no fourth: no total, no page count, no
//     X-Total-Count header. The PageEnvelope schema says why at length, and
//     every page type here is the same three fields with a different `items`.
//
//   - Money is int64 minor units and there is no currency anywhere in this
//     file. Not on a balance, not on a delta, not on a price. The Money schema
//     says the currency of an account's buckets is a decision the contract has
//     not made (#63), and a field that asserted one would be a claim this
//     system cannot make.

// page is the PageEnvelope every list on this surface answers with, generic
// over its row type so the ten pages are one shape rather than ten spellings of
// it.
//
// The rows are `any` rather than a type parameter because the transport's rows
// are its own DTOs, one per contract schema, and there is nothing for a shared
// type parameter over them to constrain. The generic form is kept anyway
// because it makes the two facts the contract fixes — the three field names and
// their order — one declaration that ten constructions cannot drift from.
type page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor"`
	HasMore    bool   `json:"has_more"`
}

// newPage renders a page from what the application resolved, converting each
// row on the way out. items is guaranteed non-nil by the use case, and the nil
// check here is the belt to that braces: the contract types `items` as an
// array, and `null` is not an array.
func newPage[T any, S any](items []S, hasMore bool, nextCursor string, render func(S) T) page[T] {
	rows := make([]T, 0, len(items))
	for _, item := range items {
		rows = append(rows, render(item))
	}
	return page[T]{Items: rows, NextCursor: nextCursor, HasMore: hasMore}
}

// wireTime renders an instant as the contract's `date-time`, RFC 3339 in UTC.
//
// One function rather than a MarshalJSON on each DTO's time fields, because a
// timestamp is the one wire shape every one of these schemas shares, and
// twenty-four hand-written struct tags would be twenty-four places for a
// format to differ. It is applied at the conversion from the application's
// plain string, so a DTO never holds a time.Time at all — see
// mustWireTime for the assertion that keeps that true.
func wireTime(at string) string { return at }

// mustWireTime is the assertion behind wireTime, and it exists because that
// function's identity is the whole of the claim: the value it returns is
// whatever the application sent, and a caller reading the type would have no
// way to tell whether a conversion validated that. mustWireTime parses with the
// exact layout wireTime's callers promise, so the seam either renders a value
// that is a date-time or panics at the boundary rather than writing a field
// the contract types as one.
func mustWireTime(at string) string {
	if _, err := time.Parse(time.RFC3339, at); err != nil {
		panic("console-api http: a read DTO carried a timestamp that is not RFC 3339: " + at)
	}
	return wireTime(at)
}

// wireInstants renders a present instant and an absent one. The contract types
// these fields as `[string, "null"]` throughout — a scheduled cancellation, a
// closed bucket, a settled run — so absent is a value on the wire and never an
// empty string a client has to guess about.
func wireInstants(at *string) *string { return at }

// Money is the contract's Money schema: an int64 of minor units, in an object,
// with no currency field. The object rather than a bare number because the
// schema is one, and because a bare integer on a balance is a number a client
// will eventually format as money — which asserts a currency this system has
// not decided. See the top of this file.
type money struct {
	MinorUnits int64 `json:"minor_units"`
}

// balances is the contract's Balances schema: a bucket's three cached
// projections, rendered as three separate figures and never derived from one
// another. A handler computes nothing here — not `available` as settled minus
// held, not a sum of a page's deltas — because the schema says the legs win
// over this cache and a browser's arithmetic has no authority behind it.
type balances struct {
	Settled   money `json:"settled"`
	Held      money `json:"held"`
	Available money `json:"available"`
}

// priceSnapshot is the consume leg's price provenance, copied by value from
// the revision the leg was priced against. The revision id is a textual
// reference, not a foreign key: a settlement must be derivable from the fact
// alone, and a price re-derived from a moving table is not provenance.
type priceSnapshot struct {
	RevisionID      string `json:"revision_id"`
	InputUnitPrice  money  `json:"input_unit_price"`
	OutputUnitPrice money  `json:"output_unit_price"`
}

// accountRecord mirrors the contract's Account. `state` is the plain string the
// schema's enum spells, never a status number and never a value derived from
// the account's own subscriptions.
type accountRecord struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	State     string `json:"state"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// userRecord mirrors the contract's User. The state is the schema's own three
// — invited, active, removed — and an omitted one is not an active one: this
// DTO has no default, so a row that did not render a state renders nothing and
// the client sees a field it must treat as missing rather than one it may
// default in the server's place.
type userRecord struct {
	ID        string `json:"id"`
	AccountID string `json:"account_id"`
	Email     string `json:"email"`
	State     string `json:"state"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// apiKeyRecord mirrors the contract's APIKey, the ownership record every read
// but the mint returns. The credential is absent, not null: this plane stores
// no plaintext and no digest, and the record the mint returns is the only
// place a credential exists.
type apiKeyRecord struct {
	ID          string      `json:"id"`
	AccountID   string      `json:"account_id"`
	CreatedBy   string      `json:"created_by,omitempty"`
	DisplayName string      `json:"display_name"`
	Prefix      string      `json:"prefix"`
	State       apiKeyState `json:"state"`
	CreatedAt   string      `json:"created_at"`
	UpdatedAt   string      `json:"updated_at,omitempty"`
	RevokedAt   string      `json:"revoked_at,omitempty"`
}

// planRecord mirrors the contract's Plan.
//
// The contract's own description of the plan catalogue says "Every plan the
// Control Plane offers, with its versions", and its Plan schema names no
// versions array. The two do not agree, and the resolution is the one that
// cannot be wrong: this DTO renders exactly the fields the SCHEMA declares, so
// a plan arrives as a plan. The versions are not invented here and are not
// dropped from the schema either — the surface ships the catalogue, and if
// versions are to be rendered on it that is a change to the contract first
// (the document changes before the code does, never after).
type planRecord struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// subscriptionRecord mirrors the contract's Subscription. The state and the
// cancel_at are separate here for the reason the schema is explicit about: a
// scheduled cancellation is DATA, NOT A STATE — the subscription stays active
// and usable until that instant passes — so a row carrying a future cancel_at
// renders as active with a cancellation date, and never as `cancelled`.
type subscriptionRecord struct {
	ID                  string  `json:"id"`
	AccountID           string  `json:"account_id"`
	PlanVersionID       string  `json:"plan_version_id"`
	State               string  `json:"state"`
	StartsAt            string  `json:"starts_at,omitempty"`
	CurrentPeriodStart  *string `json:"current_period_start"`
	CurrentPeriodEnd    *string `json:"current_period_end"`
	CancelAt            *string `json:"cancel_at"`
	CreatedAt           string  `json:"created_at"`
	UpdatedAt           string  `json:"updated_at,omitempty"`
}

// entitlementRecord mirrors the contract's Entitlement.
//
// There is no remaining, no available and no balance field here, and the
// omission is the design: an entitlement is a GRANT, capacity is drawn on a
// funding bucket, and a third copy of the same number with no rebuild story is
// how two consoles start disagreeing about what a customer bought. The bucket
// list is where what remains is read.
type entitlementRecord struct {
	ID                  string `json:"id"`
	SubscriptionID      string `json:"subscription_id"`
	GrantDefinitionID   string `json:"grant_definition_id,omitempty"`
	Cycle               int    `json:"cycle"`
	Scope               string `json:"scope,omitempty"`
	Dimension           string `json:"dimension,omitempty"`
	GrantedMinorUnits   int64  `json:"granted_minor_units,omitempty"`
	ScopeVersionID      string `json:"scope_version_id,omitempty"`
	State               string `json:"state"`
	PeriodStart         string `json:"period_start"`
	PeriodEnd           string `json:"period_end"`
	CreatedAt           string `json:"created_at"`
	UpdatedAt           string `json:"updated_at,omitempty"`
}

// fundingBucketRecord mirrors the contract's FundingBucket.
//
// `kind` is what tells an entitlement bucket from an account PAYG bucket, and
// the schema requires it: the two are never one bucket, because a cycle that
// rolls must not take the PAYG balance with it, and a list that merged them
// would render two sets of figures with incompatible meanings side by side.
// The owner is nullable on exactly the two fields the schema makes nullable,
// and no handler decides which of the two it is — it renders what the row
// carries.
type fundingBucketRecord struct {
	ID            string   `json:"id"`
	Kind          string   `json:"kind"`
	EntitlementID *string  `json:"entitlement_id"`
	AccountID     *string  `json:"account_id"`
	Status        string   `json:"status"`
	Balances      balances `json:"balances"`
	Version       int64    `json:"version"`
	OpenedAt      *string  `json:"opened_at"`
	ClosedAt      *string  `json:"closed_at"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at,omitempty"`
}

// ledgerEntryRecord mirrors the contract's LedgerEntry: one immutable leg.
//
// The deltas are signed and are rendered as stored. Sign is meaningful on a
// delta and never on a balance — a balance is what a bucket holds and this
// plane refuses to store a negative one — so nothing here takes an absolute
// value, and no handler formats either number as money: there is no currency
// to format it in, and a symbol on a ledger is a claim the ledger has not made.
type ledgerEntryRecord struct {
	ID               string         `json:"id"`
	FundingBucketID  string         `json:"funding_bucket_id"`
	Kind             string         `json:"kind"`
	Sequence         int64          `json:"sequence"`
	SettledDelta     money          `json:"settled_delta"`
	HeldDelta        money          `json:"held_delta"`
	SettlementID     *string        `json:"settlement_id"`
	ReservationID    *string        `json:"reservation_id"`
	CommandKey       *string        `json:"command_key"`
	Price            *priceSnapshot `json:"price"`
	AdjustmentReason *string        `json:"adjustment_reason"`
	OperatorID       *string        `json:"operator_id"`
	CreatedAt        string         `json:"created_at"`
}

// findingRecord mirrors the contract's Finding.
//
// The evidence is a json.RawMessage so it reaches the wire as the structured
// JSON the check that found the divergence wrote, and is NEVER interpreted
// here: no summing, no re-aggregating, no key ordering imposed, no rendering as
// a number. Its shape is that check's business and not this contract's to fix,
// so a new check is a new evidence shape rather than a migration — which is
// why the field is opaque and stays opaque.
//
// A finding's `observed` is optional in the schema, and a check that compared
// nothing is honest rather than a placeholder. The field is therefore omitted
// when the check wrote nothing, rather than rendered as `{}` — a client that
// saw an empty object could read it as a check that compared two empty
// things, which is a different statement about the divergence.
type findingRecord struct {
	ID          int64           `json:"id"`
	CheckKind   string          `json:"check_kind"`
	SubjectKind string          `json:"subject_kind"`
	SubjectID   string          `json:"subject_id"`
	Severity    string          `json:"severity"`
	Status      string          `json:"status"`
	Observed    json.RawMessage `json:"observed,omitempty"`
	Detail      string          `json:"detail,omitempty"`
	DetectedAt  string          `json:"detected_at"`
	LastSeenAt  string          `json:"last_seen_at"`
	ResolvedAt  *string         `json:"resolved_at"`
}

// reconciliationRunRecord mirrors the contract's ReconciliationRun.
//
// A run whose finished_at is null is a pass that started and never finished,
// and it is rendered as such rather than hidden: a wedged worker and an idle
// one are otherwise indistinguishable. The window is returned as its two
// halves because the schema's window_to is EXCLUSIVE — a window rendered as an
// inclusive pair is one an operator cannot tell from one that overlapped the
// last.
type reconciliationRunRecord struct {
	ID               int64   `json:"id"`
	Scope            string  `json:"scope"`
	Status           string  `json:"status"`
	WindowFrom       string  `json:"window_from"`
	WindowTo         string  `json:"window_to"`
	StartedAt        string  `json:"started_at"`
	FinishedAt       *string `json:"finished_at"`
	BucketsScanned   int64   `json:"buckets_scanned"`
	FindingsOpened   int64   `json:"findings_opened"`
	FindingsUnchanged int64  `json:"findings_unchanged"`
}

// accountOverviewResponse mirrors the contract's AccountOverview, the
// dashboard's server-side composition.
//
// Every field is a stored figure or a bounded list of stored rows, and that is
// the whole of this DTO's job: it renders what the server computed and nothing
// else. There is no arithmetic here — no sum, no difference, no derived
// balance, no count of a page's length — because a client that computed a
// figure from three responses would be doing the plane's arithmetic in the
// browser, where a disagreement has no authority behind it. The two counts are
// the contract's own `user_count` and `active_api_key_count` as the
// application read them; `open_finding_count` is the one number here that is
// not the account's alone.
type accountOverviewResponse struct {
	Account           accountRecord          `json:"account"`
	UserCount         int                    `json:"user_count,omitempty"`
	ActiveAPIKeyCount int                    `json:"active_api_key_count,omitempty"`
	Subscriptions     []subscriptionRecord   `json:"subscriptions"`
	PAYGBalances      []fundingBucketRecord  `json:"payg_balances"`
	OpenFindingCount  int                    `json:"open_finding_count,omitempty"`
}
