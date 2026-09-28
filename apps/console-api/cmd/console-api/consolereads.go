package main

import (
	"context"
	"errors"
	"time"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/adapters/inbound/http"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/application"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/accounting"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/commerce"
	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/domain/identity"
)

// The account id as each bounded context spells it. Identity mints the
// identifier, so every use case that takes one takes identity's; commerce and
// accounting each carry their own DISTINCT type for the same reference, and
// the conversion is the boundary doing its job rather than a shortcut past it.
//
// The conversion is a cast rather than a parse and deliberately performs no
// validation: the id came out of a session row this same plane wrote, and a
// grammar check on it here would be a second place a signed-in account could
// be turned away from its own data. A string that is not an account id is a
// corruption upstream, and the statement's own predicate answers it as "no
// rows" rather than as a panic on a request goroutine.
// The three id types are named here so the conversions below are checked by the
// compiler rather than by a reader: a cast to a type this file does not import
// would not compile, and that is the whole of the guarantee offered.
var (
	_ identity.AccountID
	_ commerce.AccountID
	_ accounting.AccountID
)

// The one place the console's ten read use cases meet the handlers that call
// them, and it is HERE — at the composition root — because the import rule
// forbids the inbound surface from reaching internal/domain. The wire package
// declares a narrow seam of plain strings and integers precisely so that the
// conversion of the domain's aggregates into wire fields can happen at the one
// place allowed to know about both: the rule's allow-list names cmd, the
// application and the outbound adapters, and an inbound adapter that imported
// a grammar would be handing aggregates to the transport instead of rendering
// its own response shapes.
//
// That is why this file exists rather than a constructor inside the http
// package. A conversion written in the transport would have to import the
// domain to do it, and the rule is the enforcement of the decision the seam
// states: the transport renders shapes, and someone who holds both grammars and
// every wire schema has to do the rendering. It is a wide type on purpose, and
// it is ten methods.
//
// Everything here is mechanical: call, convert, convert back. It computes
// nothing, derives nothing and filters nothing. A balance is copied from the
// bucket it was on; a delta is copied from the leg it was on; the three figures
// of a page are copied from the three the use case resolved. If a value could be
// computed two ways, this file is the wrong place for it.

// consoleReads adapts *application.ConsoleReads to the http package's
// consoleReadUseCases seam.
//
// The seam's TYPE is unexported, which is not a bar to implementing it: Go lets
// any package satisfy an unexported interface as long as every method is
// exported, and a caller cannot name the type but can pass a value that
// implements it. That is exactly how the session surface's own stand-in above
// satisfies sessionUseCases, and this type is the real counterpart to it.
//
// The field is a pointer because that is what the application hands back, and
// because a nil here is caught by newConsoleReads below rather than reaching a
// request.
type consoleReads struct {
	reads *application.ConsoleReads
}

// newConsoleReads returns the transport's view of the console's read use cases.
//
// The nil is refused at this boundary rather than answered around, and the
// reason is the one every constructor in this module has: a half-wired read
// surface should fail where the stack says which screen is missing, not on a
// customer's first request to one of them. The caller's alternative to this
// constructor is the fail-closed stand-in, and the two are deliberately
// different: an unwired read is a missing feature, while a nil here is a
// wiring mistake that would otherwise present as a nil dereference on the floor
// of a request goroutine.
func newConsoleReads(reads *application.ConsoleReads) (http.ConsoleReadUseCases, error) {
	if reads == nil {
		return nil, errConsoleReadsUnwired
	}
	return consoleReads{reads: reads}, nil
}

// errConsoleReadsUnwired is what a nil read use case produces. It is named here
// rather than imported so the composition root's own refusal and the http
// package's are the same sentence a reader finds in one place.
var errConsoleReadsUnwired = errors.New("console-api: the console read use cases are required and were nil")

