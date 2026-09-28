package http

import (
	"context"
)

// The ten read operations, as the narrow seam their handlers call.
//
// It is a separate interface from SessionUseCases rather than ten more methods
// on that one, and the reason is what a nil means. The session surface is this
// service's authentication boundary: a process that started with none would
// answer every product operation as though every caller were signed in. The
// read surface is a screen: a process that started with none has a console
// that renders nothing, which is a missing feature rather than a wrong answer.
// Keeping them apart means each can be refused, and refused loudly, on its own
// terms — a nil session use case panics at wiring, a nil read use case panics
// at wiring, and neither is answerable by the other.
//
// The account is a parameter of every account-scoped call and never a field of
// the request. It is the session's — resolveSession resolved it and the handler
// passes it as this seam's first argument — so there is no request type on this
// surface a caller could set an account on, and no way to thread a different
// one in without editing the call. The three whole-plane reads take no account
// at all rather than taking one and ignoring it, because a plan catalogue and
// a reconciliation pass are facts about the plane and naming the account would
// imply a scoping they do not have.
//
// Every field is plain: strings, integers, booleans and the nullable string a
// `*string` is. No domain type crosses this boundary, for the import-rule
// reason at the top of wire.go — the transport renders its own shapes, and a
// handler holding an aggregate would be holding the plane's grammar to decide
// what a client should see.
type ConsoleReadUseCases interface {
	// AccountOverview is the dashboard's one server-side composition: figures
	// that live in three bounded contexts, assembled by the application with
	// one account scope, so a client is not reimplementing a join in the
	// browser and is not computing a figure from three responses. The account
	// is the session's.
	AccountOverview(ctx context.Context, accountID string) (AccountOverviewResult, error)

	// ListUsers returns one page of the account's console users. state is the
	// caller's optional lifecycle filter and empty is every live user. The
	// account is the port's predicate, so another account's row is a row the
	// statement never returned.
	ListUsers(ctx context.Context, accountID, state, after string, limit int) (UserPageResult, error)

	// ListAPIKeys returns one page of the account's keys, oldest first. Every
	// key is an ownership record; no member of this surface can return the
	// credential one was minted with.
	ListAPIKeys(ctx context.Context, accountID, after string, limit int) (APIKeyPageResult, error)

	// ListPlans returns one page of the plan catalogue, which is not
	// account-scoped: a plan is what every account buys from. It takes no
	// account for that reason.
	ListPlans(ctx context.Context, after string, limit int) (PlanPageResult, error)

	// ListSubscriptions returns one page of what the account has bought. A
	// scheduled cancellation is data beside an unchanged state, never a state
	// of its own.
	ListSubscriptions(ctx context.Context, accountID, after string, limit int) (SubscriptionPageResult, error)

	// ListEntitlements returns one page of the grants the account's
	// subscriptions materialised. An entitlement is a grant and carries no
	// balance; the remaining figure lives on a funding bucket.
	ListEntitlements(ctx context.Context, accountID, after string, limit int) (EntitlementPageResult, error)

	// ListFundingBuckets returns one page of the account's buckets with their
	// three cached balances, rendered as stored and never derived by a client
	// from one another.
	ListFundingBuckets(ctx context.Context, accountID, after string, limit int) (FundingBucketPageResult, error)

	// ListLedgerEntries returns one page of ONE bucket's legs, oldest first.
	// This is the only operation on the surface that names a resource in its
	// path, and the bucket and the account travel together so the account
	// predicate and the bucket sit in the same statement: a bucket the
	// session's account does not own is a row that statement did not return,
	// and answers exactly as one that does not exist.
	ListLedgerEntries(ctx context.Context, accountID, bucketID, kind, after string, limit int) (LedgerEntryPageResult, error)

	// ListFindings returns one page of the recorded divergences, newest first.
	// Whole-plane, like the open count in the overview: a finding's subject may
	// be any entity, and findings are not scoped to one account. status and
	// severity are the caller's optional filters.
	ListFindings(ctx context.Context, status, severity, after string, limit int) (FindingPageResult, error)

	// ListReconciliationRuns returns one page of the pass history, newest
	// first. A pass whose finished_at is nil is returned as it stands rather
	// than hidden, because a wedged worker and an idle one are otherwise
	// indistinguishable.
	ListReconciliationRuns(ctx context.Context, after string, limit int) (ReconciliationRunPageResult, error)
}

