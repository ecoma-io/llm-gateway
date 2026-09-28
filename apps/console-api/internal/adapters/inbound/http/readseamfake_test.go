package http

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	stdhttp "net/http"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/application"
)

// The ten reads' test seam. It lives in a _test.go file beside the tests rather
// than in a non-test file so it can never be reached by cmd/console-api, and it
// is the same constructor contract_test.go's route table needed for the second
// seam: a fake of ConsoleReadUseCases needs no port, no connection string and no
// cluster, so the route inventory and the contract comparison stay unit tests.
//
// Every use case records the call it received and returns a configured answer,
// so a test can assert both what a handler asked for — the account id came from
// the session, the bucket came from the path, the limit came from the query —
// and what it answered. The fake is the only place a cross-account answer can
// be produced without a database, so it carries an account predicate of its own:
// see refuseForeignAccounts, which is the cross-account refusal every
// account-scoped read is tested against.
type fakeConsoleReadUseCases struct {
	mu sync.Mutex

	// The account this fake treats as the caller's own. A handler is expected to
	// pass exactly this — the session's account — and a test that sets it
	// differently is testing that the handler took the account from somewhere
	// else.
	sessionAccountID string

	// The buckets this fake treats as the caller's own, for the ledger. A
	// request naming any other bucket is answered as not found.
	ownedBuckets map[string]bool

	// err, when set, is what every read answers with. The default is nil, so a
	// test that does not care gets successful pages.
	err error

	// The per-use-case answers. A nil page means the fake's default, which is
	// one row — enough for a happy path, and set explicitly by a test that is
	// about a different fact.
	overview      *AccountOverviewResult
	users         *UserPageResult
	keys          *APIKeyPageResult
	plans         *PlanPageResult
	subscriptions *SubscriptionPageResult
	entitlements  *EntitlementPageResult
	buckets       *FundingBucketPageResult
	ledger        *LedgerEntryPageResult
	findings      *FindingPageResult
	runs          *ReconciliationRunPageResult

	// The calls each use case received, for a test to assert on. The account is
	// kept as its own field on the list calls, because "the account came from
	// the session" is the claim most of these tests exist to make.
	overviewCalls []string
	userCalls     []readCall
	keyCalls      []readCall
	planCalls     []pageRequest
	subCalls      []readCall
	entCalls      []readCall
	bucketCalls   []readCall
	ledgerCalls   []ledgerCall
	findingCalls  []findingsCall
	runCalls      []pageRequest
}

// readCall is one account-scoped list call as it arrived: whose account, which
// filter, and which page.
type readCall struct {
	AccountID string
	Filter    string
	Page      pageRequest
}

// ledgerCall is the ledger's call, which is the one on the surface carrying two
// identifiers: the session's account and the bucket the path named. Both are
// recorded together because the 404 rule is about the pair.
type ledgerCall struct {
	AccountID string
	BucketID  string
	Kind      string
	Page      pageRequest
}

// findingsCall is the findings' call, which is whole-plane and carries two
// optional filters and no account.
type findingsCall struct {
	Status   string
	Severity string
	Page     pageRequest
}

// refuseForeignAccounts makes the fake enforce the account predicate the SQL
// layer enforces in its WHERE clause, so a test can drive the cross-account
// refusal without a database and the handler above it is held to the same rule
// the real statement holds it to.
//
// A bucket is answered NotFound rather than Forbidden, and the distinction is
// the point: a 403 would confirm the bucket exists, so "not yours" and "not
// there" have to be the same answer. The error is a plain not_found envelope
// through the application's own vocabulary, which is what the transport maps.
func refuseForeignAccounts(accountID, sessionAccountID string) error {
	if accountID == "" || accountID == sessionAccountID {
		return nil
	}
	return notFoundForForeignAccount()
}

func newFakeConsoleReadUseCases() *fakeConsoleReadUseCases {
	return &fakeConsoleReadUseCases{
		sessionAccountID: "11111111-1111-4111-8111-111111111111",
		ownedBuckets:     map[string]bool{"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa": true},
	}
}

// newFakeSessionWithReadUseCases returns a handler whose reads are this fake, so
// a test can assert the account id the handler forwarded. The session behind it
// is the live one: the reads' account comes from the session, and a test that
// wanted the two to disagree would set its own Principal.
func newFakeSessionWithReadUseCases(reads *fakeConsoleReadUseCases) stdhttp.Handler {
	return New(application.New("test"), &answeringPinger{}, newFakeSessionUseCases(), reads)
}