// AccountOverview renders the composition the application assembled. The two
// counts and the open finding count are the application's figures verbatim; no
// count is recomputed from a list's length, and no list's length is reported
// as a total.
func (c consoleReads) AccountOverview(ctx context.Context, accountID string) (http.AccountOverviewResult, error) {
	overview, err := c.reads.AccountOverview(ctx, identity.AccountID(accountID))
	if err != nil {
		return http.AccountOverviewResult{}, err
	}
	result := http.AccountOverviewResult{
		Account: http.AccountRecord{
			ID:        string(overview.Account.ID),
			Name:      overview.Account.Name,
			State:     string(overview.Account.State),
			CreatedAt: wireTimestamp(overview.Account.CreatedAt),
			UpdatedAt: wireTimestamp(overview.Account.UpdatedAt),
		},
		UserCount:         overview.UserCount,
		ActiveAPIKeyCount: overview.ActiveAPIKeyCount,
		OpenFindingCount:  overview.OpenFindingCount,
		// Non-nil so the response carries an array rather than null: the
		// contract types these as arrays, and `null` is not an array.
		Subscriptions: make([]http.SubscriptionRecord, 0, len(overview.Subscriptions)),
		PAYGBalances:  make([]http.FundingBucketRecord, 0, len(overview.PAYGBalances)),
	}
	for _, subscription := range overview.Subscriptions {
		result.Subscriptions = append(result.Subscriptions, seamSubscription(subscription))
	}
	for _, bucket := range overview.PAYGBalances {
		result.PAYGBalances = append(result.PAYGBalances, seamBucket(bucket))
	}
	return result, nil
}

// ListUsers returns one page of the account's users. The account is the
// session's and is passed as this call's first argument, so it reaches the
// port as a predicate rather than as a filter over rows already fetched.
func (c consoleReads) ListUsers(ctx context.Context, accountID, state, after string, limit int) (http.UserPageResult, error) {
	page, err := c.reads.ListUsers(ctx, identity.AccountID(accountID), state, after, limit)
	if err != nil {
		return http.UserPageResult{}, err
	}
	result := http.UserPageResult{
		HasMore:    page.HasMore,
		NextCursor: page.NextCursor,
		Items:      make([]http.UserRecord, 0, len(page.Items)),
	}
	for _, user := range page.Items {
		result.Items = append(result.Items, http.UserRecord{
			ID:        string(user.ID),
			AccountID: string(user.AccountID),
			Email:     user.Email,
			State:     string(user.State),
			CreatedAt: wireTimestamp(user.CreatedAt),
			UpdatedAt: wireTimestamp(user.UpdatedAt),
		})
	}
	return result, nil
}

// ListAPIKeys returns one page of the account's keys. Every row is an
// ownership record: the credential is not in the type this crosses as, and the
// mint's response is the only place one exists.
func (c consoleReads) ListAPIKeys(ctx context.Context, accountID, after string, limit int) (http.APIKeyPageResult, error) {
	page, err := c.reads.ListAPIKeys(ctx, identity.AccountID(accountID), after, limit)
	if err != nil {
		return http.APIKeyPageResult{}, err
	}
	result := http.APIKeyPageResult{
		HasMore:    page.HasMore,
		NextCursor: page.NextCursor,
		Items:      make([]http.APIKeyRecord, 0, len(page.Items)),
	}
	for _, key := range page.Items {
		result.Items = append(result.Items, http.APIKeyRecord{
			ID:          string(key.ID),
			AccountID:   string(key.AccountID),
			CreatedBy:   string(key.CreatedBy),
			DisplayName: key.DisplayName,
			Prefix:      key.Prefix,
			State:       string(key.State),
			CreatedAt:   wireTimestamp(key.CreatedAt),
			UpdatedAt:   wireTimestamp(key.UpdatedAt),
			// The seam carries the optional instants as empty strings rather
			// than pointers, matching the apiKeyRecord DTO above it: both
			// `omitempty` and an absent column are the same absence, and an
			// absent revoked_at is a key that has not been revoked.
			RevokedAt: optionalTimestamp(key.RevokedAt),
		})
	}
	return result, nil
}

// ListPlans returns one page of the catalogue, which takes no account because a
// plan is what every account buys from.
func (c consoleReads) ListPlans(ctx context.Context, after string, limit int) (http.PlanPageResult, error) {
	page, err := c.reads.ListPlans(ctx, after, limit)
	if err != nil {
		return http.PlanPageResult{}, err
	}
	result := http.PlanPageResult{
		HasMore:    page.HasMore,
		NextCursor: page.NextCursor,
		Items:      make([]http.PlanRecord, 0, len(page.Items)),
	}
	for _, plan := range page.Items {
		result.Items = append(result.Items, http.PlanRecord{
			ID:        string(plan.ID),
			Name:      plan.Name,
			CreatedAt: wireTimestamp(plan.CreatedAt),
			// The Plan schema has no updated_at and the aggregate carries none
			// either, so this is the empty string the DTO omits rather than a
			// zero instant a client would have to recognise.
			UpdatedAt: "",
		})
	}
	return result, nil
}

