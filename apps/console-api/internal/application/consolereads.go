package application

import (
	"context"
	"strconv"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/commerce"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/identity"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/ports/outbound/persistence"
)

// The console's ten read operations, as use cases.
//
// Every one of them is a read, and every one of them carries the account from
// its CALLER — the session principal the transport resolved — into the
// statement as the port's first argument. The account never arrives in a path
// segment, a query parameter, or a request body, because on this surface there
// is exactly one resource id in a URL (a funding bucket) and no account id in
// one at all (ADR 0012 §2: a path-scoped /accounts/{id}/... writes every
// operator's reading of every customer's ledger into every reverse proxy's
// access log).
//
// A list returns a page, and a page is three facts: the rows, whether more
// exist past them, and the cursor to continue from. There is no total anywhere
// in this file, and adding one later would be a number that is wrong the
// instant it is written. The cursor is minted and placed here, and only the
// sort key crosses the port boundary.
//
// The two whole-plane operations — the plan catalogue and the reconciliation
// lists — are the ones the contract says in its own words are not
// account-scoped: a plan is what every account buys from, and a
// reconciliation pass sweeps the plane. They take no account at all rather
// than taking one and ignoring it.

// The collection names inside a cursor. They are constants rather than the
// operationId spelled at each call site, because the name inside a cursor is
// compared: a mismatch is what stops a cursor minted for one collection from
// paging another by a keyset that happens to be total over this one's rows.
const (
	collectionUsers         = "console.users"
	collectionAPIKeys       = "console.api-keys"
	collectionPlans         = "console.plans"
	collectionSubscriptions = "console.subscriptions"
	collectionEntitlements  = "console.entitlements"
	collectionBuckets       = "console.funding-buckets"
	collectionLedger        = "console.ledger"
	collectionFindings      = "console.reconciliation.findings"
	collectionRuns          = "console.reconciliation.runs"
)

// ConsoleReads is the console's read surface: the ten operations the contract
// declares, over the read ports the SQL layer already implements.
//
// It is one type rather than ten because the ten share their shape — an
// account, a cursor, a page size, a page back — and the sharing is the point:
// the one place that decides what a page carries, and the one place that
// refuses a limit it cannot honour, is a place a reader can check rather than
// ten places they have to check in pairs.
type ConsoleReads struct {
	accounts      persistence.Accounts
	users         persistence.AccountUsers
	keys          persistence.AccountAPIKeys
	plans         persistence.AccountPlans
	subscriptions persistence.AccountSubscriptions
	entitlements  persistence.AccountEntitlements
	buckets       persistence.AccountBuckets
	ledger        persistence.AccountLedger
	findings      persistence.AccountFindings
	runs          persistence.AccountRuns

	// openFindings is the worker's own headline read, reused by the
	// dashboard's composition. It is not a member of AccountFindings: that
	// port is the console's read of the list, and a count over the whole table
	// is the reconciliation worker's own figure — a fact about the plane
	// rather than about a page, which is exactly why the contract names it as
	// the one number in the overview that is not the account's alone.
	openFindings persistence.ReconciliationFindings
}

// NewConsoleReads returns the console's read use cases over the ports that
// already carry them. It panics on a nil port for the reason every constructor
// in this module does: a half-wired read surface fails at wiring time, where
// the stack says which screen is missing, rather than on a customer's first
// request to it.
func NewConsoleReads(
	accounts persistence.Accounts,
	users persistence.AccountUsers,
	keys persistence.AccountAPIKeys,
	plans persistence.AccountPlans,
	subscriptions persistence.AccountSubscriptions,
	entitlements persistence.AccountEntitlements,
	buckets persistence.AccountBuckets,
	ledger persistence.AccountLedger,
	findings persistence.AccountFindings,
	runs persistence.AccountRuns,
	openFindings persistence.ReconciliationFindings,
) *ConsoleReads {
	if accounts == nil || users == nil || keys == nil || plans == nil || subscriptions == nil ||
		entitlements == nil || buckets == nil || ledger == nil || findings == nil || runs == nil ||
		openFindings == nil {
		panic("application: NewConsoleReads requires every console read port to be non-nil")
	}
	return &ConsoleReads{
		accounts:      accounts,
		users:         users,
		keys:          keys,
		plans:         plans,
		subscriptions: subscriptions,
		entitlements:  entitlements,
		buckets:       buckets,
		ledger:        ledger,
		findings:      findings,
		runs:          runs,
		openFindings:  openFindings,
	}
}