func (f *fakeConsoleReadUseCases) AccountOverview(_ context.Context, accountID string) (AccountOverviewResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.overviewCalls = append(f.overviewCalls, accountID)
	if f.err != nil {
		return AccountOverviewResult{}, f.err
	}
	if f.overview != nil {
		return *f.overview, nil
	}
	return AccountOverviewResult{
		Account:           AccountRecord{ID: accountID, Name: "Acme", State: "active", CreatedAt: fixtureTime, UpdatedAt: fixtureTime},
		UserCount:         2,
		ActiveAPIKeyCount: 1,
		OpenFindingCount:  3,
		Subscriptions:     []SubscriptionRecord{},
		PAYGBalances:      []FundingBucketRecord{},
	}, nil
}

func (f *fakeConsoleReadUseCases) ListUsers(_ context.Context, accountID, state, after string, limit int) (UserPageResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.userCalls = append(f.userCalls, readCall{AccountID: accountID, Filter: state, Page: pageRequest{After: after, Limit: limit}})
	if f.err != nil {
		return UserPageResult{}, f.err
	}
	if f.users != nil {
		return *f.users, nil
	}
	return UserPageResult{
		Items:      []UserRecord{{ID: "user-1", AccountID: accountID, Email: "a@example.com", State: "active", CreatedAt: fixtureTime, UpdatedAt: fixtureTime}},
		NextCursor: "cursor-users",
	}, nil
}

func (f *fakeConsoleReadUseCases) ListAPIKeys(_ context.Context, accountID, after string, limit int) (APIKeyPageResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keyCalls = append(f.keyCalls, readCall{AccountID: accountID, Page: pageRequest{After: after, Limit: limit}})
	if f.err != nil {
		return APIKeyPageResult{}, f.err
	}
	if f.keys != nil {
		return *f.keys, nil
	}
	return APIKeyPageResult{
		Items:      []APIKeyRecord{{ID: "key-1", AccountID: accountID, DisplayName: "ci", Prefix: "gw_key", State: "active", CreatedAt: fixtureTime}},
		NextCursor: "cursor-keys",
	}, nil
}

func (f *fakeConsoleReadUseCases) ListPlans(_ context.Context, after string, limit int) (PlanPageResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.planCalls = append(f.planCalls, pageRequest{After: after, Limit: limit})
	if f.err != nil {
		return PlanPageResult{}, f.err
	}
	if f.plans != nil {
		return *f.plans, nil
	}
	return PlanPageResult{
		Items:      []PlanRecord{{ID: "plan-1", Name: "Starter", CreatedAt: fixtureTime}},
		NextCursor: "cursor-plans",
	}, nil
}

func (f *fakeConsoleReadUseCases) ListSubscriptions(_ context.Context, accountID, after string, limit int) (SubscriptionPageResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subCalls = append(f.subCalls, readCall{AccountID: accountID, Page: pageRequest{After: after, Limit: limit}})
	if f.err != nil {
		return SubscriptionPageResult{}, f.err
	}
	if f.subscriptions != nil {
		return *f.subscriptions, nil
	}
	return SubscriptionPageResult{
		Items:      []SubscriptionRecord{{ID: "sub-1", AccountID: accountID, PlanVersionID: "ver-1", State: "active", StartsAt: fixtureTime, CreatedAt: fixtureTime, UpdatedAt: fixtureTime}},
		NextCursor: "cursor-subscriptions",
	}, nil
}

func (f *fakeConsoleReadUseCases) ListEntitlements(_ context.Context, accountID, after string, limit int) (EntitlementPageResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entCalls = append(f.entCalls, readCall{AccountID: accountID, Page: pageRequest{After: after, Limit: limit}})
	if f.err != nil {
		return EntitlementPageResult{}, f.err
	}
	if f.entitlements != nil {
		return *f.entitlements, nil
	}
	return EntitlementPageResult{
		Items:      []EntitlementRecord{{ID: "ent-1", SubscriptionID: "sub-1", Cycle: 1, State: "active", PeriodStart: fixtureTime, PeriodEnd: fixtureTime, CreatedAt: fixtureTime}},
		NextCursor: "cursor-entitlements",
	}, nil
}

func (f *fakeConsoleReadUseCases) ListFundingBuckets(_ context.Context, accountID, after string, limit int) (FundingBucketPageResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bucketCalls = append(f.bucketCalls, readCall{AccountID: accountID, Page: pageRequest{After: after, Limit: limit}})
	if f.err != nil {
		return FundingBucketPageResult{}, f.err
	}
	if f.buckets != nil {
		return *f.buckets, nil
	}
	return FundingBucketPageResult{
		Items:      []FundingBucketRecord{{ID: "bucket-1", Kind: "account", AccountID: &f.sessionAccountID, Status: "active", Settled: 100, Held: 10, Available: 90, Version: 3, CreatedAt: fixtureTime, UpdatedAt: fixtureTime}},
		NextCursor: "cursor-buckets",
	}, nil
}