// ListSubscriptions returns one page of what the account bought. The state and
// the cancellation instant cross separately, because a scheduled cancellation
// is data beside an unchanged state and never a state of its own.
func (c consoleReads) ListSubscriptions(ctx context.Context, accountID, after string, limit int) (http.SubscriptionPageResult, error) {
	page, err := c.reads.ListSubscriptions(ctx, commerce.AccountID(accountID), after, limit)
	if err != nil {
		return http.SubscriptionPageResult{}, err
	}
	result := http.SubscriptionPageResult{
		HasMore:    page.HasMore,
		NextCursor: page.NextCursor,
		Items:      make([]http.SubscriptionRecord, 0, len(page.Items)),
	}
	for _, subscription := range page.Items {
		result.Items = append(result.Items, seamSubscription(subscription))
	}
	return result, nil
}

// ListEntitlements returns one page of the account's grants. No balance, no
// remaining, no available: capacity is read on a bucket, and a third copy of
// this number with no rebuild story is what this read exists to avoid.
func (c consoleReads) ListEntitlements(ctx context.Context, accountID, after string, limit int) (http.EntitlementPageResult, error) {
	page, err := c.reads.ListEntitlements(ctx, commerce.AccountID(accountID), after, limit)
	if err != nil {
		return http.EntitlementPageResult{}, err
	}
	result := http.EntitlementPageResult{
		HasMore:    page.HasMore,
		NextCursor: page.NextCursor,
		Items:      make([]http.EntitlementRecord, 0, len(page.Items)),
	}
	for _, entitlement := range page.Items {
		// The scope is a NAME in the contract and a version id in the domain.
		// The domain stores the resolved version id because that is what the
		// roll had at the time, and the console resolves it through the Data
		// Plane that owns the catalogue — so what crosses here is the row as
		// stored, and the name is whatever the store returned in that column.
		// Inventing a lookup here would be a second place a grant's scope could
		// be resolved, and the two could disagree.
		result.Items = append(result.Items, http.EntitlementRecord{
			ID:                string(entitlement.ID),
			SubscriptionID:    string(entitlement.SubscriptionID),
			GrantDefinitionID: string(entitlement.GrantDefinitionID),
			Cycle:             entitlement.CycleNumber,
			Scope:             string(entitlement.AliasGroupVersionID),
			Dimension:         string(entitlement.Dimension),
			GrantedMinorUnits: entitlement.GrantedAmount,
			ScopeVersionID:    string(entitlement.AliasGroupVersionID),
			State:             string(entitlement.State),
			PeriodStart:       wireTimestamp(entitlement.PeriodStart),
			PeriodEnd:         wireTimestamp(entitlement.PeriodEnd),
			CreatedAt:         wireTimestamp(entitlement.CreatedAt),
			UpdatedAt:         wireTimestamp(entitlement.UpdatedAt),
		})
	}
	return result, nil
}

// ListFundingBuckets returns one page of the account's buckets, their three
// balances as stored. Nothing here sums them, derives `available` from the
// other two, or converts a count of this page into a total.
func (c consoleReads) ListFundingBuckets(ctx context.Context, accountID, after string, limit int) (http.FundingBucketPageResult, error) {
	page, err := c.reads.ListFundingBuckets(ctx, accounting.AccountID(accountID), after, limit)
	if err != nil {
		return http.FundingBucketPageResult{}, err
	}
	result := http.FundingBucketPageResult{
		HasMore:    page.HasMore,
		NextCursor: page.NextCursor,
		Items:      make([]http.FundingBucketRecord, 0, len(page.Items)),
	}
	for _, bucket := range page.Items {
		result.Items = append(result.Items, seamBucket(bucket))
	}
	return result, nil
}