// A page of users, as the wire carries it: the rows, whether more exist, and
// where to continue. There is deliberately no total, and this struct is where
// that absence is written down rather than left to a reader to infer.
type UserPage struct {
	Items      []identity.User
	HasMore    bool
	NextCursor string
}

// ListUsers returns one page of the account's users.
//
// The account comes from the session and is the port's first argument, so
// another account's row is a row the statement never returned — the same
// answer, at the same cost, a genuinely absent row gives. A 403 there would
// confirm the row exists.
//
// state is the caller's optional lifecycle filter, and it is a predicate in
// the same statement rather than a test on rows already fetched. An empty
// state is "every live user": invited and active together, which is what a
// member list is.
func (reads *ConsoleReads) ListUsers(ctx context.Context, accountID identity.AccountID, state, cursor string, limit int) (UserPage, error) {
	if accountID == "" {
		return UserPage{}, invalidRequest("a session must name an account before the users list can be read")
	}
	if err := checkUserState(state); err != nil {
		return UserPage{}, err
	}
	filters := Fingerprint(collectionUsers, map[string]string{"state": state})
	after, pageSize, err := ResolvePage(collectionUsers, cursor, limit, filters)
	if err != nil {
		return UserPage{}, err
	}
	rows, err := reads.users.ListForAccount(ctx, accountID, persistence.UserPage{
		State: state,
		After: identity.UserID(after),
		Limit: pageSize,
	})
	if err != nil {
		return UserPage{}, Internal(err)
	}
	items, hasMore, next := PageOf(rows, pageSize, func(u identity.User) string { return string(u.ID) }, collectionUsers, filters, after)
	return UserPage{Items: items, HasMore: hasMore, NextCursor: next}, nil
}

// checkUserState refuses a lifecycle state outside the contract's own three.
// It is a refusal rather than a passthrough for the same reason the limit is:
// a value this list cannot honour would otherwise become a filter that matches
// nothing and reads as "this account has no users", which is a different
// statement about the account than the one the caller made.
func checkUserState(state string) error {
	switch identity.UserState(state) {
	case "", identity.UserInvited, identity.UserActive, identity.UserRemoved:
		return nil
	default:
		return invalidRequest("the state parameter must be one of invited, active or removed")
	}
}

// An APIKeyPage is one page of the account's keys. Every key here is an
// ownership record: the credential a key was minted with exists once, in the
// mint's response, and no member of this surface can return it.
type APIKeyPage struct {
	Items      []identity.APIKey
	HasMore    bool
	NextCursor string
}

// ListAPIKeys returns one page of the account's keys, oldest first.
//
// Unfiltered by design: an account's keys are a small, bounded set an
// operator reads whole, and a state filter here would be a filter the account
// predicate has nothing to do with. The cursor therefore carries no filters at
// all beyond the collection's name.
func (reads *ConsoleReads) ListAPIKeys(ctx context.Context, accountID identity.AccountID, cursor string, limit int) (APIKeyPage, error) {
	if accountID == "" {
		return APIKeyPage{}, invalidRequest("a session must name an account before the api keys list can be read")
	}
	filters := Fingerprint(collectionAPIKeys, nil)
	after, pageSize, err := ResolvePage(collectionAPIKeys, cursor, limit, filters)
	if err != nil {
		return APIKeyPage{}, err
	}
	rows, err := reads.keys.ListForAccount(ctx, accountID, persistence.APIKeyPage{
		After: identity.APIKeyID(after),
		Limit: pageSize,
	})
	if err != nil {
		return APIKeyPage{}, Internal(err)
	}
	items, hasMore, next := PageOf(rows, pageSize, func(k identity.APIKey) string { return string(k.ID) }, collectionAPIKeys, filters, after)
	return APIKeyPage{Items: items, HasMore: hasMore, NextCursor: next}, nil
}

// A PlanPage is one page of the plan catalogue.
type PlanPage struct {
	Items      []commerce.Plan
	HasMore    bool
	NextCursor string
}