func (f *fakeConsoleReadUseCases) ListLedgerEntries(_ context.Context, accountID, bucketID, kind, after string, limit int) (LedgerEntryPageResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ledgerCalls = append(f.ledgerCalls, ledgerCall{AccountID: accountID, BucketID: bucketID, Kind: kind, Page: pageRequest{After: after, Limit: limit}})
	if f.err != nil {
		return LedgerEntryPageResult{}, f.err
	}
	// The cross-account rule, enforced where the statement enforces it: a bucket
	// this account does not own answers not_found, which the transport renders
	// as 404. It is never 403 — a 403 would confirm the bucket exists.
	if err := refuseForeignAccounts(accountID, f.sessionAccountID); err != nil {
		return LedgerEntryPageResult{}, err
	}
	if !f.ownedBuckets[bucketID] {
		return LedgerEntryPageResult{}, notFoundForForeignAccount()
	}
	if f.ledger != nil {
		return *f.ledger, nil
	}
	return LedgerEntryPageResult{
		Items: []LedgerEntryRecord{{
			ID: "leg-1", FundingBucketID: bucketID, Kind: "consume", Sequence: 1,
			SettledDelta: -50, HeldDelta: -50, CreatedAt: fixtureTime,
		}},
		NextCursor: "cursor-ledger",
	}, nil
}

func (f *fakeConsoleReadUseCases) ListFindings(_ context.Context, status, severity, after string, limit int) (FindingPageResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.findingCalls = append(f.findingCalls, findingsCall{Status: status, Severity: severity, Page: pageRequest{After: after, Limit: limit}})
	if f.err != nil {
		return FindingPageResult{}, f.err
	}
	if f.findings != nil {
		return *f.findings, nil
	}
	return FindingPageResult{
		Items: []FindingRecord{{
			ID: 7, CheckKind: "f1", SubjectKind: "funding_bucket", SubjectID: "bucket-1",
			Severity: "warning", Status: "open", DetectedAt: fixtureTime, LastSeenAt: fixtureTime,
		}},
		NextCursor: "cursor-findings",
	}, nil
}

func (f *fakeConsoleReadUseCases) ListReconciliationRuns(_ context.Context, after string, limit int) (ReconciliationRunPageResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runCalls = append(f.runCalls, pageRequest{After: after, Limit: limit})
	if f.err != nil {
		return ReconciliationRunPageResult{}, f.err
	}
	if f.runs != nil {
		return *f.runs, nil
	}
	return ReconciliationRunPageResult{
		Items:      []ReconciliationRunRecord{{ID: 1, Scope: "window", Status: "completed", WindowFrom: fixtureTime, WindowTo: fixtureTime, StartedAt: fixtureTime, BucketsScanned: 4, FindingsOpened: 1, FindingsUnchanged: 2}},
		NextCursor: "cursor-runs",
	}, nil
}

// fixtureTime is the instant every fake row is stamped with. A constant rather
// than a computed one so a test asserting a rendered timestamp is asserting a
// literal and not a format of the moment it ran.
const fixtureTime = "2026-09-28T12:00:00Z"

// errNoReads is what the fake returns when a test wants every read to fail, and
// it is the same error for all of them: the reads' refusals are not
// distinguishable from one another either, and a test that needed to tell them
// apart would be asking a question the surface does not answer.
var errNoReads = errors.New("console-api: the read use cases answered nothing")

// notFoundForForeignAccount is the fake's cross-account refusal. It is a 404
// and not a 403, and the reason is the whole of the rule: a 403 confirms the
// resource exists, so "not yours" and "not there" must be one answer.
func notFoundForForeignAccount() error {
	return errors.New("not found: the resource is not available to this account")
}

// jsonBytes is the evidence a finding carries. It is written as a literal here
// so a test can assert it reaches the wire byte-for-byte — a client that
// re-encodes the evidence and gets a different shape has learned something the
// surface did not promise them.
var jsonBytes = json.RawMessage(`{"cached_available":90,"from_legs":90}`)

// ensure the fake satisfies the seam at compile time, so a change to the seam
// cannot leave the fake quietly answering a method nothing calls.
var _ ConsoleReadUseCases = (*fakeConsoleReadUseCases)(nil)