// ListLedgerEntries returns one page of one bucket's legs.
//
// The account and the bucket cross together, which is the whole of the 404
// rule: the port takes the account as its first argument and the bucket inside
// the same statement, so a bucket the session's account does not own is a row
// that statement did not return — the same empty page, at the same cost, as a
// bucket that does not exist. This layer never compares the two ids to decide
// that; a comparison here would be a second authorization rule, and it would be
// one that could disagree with the one in the WHERE clause.
func (c consoleReads) ListLedgerEntries(ctx context.Context, accountID, bucketID, kind, after string, limit int) (http.LedgerEntryPageResult, error) {
	page, err := c.reads.ListLedgerEntries(ctx, accounting.AccountID(accountID),
		accounting.FundingBucketID(bucketID), kind, after, limit)
	if err != nil {
		return http.LedgerEntryPageResult{}, err
	}
	result := http.LedgerEntryPageResult{
		HasMore:    page.HasMore,
		NextCursor: page.NextCursor,
		Items:      make([]http.LedgerEntryRecord, 0, len(page.Items)),
	}
	for _, entry := range page.Items {
		leg := http.LedgerEntryRecord{
			ID:              string(entry.ID),
			FundingBucketID: string(entry.FundingBucketID),
			Kind:            string(entry.Kind),
			Sequence:        entry.Sequence,
			// Both deltas keep their sign. Sign is meaningful on a delta and
			// never on a balance, and no arithmetic happens to either.
			SettledDelta:     entry.SettledDelta.Int64(),
			HeldDelta:        entry.HeldDelta.Int64(),
			SettlementID:     wireStringPtr(string(entry.SettlementID)),
			ReservationID:    wireStringPtr(string(entry.ReservationID)),
			CommandKey:       wireStringPtr(string(entry.CommandKey)),
			AdjustmentReason: wireStringPtr(entry.AdjustmentReason),
			OperatorID:       wireStringPtr(string(entry.OperatorID)),
			CreatedAt:        wireTimestamp(entry.CreatedAt),
		}
		if entry.Price != nil {
			leg.Price = &http.PriceSnapshotRecord{
				RevisionID:      string(entry.Price.RevisionID),
				InputUnitPrice:  entry.Price.InputUnitPrice.Int64(),
				OutputUnitPrice: entry.Price.OutputUnitPrice.Int64(),
			}
		}
		result.Items = append(result.Items, leg)
	}
	return result, nil
}

// ListFindings returns one page of the recorded divergences, whole-plane.
//
// The evidence crosses as the text the check wrote. It is not parsed, not
// decoded, not walked and not re-encoded here: the only thing this layer does
// with it is verify that it is JSON, because rendering a field the contract
// types as an object and writing something that is not one would be a body no
// client can parse. Everything past that is the check's shape and not this
// surface's to know.
func (c consoleReads) ListFindings(ctx context.Context, status, severity, after string, limit int) (http.FindingPageResult, error) {
	page, err := c.reads.ListFindings(ctx, status, severity, after, limit)
	if err != nil {
		return http.FindingPageResult{}, err
	}
	result := http.FindingPageResult{
		HasMore:    page.HasMore,
		NextCursor: page.NextCursor,
		Items:      make([]http.FindingRecord, 0, len(page.Items)),
	}
	for _, finding := range page.Items {
		result.Items = append(result.Items, http.FindingRecord{
			ID:          finding.ID,
			CheckKind:   finding.CheckKind,
			SubjectKind: finding.SubjectKind,
			SubjectID:   finding.SubjectID,
			Severity:    finding.Severity,
			Status:      finding.Status,
			Observed:    string(finding.Observed),
			Detail:      finding.Detail,
			DetectedAt:  wireTimestamp(finding.DetectedAt),
			LastSeenAt:  wireTimestamp(finding.LastSeenAt),
			ResolvedAt:  wireTimestampPtr(finding.ResolvedAt),
		})
	}
	return result, nil
}

// ListReconciliationRuns returns one page of the pass history, whole-plane. A
// pass with no finished_at crosses as a nil and renders as null, which is the
// only thing that distinguishes a wedged worker from an idle one.
func (c consoleReads) ListReconciliationRuns(ctx context.Context, after string, limit int) (http.ReconciliationRunPageResult, error) {
	page, err := c.reads.ListReconciliationRuns(ctx, after, limit)
	if err != nil {
		return http.ReconciliationRunPageResult{}, err
	}
	result := http.ReconciliationRunPageResult{
		HasMore:    page.HasMore,
		NextCursor: page.NextCursor,
		Items:      make([]http.ReconciliationRunRecord, 0, len(page.Items)),
	}
	for _, run := range page.Items {
		result.Items = append(result.Items, http.ReconciliationRunRecord{
			ID:                run.ID,
			Scope:             run.Scope,
			Status:            run.Status,
			WindowFrom:        wireTimestamp(run.WindowFrom),
			WindowTo:          wireTimestamp(run.WindowTo),
			StartedAt:         wireTimestamp(run.StartedAt),
			FinishedAt:        wireTimestampPtr(run.FinishedAt),
			BucketsScanned:    run.BucketsScanned,
			FindingsOpened:    run.FindingsOpened,
			FindingsUnchanged: run.FindingsUnchanged,
		})
	}
	return result, nil
}