// The rows and pages the seam returns, in plain fields. One set per contract
// schema, in the same order the schemas appear in
// api/openapi/shared/console.yaml, so a reader compares two lists rather than
// hunting for a counterpart.
//
// A timestamp is a string and a nullable one is a *string, because the wire
// types them that way and because a *time.Time in a transport DTO is a value
// whose zero time would render as 0001-01-01 if a conversion forgot the
// absence. The empty string and the nil pointer are the two absences a field
// on this surface has, and both are honoured rather than defaulted.

// AccountRecord is the seam's account row.
type AccountRecord struct {
	ID        string
	Name      string
	State     string
	CreatedAt string
	UpdatedAt string
}

// APIKeyRecord is the seam's key record, as every read but the mint returns it.
// The credential is absent from the type: this plane stores no plaintext and
// no digest, and a read that could return one would make "shown once" a
// property of the client rather than of the system.
type APIKeyRecord struct {
	ID          string
	AccountID   string
	CreatedBy   string
	DisplayName string
	Prefix      string
	State       string
	CreatedAt   string
	UpdatedAt   string
	RevokedAt   string
}

// UserRecord is the seam's user row.
type UserRecord struct {
	ID        string
	AccountID string
	Email     string
	State     string
	CreatedAt string
	UpdatedAt string
}

// PlanRecord is the seam's catalogue row. There is no versions array, because
// the contract's Plan schema declares none — see planRecord in readwire.go for
// what that means and what it would take to change.
type PlanRecord struct {
	ID        string
	Name      string
	CreatedAt string
	UpdatedAt string
}

// SubscriptionRecord is the seam's subscription row. CurrentPeriodStart,
// CurrentPeriodEnd and CancelAt are nil exactly when the subscription carries
// no such instant, and a scheduled cancellation leaves the state alone.
type SubscriptionRecord struct {
	ID                 string
	AccountID          string
	PlanVersionID      string
	State              string
	StartsAt           string
	CurrentPeriodStart *string
	CurrentPeriodEnd   *string
	CancelAt           *string
	CreatedAt          string
	UpdatedAt          string
}

// EntitlementRecord is the seam's grant row. There is no remaining and no
// available: the contract's Entitlement schema has neither, and capacity is
// drawn on a funding bucket.
type EntitlementRecord struct {
	ID                string
	SubscriptionID    string
	GrantDefinitionID string
	Cycle             int
	Scope             string
	Dimension         string
	GrantedMinorUnits int64
	ScopeVersionID    string
	State             string
	PeriodStart       string
	PeriodEnd         string
	CreatedAt         string
	UpdatedAt         string
}

// FundingBucketRecord is the seam's bucket row. The three balances are int64
// minor units as stored and are never summed or derived by anything above this
// line — the ledger is the authority and these are its projection.
//
// EntitlementID and AccountID are the two nullable owner references: exactly one
// is set, and which one is a fact about the row, not a decision this layer
// makes.
type FundingBucketRecord struct {
	ID            string
	Kind          string
	EntitlementID *string
	AccountID     *string
	Status        string
	Settled       int64
	Held          int64
	Available     int64
	Version       int64
	OpenedAt      *string
	ClosedAt      *string
	CreatedAt     string
	UpdatedAt     string
}

// PriceSnapshotRecord is the consume leg's price provenance, copied by value.
// Both unit prices are minor units with no currency, for the same reason every
// amount on this surface has none.
type PriceSnapshotRecord struct {
	RevisionID      string
	InputUnitPrice  int64
	OutputUnitPrice int64
}