// ListPlans returns one page of the plan catalogue.
//
// The one list on this surface with no account predicate, and that is the
// contract's statement rather than an omission: a plan is not account-scoped,
// it is the catalogue every account buys from, so the operation returns the
// whole thing. "The plans this account uses" is a different question, and the
// subscriptions list is where it is answered.
func (reads *ConsoleReads) ListPlans(ctx context.Context, cursor string, limit int) (PlanPage, error) {
	filters := Fingerprint(collectionPlans, nil)
	after, pageSize, err := ResolvePage(collectionPlans, cursor, limit, filters)
	if err != nil {
		return PlanPage{}, err
	}
	rows, err := reads.plans.List(ctx, persistence.PlanPage{
		After: commerce.PlanID(after),
		Limit: pageSize,
	})
	if err != nil {
		return PlanPage{}, Internal(err)
	}
	items, hasMore, next := PageOf(rows, pageSize, func(p commerce.Plan) string { return string(p.ID) }, collectionPlans, filters, after)
	return PlanPage{Items: items, HasMore: hasMore, NextCursor: next}, nil
}

// A SubscriptionPage is one page of what the account has bought.
type SubscriptionPage struct {
	Items      []commerce.Subscription
	HasMore    bool
	NextCursor string
}

// ListSubscriptions returns one page of the account's subscriptions.
//
// State and cancel_at stay separate here as they are in the schema: a
// scheduled cancellation is DATA, NOT A STATE — the subscription stays active
// and usable until that instant passes — so a row carrying a future cancel_at
// is active, and one that rendered "cancelled" for it would describe a
// decision that has not taken effect yet.
func (reads *ConsoleReads) ListSubscriptions(ctx context.Context, accountID commerce.AccountID, cursor string, limit int) (SubscriptionPage, error) {
	if accountID == "" {
		return SubscriptionPage{}, invalidRequest("a session must name an account before the subscriptions list can be read")
	}
	filters := Fingerprint(collectionSubscriptions, nil)
	after, pageSize, err := ResolvePage(collectionSubscriptions, cursor, limit, filters)
	if err != nil {
		return SubscriptionPage{}, err
	}
	rows, err := reads.subscriptions.ListForAccount(ctx, accountID, persistence.SubscriptionPage{
		After: commerce.SubscriptionID(after),
		Limit: pageSize,
	})
	if err != nil {
		return SubscriptionPage{}, Internal(err)
	}
	items, hasMore, next := PageOf(rows, pageSize, func(s commerce.Subscription) string { return string(s.ID) }, collectionSubscriptions, filters, after)
	return SubscriptionPage{Items: items, HasMore: hasMore, NextCursor: next}, nil
}

// An EntitlementPage is one page of the grants the account's rolls
// materialised.
//
// An entitlement is a GRANT, not a balance, and there is no remaining or
// available column in this type: capacity is drawn on a funding bucket, and
// this read and that one are deliberately separate.
type EntitlementPage struct {
	Items      []commerce.Entitlement
	HasMore    bool
	NextCursor string
}

// ListEntitlements returns one page of the account's entitlements.
//
// The entitlements table carries no account column, so the port's predicate is
// an EXISTS against the subscription that owns each grant — still the WHERE
// clause, still the first argument, still evaluated before any row is
// returned. That is what keeps "not yours" and "not there" the same answer.
func (reads *ConsoleReads) ListEntitlements(ctx context.Context, accountID commerce.AccountID, cursor string, limit int) (EntitlementPage, error) {
	if accountID == "" {
		return EntitlementPage{}, invalidRequest("a session must name an account before the entitlements list can be read")
	}
	filters := Fingerprint(collectionEntitlements, nil)
	after, pageSize, err := ResolvePage(collectionEntitlements, cursor, limit, filters)
	if err != nil {
		return EntitlementPage{}, err
	}
	rows, err := reads.entitlements.ListForAccount(ctx, accountID, persistence.EntitlementPage{
		After: commerce.EntitlementID(after),
		Limit: pageSize,
	})
	if err != nil {
		return EntitlementPage{}, Internal(err)
	}
	items, hasMore, next := PageOf(rows, pageSize, func(e commerce.Entitlement) string { return string(e.ID) }, collectionEntitlements, filters, after)
	return EntitlementPage{Items: items, HasMore: hasMore, NextCursor: next}, nil
}

// A FundingBucketPage is one page of the account's buckets, each with the
// three cached balances the domain defines.
//
// The balances come back as stored and are never summed here or anywhere
// else: a Σ over this list's deltas is not a number any of the three balances
// means, and a balance derived from a page is a balance derived from a page.
type FundingBucketPage struct {
	Items      []accounting.Bucket
	HasMore    bool
	NextCursor string
}