// seamSubscription converts one subscription aggregate. It is a function
// because the list and the dashboard's bounded list render the same row, and
// two conversions of one row is one of them eventually missing a field.
func seamSubscription(subscription commerce.Subscription) http.SubscriptionRecord {
	return http.SubscriptionRecord{
		ID:                 string(subscription.ID),
		AccountID:          string(subscription.AccountID),
		PlanVersionID:      string(subscription.PlanVersionID),
		State:              string(subscription.State),
		StartsAt:           wireTimestamp(subscription.StartAt),
		CurrentPeriodStart: wireTimestampPtr(subscription.PeriodStart),
		CurrentPeriodEnd:   wireTimestampPtr(subscription.PeriodEnd),
		CancelAt:           wireTimestampPtr(subscription.CancelAt),
		CreatedAt:          wireTimestamp(subscription.CreatedAt),
		UpdatedAt:          wireTimestamp(subscription.UpdatedAt),
	}
}

// seamBucket converts one bucket aggregate, its three balances copied as
// stored. The two owner references cross as the two nullable pointers the
// schema makes nullable: exactly one is set, and which one is a fact about the
// row rather than something this conversion decides.
func seamBucket(bucket accounting.Bucket) http.FundingBucketRecord {
	kind := "account"
	if bucket.EntitlementID != "" {
		kind = "entitlement"
	}
	return http.FundingBucketRecord{
		ID:            string(bucket.ID),
		Kind:          kind,
		EntitlementID: wireStringPtr(string(bucket.EntitlementID)),
		AccountID:     wireStringPtr(string(bucket.AccountID)),
		Status:        string(bucket.Status),
		// Copied, not derived. `available` is the store's own figure: a client
		// computing it from the other two would be doing the ledger's algebra
		// in the browser, and the legs win over this cache anyway.
		Settled:   bucket.Settled.Int64(),
		Held:      bucket.Held.Int64(),
		Available: bucket.Available.Int64(),
		Version:   bucket.Version,
		OpenedAt:  nil,
		ClosedAt:  nil,
		CreatedAt: wireTimestamp(bucket.CreatedAt),
		UpdatedAt: wireTimestamp(bucket.UpdatedAt),
	}
}

// wireTimestamp renders an instant as the contract's `date-time`: RFC 3339 in
// UTC, which is the form every timestamp on this surface is in and the one the
// store wrote it in.
//
// The zero instant is an ABSENCE, not a date. A row that carries no such column
// arrives as a nil pointer and renders as null; a present column that happens to
// hold the zero time does not exist in these tables, and rendering it as
// 0001-01-01 would be a date nobody wrote.
func wireTimestamp(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339Nano)
}

// wireTimestampPtr renders a present instant and an absent one. The contract
// types these fields as `[string, "null"]` throughout — a scheduled
// cancellation, a closed bucket, a settled run — so absent is a value on the
// wire and never an empty string a client would have to guess about.
func wireTimestampPtr(at *time.Time) *string {
	if at == nil {
		return nil
	}
	rendered := wireTimestamp(*at)
	return &rendered
}

// optionalTimestamp renders an absent instant as the empty string, which both
// this seam and the apiKeyRecord DTO omit. It is the same absence wireTimestampPtr
// renders as null, expressed the way THIS DTO spells it — and it is a separate
// function rather than a shared one precisely because the two spellings are not
// interchangeable: `omitempty` drops the empty string from the body, and a null
// would render the field.
func optionalTimestamp(at *time.Time) string {
	if at == nil {
		return ""
	}
	return wireTimestamp(*at)
}

// wireStringPtr is the nullable string's absence: an empty string is not an
// absent id, so an id that was never set crosses as null rather than as "".
func wireStringPtr(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