// LedgerEntryRecord is the seam's leg. Both deltas are signed: sign is
// meaningful on a delta and never on a balance. Price is non-nil exactly on a
// consume leg, and its absence is a nil rather than a zero snapshot.
type LedgerEntryRecord struct {
	ID               string
	FundingBucketID  string
	Kind             string
	Sequence         int64
	SettledDelta     int64
	HeldDelta        int64
	SettlementID     *string
	ReservationID    *string
	CommandKey       *string
	Price            *PriceSnapshotRecord
	AdjustmentReason *string
	OperatorID       *string
	CreatedAt        string
}

// FindingRecord is the seam's recorded divergence. Observed is the evidence the
// check compared, as the JSON text that check wrote, and it crosses as text
// precisely so nothing above this line can interpret it: a client rendering
// evidence and a client summing it are different clients, and only the first is
// what this surface offers.
type FindingRecord struct {
	ID          int64
	CheckKind   string
	SubjectKind string
	SubjectID   string
	Severity    string
	Status      string
	Observed    string
	Detail      string
	DetectedAt  string
	LastSeenAt  string
	ResolvedAt  *string
}

// ReconciliationRunRecord is the seam's pass. FinishedAt is nil while the pass
// is running or died between two writes, and is rendered as such rather than
// hidden. WindowFrom is inclusive and WindowTo exclusive, the half-open window
// the next pass opens from.
type ReconciliationRunRecord struct {
	ID                int64
	Scope             string
	Status            string
	WindowFrom        string
	WindowTo          string
	StartedAt         string
	FinishedAt        *string
	BucketsScanned    int64
	FindingsOpened    int64
	FindingsUnchanged int64
}

// The page shapes. Three fields each — the rows, whether more exist, and where
// to continue — and the absence of a fourth is the PageEnvelope's design rather
// than a shorthand: a total is a number that is wrong the instant it is
// written, and one list here computing a count another does not is how two
// screens of the same console start disagreeing about how much of something
// there is. The NextCursor is non-empty on every page, including the last; a
// page carrying none is not a page a client can hold.
type (
	// UserPageResult is one page of the account's users.
	UserPageResult struct {
		Items      []UserRecord
		HasMore    bool
		NextCursor string
	}
	// APIKeyPageResult is one page of the account's keys.
	APIKeyPageResult struct {
		Items      []APIKeyRecord
		HasMore    bool
		NextCursor string
	}
	// PlanPageResult is one page of the catalogue.
	PlanPageResult struct {
		Items      []PlanRecord
		HasMore    bool
		NextCursor string
	}
	// SubscriptionPageResult is one page of what the account has bought.
	SubscriptionPageResult struct {
		Items      []SubscriptionRecord
		HasMore    bool
		NextCursor string
	}
	// EntitlementPageResult is one page of the account's grants.
	EntitlementPageResult struct {
		Items      []EntitlementRecord
		HasMore    bool
		NextCursor string
	}
	// FundingBucketPageResult is one page of the account's buckets.
	FundingBucketPageResult struct {
		Items      []FundingBucketRecord
		HasMore    bool
		NextCursor string
	}
	// LedgerEntryPageResult is one page of one bucket's legs.
	LedgerEntryPageResult struct {
		Items      []LedgerEntryRecord
		HasMore    bool
		NextCursor string
	}
	// FindingPageResult is one page of the recorded divergences.
	FindingPageResult struct {
		Items      []FindingRecord
		HasMore    bool
		NextCursor string
	}
	// ReconciliationRunPageResult is one page of the pass history.
	ReconciliationRunPageResult struct {
		Items      []ReconciliationRunRecord
		HasMore    bool
		NextCursor string
	}
)

// AccountOverviewResult is the dashboard's composed answer. Every field is
// either a stored figure or a bounded list of stored rows; the two counts are
// the contract's own, and OpenFindingCount is the one number here that is not
// the account's alone, because how the plane is doing is not what this customer
// owes. Nothing here is computed by the transport: a client that summed a
// subscription list or derived a balance from the buckets would be doing the
// plane's arithmetic in the browser, where a disagreement has no authority
// behind it.
type AccountOverviewResult struct {
	Account           AccountRecord
	UserCount         int
	ActiveAPIKeyCount int
	Subscriptions     []SubscriptionRecord
	PAYGBalances      []FundingBucketRecord
	OpenFindingCount  int
}