// ListFundingBuckets returns one page of the account's buckets — its
// entitlement cycles and its PAYG balance alike.
//
// One predicate covers both kinds because the account id is in the row either
// way: an entitlement bucket's owning account is present in account_id too, so
// no join to entitlements is needed to evaluate it.
func (reads *ConsoleReads) ListFundingBuckets(ctx context.Context, accountID accounting.AccountID, cursor string, limit int) (FundingBucketPage, error) {
	if accountID == "" {
		return FundingBucketPage{}, invalidRequest("a session must name an account before the funding buckets list can be read")
	}
	filters := Fingerprint(collectionBuckets, nil)
	after, pageSize, err := ResolvePage(collectionBuckets, cursor, limit, filters)
	if err != nil {
		return FundingBucketPage{}, err
	}
	rows, err := reads.buckets.ListForAccount(ctx, accountID, persistence.FundingBucketPage{
		After: accounting.FundingBucketID(after),
		Limit: pageSize,
	})
	if err != nil {
		return FundingBucketPage{}, Internal(err)
	}
	items, hasMore, next := PageOf(rows, pageSize, func(b accounting.Bucket) string { return string(b.ID) }, collectionBuckets, filters, after)
	return FundingBucketPage{Items: items, HasMore: hasMore, NextCursor: next}, nil
}

// A LedgerEntryPage is one bucket's legs, oldest first.
//
// The keyset is the bucket's own allocated sequence, and the page is per
// bucket for the reason ADR 0012 §4 states: a ledger is a fact about where
// money went for ONE owner of that money, and a merged list of every bucket's
// legs is a question this plane has no index to answer.
type LedgerEntryPage struct {
	Items      []accounting.LedgerEntry
	HasMore    bool
	NextCursor string
}

// ListLedgerEntries returns one page of one bucket's history.
//
// This is the only operation on the surface that names a resource in its
// path, and the id it names is a bucket. A bucket the session's account does
// not own is a 404 the query did not return, exactly as if it did not exist —
// the port's first argument is the account and the bucket is inside the same
// statement, so "not yours" and "not there" cost the same and answer the same.
// Never a 403: a 403 would confirm the bucket exists.
//
// kind is the caller's optional leg-kind filter, and it is a real predicate in
// the same statement. An operator reads a ledger for two different questions —
// what came in, and what went out — and the two are not the same read.
func (reads *ConsoleReads) ListLedgerEntries(ctx context.Context, accountID accounting.AccountID, bucketID accounting.FundingBucketID, kind, cursor string, limit int) (LedgerEntryPage, error) {
	if accountID == "" {
		return LedgerEntryPage{}, invalidRequest("a session must name an account before a ledger can be read")
	}
	if bucketID == "" {
		return LedgerEntryPage{}, invalidRequest("the funding_bucket_id path parameter is required")
	}
	if err := checkLedgerKind(kind); err != nil {
		return LedgerEntryPage{}, err
	}
	// The bucket is inside the fingerprint as well as inside the path: a
	// cursor earned on bucket A replayed against bucket B would page B by a
	// sequence that means something else entirely, and the leg it landed on
	// would be a leg of B's history at A's position.
	filters := Fingerprint(collectionLedger, map[string]string{
		"kind":   kind,
		"bucket": string(bucketID),
	})
	after, pageSize, err := ResolvePage(collectionLedger, cursor, limit, filters)
	if err != nil {
		return LedgerEntryPage{}, err
	}
	rows, err := reads.ledger.ListForBucket(ctx, accountID, bucketID, persistence.LedgerPage{
		Kind:          kind,
		AfterSequence: parseSequence(after),
		Limit:         pageSize,
	})
	if err != nil {
		return LedgerEntryPage{}, Internal(err)
	}
	items, hasMore, next := PageOf(rows, pageSize, func(e accounting.LedgerEntry) string {
		return strconv.FormatInt(e.Sequence, 10)
	}, collectionLedger, filters, after)
	return LedgerEntryPage{Items: items, HasMore: hasMore, NextCursor: next}, nil
}

// checkLedgerKind refuses a leg kind outside the six the domain declares. A
// value this list cannot honour would otherwise become a filter that matches
// nothing, and an empty page reads as "this bucket has no legs" — a statement
// about the bucket's money that nobody made.
//
// The six are named rather than ranged over a domain slice, because the domain
// deliberately has no exported list of them: they are the six cases of a
// switch, each with its own semantics, and a slice here would be a second
// vocabulary to keep in step with the first. The contract's enum is the third
// and the one this list is measured against.
func checkLedgerKind(kind string) error {
	switch accounting.Kind(kind) {
	case "",
		accounting.KindGrant,
		accounting.KindTopup,
		accounting.KindHold,
		accounting.KindRelease,
		accounting.KindConsume,
		accounting.KindAdjustment:
		return nil
	default:
		return invalidRequest("the kind parameter must be one of grant, topup, hold, release, consume or adjustment")
	}
}

// parseSequence reads the keyset position a cursor named for a sequence
// keyset. The zero value is the beginning of the bucket's history, which is
// what an absent cursor produces and what a bucket's first leg (sequence 1)
// sorts after. A cursor that named a non-numeric sequence would have been
// refused when it was minted — EncodeCursor only ever writes one this package
// parsed — so the fallback is the beginning rather than an error.
func parseSequence(after string) int64 {
	sequence, err := strconv.ParseInt(after, 10, 64)
	if err != nil || sequence < 0 {
		return 0
	}
	return sequence
}

// A FindingPage is one page of the divergences the worker found.
//
// A finding's subject may be a bucket, a settlement, a request, or the literal
// control_plane for a feed-wide signal, and findings are not scoped to one
// account. The list is therefore whole-plane, in the same way
// open_finding_count is, and the contract says so in those words.
type FindingPage struct {
	Items      []persistence.Finding
	HasMore    bool
	NextCursor string
}

// ListFindings returns one page of findings, newest first.
//
// The id is the keyset rather than detected_at: it is unique, and detected_at
// is two passes' clocks colliding on one divergence, which is the non-total
// key this plane refuses to page on everywhere else.
//
// The id is also descending — the port returns the newest first — so the
// cursor names the last row returned and the statement's own `id < $n` bound
// resumes below it. It is the one list here that walks backwards, and the
// port's keyset is written for exactly that.
func (reads *ConsoleReads) ListFindings(ctx context.Context, status, severity, cursor string, limit int) (FindingPage, error) {
	if err := checkFindingStatus(status); err != nil {
		return FindingPage{}, err
	}
	if err := checkFindingSeverity(severity); err != nil {
		return FindingPage{}, err
	}
	filters := Fingerprint(collectionFindings, map[string]string{
		"status":   status,
		"severity": severity,
	})
	after, pageSize, err := ResolvePage(collectionFindings, cursor, limit, filters)
	if err != nil {
		return FindingPage{}, err
	}
	rows, err := reads.findings.List(ctx, persistence.FindingPage{
		Status:   status,
		Severity: severity,
		After:    parseSequence(after),
		Limit:    pageSize,
	})
	if err != nil {
		return FindingPage{}, Internal(err)
	}
	items, hasMore, next := PageOf(rows, pageSize, func(f persistence.Finding) string {
		return strconv.FormatInt(f.ID, 10)
	}, collectionFindings, filters, after)
	return FindingPage{Items: items, HasMore: hasMore, NextCursor: next}, nil
}

func checkFindingStatus(status string) error {
	switch status {
	case "", "open", "acknowledged", "resolved":
		return nil
	default:
		return invalidRequest("the status parameter must be one of open, acknowledged or resolved")
	}
}

func checkFindingSeverity(severity string) error {
	switch severity {
	case "", "info", "warning", "critical":
		return nil
	default:
		return invalidRequest("the severity parameter must be one of info, warning or critical")
	}
}

// A ReconciliationRunPage is one page of the pass history.
type ReconciliationRunPage struct {
	Items      []persistence.Run
	HasMore    bool
	NextCursor string
}

// ListReconciliationRuns returns one page of passes, newest first.
//
// Whole-plane for the reason AccountFindings has no predicate: a pass sweeps
// the plane. A run whose FinishedAt is nil is returned as it stands rather
// than hidden — a wedged worker and an idle one are otherwise
// indistinguishable, and the contract says rendering that is worth the trouble.
func (reads *ConsoleReads) ListReconciliationRuns(ctx context.Context, cursor string, limit int) (ReconciliationRunPage, error) {
	filters := Fingerprint(collectionRuns, nil)
	after, pageSize, err := ResolvePage(collectionRuns, cursor, limit, filters)
	if err != nil {
		return ReconciliationRunPage{}, err
	}
	rows, err := reads.runs.List(ctx, persistence.RunPage{
		After: parseSequence(after),
		Limit: pageSize,
	})
	if err != nil {
		return ReconciliationRunPage{}, Internal(err)
	}
	items, hasMore, next := PageOf(rows, pageSize, func(r persistence.Run) string {
		return strconv.FormatInt(r.ID, 10)
	}, collectionRuns, filters, after)
	return ReconciliationRunPage{Items: items, HasMore: hasMore, NextCursor: next}, nil
}
