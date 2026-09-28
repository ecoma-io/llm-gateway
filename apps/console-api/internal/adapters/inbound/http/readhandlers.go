package http

import (
	"encoding/json"
	stdhttp "net/http"
	"strconv"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/application"
)

// The ten read operations, as handlers over the narrow seam below.
//
// Every handler here is the same four steps, and each of them is one of the
// rules this surface is built to hold rather than a habit worth restating at
// each call site:
//
//   - resolveSession first. The account comes from the Principal it returns and
//     from nowhere else — no path segment, no query parameter, no body field —
//     and a caller with no live session never reaches the use case at all.
//
//   - read the query parameters and the one path parameter, and refuse what the
//     contract refuses. Every refusal is the same envelope, written by
//     writeError, from a typed *application.Error the use case already knows
//     how to describe. No handler derives a status from a number, and no
//     handler clamps a limit it cannot honour: an out-of-range `limit` is 400
//     invalid_request, because a page the caller did not ask for is one it
//     cannot tell apart from a page the collection capped itself.
//
//   - call the use case with the session's account. For the eight account-scoped
//     reads that predicate is the port's first argument, so another account's
//     row is a row the statement never returned; for the ledger it rides inside
//     the same statement as the bucket the path named, so a bucket the session's
//     account does not own answers exactly as a bucket that does not exist.
//
//   - render what came back. No totals, no balances computed, no currencies, no
//     derived anything. The DTOs in readwire.go are the contract's schemas and
//     a conversion per row; the arithmetic of this plane happened in the
//     application, where a disagreement with it has authority behind it.

// The query parameter names, named once because all nine list-shaped reads
// page with the same two and a filter's name is part of the contract. The
// after parameter's value is opaque and is passed through untouched: this
// transport never decodes a cursor, never compares one and never synthesises
// one — application.DecodeCursor is the only thing that reads a cursor, and it
// refuses one it cannot place.
const (
	pageAfterParam = "after"
	pageLimitParam = "limit"

	usersStateParam        = "state"
	ledgerKindParam        = "kind"
	findingsStatusParam    = "status"
	findingsSeverityParam  = "severity"
	fundingBucketPathParam = "funding_bucket_id"
)

// pageRequest is the paging ask a list handler forwards, parsed from the query
// and nothing else.
//
// The zero Limit is the contract's own documented default, not a demand for no
// rows: ResolvePage resolves it against the persistence layer's own bound
// constants, so the default and the bounds are stated in one place and the
// SQL layer cannot disagree with this surface about where they are.
type pageRequest struct {
	After string
	Limit int
}

// paging reads the two parameters every list on this surface pages with.
//
// A limit that is not an integer is a refusal rather than a default, for the
// same reason an out-of-range one is: a caller that sent `limit=abc` did not
// ask for fifty rows, it sent a value this operation does not read, and
// answering it as though it had said nothing would make a malformed request
// indistinguishable from one that said nothing at all.
func paging(r *stdhttp.Request) (pageRequest, error) {
	query := r.URL.Query()
	ask := pageRequest{After: query.Get(pageAfterParam)}

	raw := query.Get(pageLimitParam)
	if raw == "" {
		// Absent, or present and empty: both are the documented default. The
		// distinction is not one the contract draws, and a client sending an
		// empty `limit=` is a client that sent no limit.
		return ask, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil {
		return pageRequest{}, application.InvalidRequest("the limit parameter is not a whole number of rows")
	}
	ask.Limit = limit
	return ask, nil
}

// optionalQuery reads one optional filter value. An absent filter and an
// empty-valued one are the same request — the list that answers it is the same
// list either way — so neither is distinguished here, and the application
// refuses a value outside the filter's own vocabulary.
func optionalQuery(r *stdhttp.Request, name string) string {
	return r.URL.Query().Get(name)
}

// handleGetAccountOverview is GET /account/overview: the dashboard's one
// composed answer.
//
// The composition happened in the application, across three bounded contexts,
// in one use case with one account scope; this handler renders it. There is no
// arithmetic here of any kind — no sum over the subscription list, no balance
// computed from the buckets, no count derived from a page — and that is the
// point of the operation existing rather than a client making three requests
// and joining them in the browser.
func handleGetAccountOverview(sessions SessionUseCases, reads ConsoleReadUseCases) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		sessionPrincipal, ok := resolveSession(w, r, sessions)
		if !ok {
			return
		}
		overview, err := reads.AccountOverview(r.Context(), accountOf(sessionPrincipal))
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, stdhttp.StatusOK, renderAccountOverview(overview))
	}
}

// handleListUsers is GET /users: the account's console users, keyset-paged.
//
// The state filter is optional and omitting it returns invited and active
// together, which is what a member list is. The account is the session's and
// the port's predicate, so a user of another account is a row the statement
// never returned.
func handleListUsers(sessions SessionUseCases, reads ConsoleReadUseCases) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		sessionPrincipal, ok := resolveSession(w, r, sessions)
		if !ok {
			return
		}
		ask, err := paging(r)
		if err != nil {
			writeError(w, r, err)
			return
		}
		result, err := reads.ListUsers(r.Context(), accountOf(sessionPrincipal), optionalQuery(r, usersStateParam), ask.After, ask.Limit)
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, stdhttp.StatusOK, newPage(result.Items, result.HasMore, result.NextCursor, renderUser))
	}
}

// handleListAPIKeys is GET /api-keys: the account's keys, keyset-paged.
//
// Unfiltered, and the page is the only thing a caller learns: there is no total
// here, so a console that wanted "12 keys" is asking the overview, whose
// active_api_key_count is a stored figure — not this list, whose page is not.
func handleListAPIKeys(sessions SessionUseCases, reads ConsoleReadUseCases) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		sessionPrincipal, ok := resolveSession(w, r, sessions)
		if !ok {
			return
		}
		ask, err := paging(r)
		if err != nil {
			writeError(w, r, err)
			return
		}
		result, err := reads.ListAPIKeys(r.Context(), accountOf(sessionPrincipal), ask.After, ask.Limit)
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, stdhttp.StatusOK, newPage(result.Items, result.HasMore, result.NextCursor, renderAPIKey))
	}
}

// handleListPlans is GET /plans: the plan catalogue.
//
// The one list on this surface with no account predicate, and that is the
// contract's statement rather than an omission — a plan is what every account
// buys from — so this handler does not resolve an account into the call beyond
// the session that reached it at all. A session is still required: the
// catalogue is a product surface read, and nothing here is reachable without
// one.
func handleListPlans(sessions SessionUseCases, reads ConsoleReadUseCases) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		if _, ok := resolveSession(w, r, sessions); !ok {
			return
		}
		ask, err := paging(r)
		if err != nil {
			writeError(w, r, err)
			return
		}
		result, err := reads.ListPlans(r.Context(), ask.After, ask.Limit)
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, stdhttp.StatusOK, newPage(result.Items, result.HasMore, result.NextCursor, renderPlan))
	}
}

// handleListSubscriptions is GET /subscriptions: the account's subscriptions.
//
// A scheduled cancellation renders as a cancel_at beside an unchanged state, so
// a row carrying a future cancellation reads as active with a date attached —
// which is what it is.
func handleListSubscriptions(sessions SessionUseCases, reads ConsoleReadUseCases) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		sessionPrincipal, ok := resolveSession(w, r, sessions)
		if !ok {
			return
		}
		ask, err := paging(r)
		if err != nil {
			writeError(w, r, err)
			return
		}
		result, err := reads.ListSubscriptions(r.Context(), accountOf(sessionPrincipal), ask.After, ask.Limit)
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, stdhttp.StatusOK, newPage(result.Items, result.HasMore, result.NextCursor, renderSubscription))
	}
}

// handleListEntitlements is GET /entitlements: the grants the account's
// subscriptions materialised.
//
// No remaining and no available: an entitlement is a grant, and what remains of
// it is drawn on a funding bucket, which is a separate operation with a
// separate reason. The port's predicate is an EXISTS against the owning
// subscription, still in the WHERE clause and still before any row is returned.
func handleListEntitlements(sessions SessionUseCases, reads ConsoleReadUseCases) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		sessionPrincipal, ok := resolveSession(w, r, sessions)
		if !ok {
			return
		}
		ask, err := paging(r)
		if err != nil {
			writeError(w, r, err)
			return
		}
		result, err := reads.ListEntitlements(r.Context(), accountOf(sessionPrincipal), ask.After, ask.Limit)
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, stdhttp.StatusOK, newPage(result.Items, result.HasMore, result.NextCursor, renderEntitlement))
	}
}

// handleListFundingBuckets is GET /funding-buckets: the account's buckets with
// their three cached balances.
//
// The balances are rendered as stored. Nothing here derives `available` from
// `settled` and `held`, and nothing sums a page: the three are a projection the
// server maintains, the legs win over them, and a browser's arithmetic has no
// authority behind it. There is no currency field on any of them and no symbol
// on any of them — see readwire.go.
func handleListFundingBuckets(sessions SessionUseCases, reads ConsoleReadUseCases) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		sessionPrincipal, ok := resolveSession(w, r, sessions)
		if !ok {
			return
		}
		ask, err := paging(r)
		if err != nil {
			writeError(w, r, err)
			return
		}
		result, err := reads.ListFundingBuckets(r.Context(), accountOf(sessionPrincipal), ask.After, ask.Limit)
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, stdhttp.StatusOK, newPage(result.Items, result.HasMore, result.NextCursor, renderFundingBucket))
	}
}

// handleListLedgerEntries is GET /funding-buckets/{funding_bucket_id}/ledger:
// one bucket's legs, oldest first.
//
// This is the only path parameter on the whole surface and it names a bucket,
// never an account. It reaches the application beside the account — the two
// are the same call, and the account is the port's predicate and the bucket
// sits inside the same statement — so a bucket the session's account does not
// own is a bucket the query did not return. The use case answers that with an
// empty page and the cursor that resumes the walk, which is what a bucket that
// does not exist answers with too: 404 is the route's own answer, not the
// ledger's, because an empty ledger is a legitimate page and a page is not an
// absence.
//
// The kind filter is a real predicate in the same statement, and the cursor
// carries it and the bucket id, so a cursor earned on one bucket or under one
// kind cannot be replayed against another.
func handleListLedgerEntries(sessions SessionUseCases, reads ConsoleReadUseCases) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		sessionPrincipal, ok := resolveSession(w, r, sessions)
		if !ok {
			return
		}
		bucketID := r.PathValue(fundingBucketPathParam)
		if bucketID == "" {
			// The pattern requires the segment, so an empty one cannot arrive
			// through New. It is refused rather than passed on because the
			// application is entitled to trust that a handler forwards a
			// parameter that exists — and an empty bucket id is a resource the
			// caller's account owns, which is a claim this handler cannot make.
			writeError(w, r, application.InvalidRequest("the funding_bucket_id path parameter is required"))
			return
		}
		ask, err := paging(r)
		if err != nil {
			writeError(w, r, err)
			return
		}
		result, err := reads.ListLedgerEntries(r.Context(), accountOf(sessionPrincipal), bucketID,
			optionalQuery(r, ledgerKindParam), ask.After, ask.Limit)
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, stdhttp.StatusOK, newPage(result.Items, result.HasMore, result.NextCursor, renderLedgerEntry))
	}
}

// handleListFindings is GET /reconciliation/findings: the recorded divergences,
// newest first.
//
// Whole-plane for the reason the contract says in those words: a finding's
// subject may be a bucket, a settlement, a request, or the literal
// control_plane, and findings are not scoped to one account. A session is still
// required — this is a product read and nothing here is reachable without one —
// and the account is not consulted, because a count of it would be a
// projection the list does not offer.
func handleListFindings(sessions SessionUseCases, reads ConsoleReadUseCases) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		if _, ok := resolveSession(w, r, sessions); !ok {
			return
		}
		ask, err := paging(r)
		if err != nil {
			writeError(w, r, err)
			return
		}
		result, err := reads.ListFindings(r.Context(), optionalQuery(r, findingsStatusParam),
			optionalQuery(r, findingsSeverityParam), ask.After, ask.Limit)
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, stdhttp.StatusOK, newPage(result.Items, result.HasMore, result.NextCursor, renderFinding))
	}
}

// handleListReconciliationRuns is GET /reconciliation/runs: the pass history,
// newest first.
//
// A run whose finished_at is null is rendered as such: a wedged worker and an
// idle one are otherwise indistinguishable, and the contract says rendering
// that is worth the trouble.
func handleListReconciliationRuns(sessions SessionUseCases, reads ConsoleReadUseCases) stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		if _, ok := resolveSession(w, r, sessions); !ok {
			return
		}
		ask, err := paging(r)
		if err != nil {
			writeError(w, r, err)
			return
		}
		result, err := reads.ListReconciliationRuns(r.Context(), ask.After, ask.Limit)
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, stdhttp.StatusOK, newPage(result.Items, result.HasMore, result.NextCursor, renderReconciliationRun))
	}
}

// accountOf is the one place a session Principal becomes the account id a
// product read is scoped to. It is a function rather than a field read at each
// call site so the rule is stated once: the account comes from the session and
// from nowhere else, and a handler that wanted a different account would have
// to go through here to get one.
func accountOf(p Principal) string { return p.AccountID }

// The render conversions. Each is the seam's row as the contract's schema, field
// for field, and each is a conversion rather than a struct literal for the
// reason sessionHandlers.go's are: the seam's row and the DTO carry the same
// values under the same names, and a literal restating them is a second place
// to get one wrong — a field added on one side and forgotten on the other
// compiles and is silently zero on the wire.

// renderAPIKey converts a key's ownership record from the seam's row into the
// wire DTO. The credential is absent from both sides of this conversion and that
// is not an oversight: the mint's DTO is the only place a token exists, and a
// list that could return one would have made "shown once" a property of the
// client rather than the system.
//
// It is the only place the contract's closed `active | revoked` enum is spelled,
// and it now serves the mint as well as the read — the mint's record crosses
// the seam as the same APIKeyRecord, so the two callers convert through one
// function and a third spelling of the state could not be added beside it.
func renderAPIKey(row APIKeyRecord) apiKeyRecord {
	return apiKeyRecord{
		ID:          row.ID,
		AccountID:   row.AccountID,
		CreatedBy:   row.CreatedBy,
		DisplayName: row.DisplayName,
		Prefix:      row.Prefix,
		State:       apiKeyState(row.State),
		CreatedAt:   mustWireTime(row.CreatedAt),
		UpdatedAt:   mustWireTime(row.UpdatedAt),
		RevokedAt:   mustWireTime(row.RevokedAt),
	}
}

// renderUser converts one user row.
func renderUser(row UserRecord) userRecord {
	return userRecord{
		ID:        row.ID,
		AccountID: row.AccountID,
		Email:     row.Email,
		State:     row.State,
		CreatedAt: mustWireTime(row.CreatedAt),
		UpdatedAt: mustWireTime(row.UpdatedAt),
	}
}

// renderPlan converts one catalogue row. No versions are rendered, because the
// contract's Plan schema declares none — see planRecord.
func renderPlan(row PlanRecord) planRecord {
	return planRecord{
		ID:        row.ID,
		Name:      row.Name,
		CreatedAt: mustWireTime(row.CreatedAt),
		UpdatedAt: mustWireTime(row.UpdatedAt),
	}
}

// renderSubscription converts one subscription row. The period and cancellation
// instants are nullable on the contract and nullable here, and an absent one is
// rendered as null rather than as the zero time or an empty string.
func renderSubscription(row SubscriptionRecord) subscriptionRecord {
	return subscriptionRecord{
		ID:                 row.ID,
		AccountID:          row.AccountID,
		PlanVersionID:      row.PlanVersionID,
		State:              row.State,
		StartsAt:           mustWireTime(row.StartsAt),
		CurrentPeriodStart: wireInstants(row.CurrentPeriodStart),
		CurrentPeriodEnd:   wireInstants(row.CurrentPeriodEnd),
		CancelAt:           wireInstants(row.CancelAt),
		CreatedAt:          mustWireTime(row.CreatedAt),
		UpdatedAt:          mustWireTime(row.UpdatedAt),
	}
}

// renderEntitlement converts one grant row. No remaining, no available: the
// contract's Entitlement schema has no such field and this conversion does not
// invent one. Capacity is read on a bucket.
func renderEntitlement(row EntitlementRecord) entitlementRecord {
	return entitlementRecord{
		ID:                row.ID,
		SubscriptionID:    row.SubscriptionID,
		GrantDefinitionID: row.GrantDefinitionID,
		Cycle:             row.Cycle,
		Scope:             row.Scope,
		Dimension:         row.Dimension,
		GrantedMinorUnits: row.GrantedMinorUnits,
		ScopeVersionID:    row.ScopeVersionID,
		State:             row.State,
		PeriodStart:       mustWireTime(row.PeriodStart),
		PeriodEnd:         mustWireTime(row.PeriodEnd),
		CreatedAt:         mustWireTime(row.CreatedAt),
		UpdatedAt:         mustWireTime(row.UpdatedAt),
	}
}

// renderFundingBucket converts one bucket row, its three balances rendered as
// stored. No balance is derived from another, no owner is inferred, and no
// currency is added: the Money schema has no currency field because this
// system's currency is undecided, and a field that named one would be a claim
// this plane cannot make.
func renderFundingBucket(row FundingBucketRecord) fundingBucketRecord {
	return fundingBucketRecord{
		ID:            row.ID,
		Kind:          row.Kind,
		EntitlementID: wireInstants(row.EntitlementID),
		AccountID:     wireInstants(row.AccountID),
		Status:        row.Status,
		Balances: balances{
			Settled:   money{MinorUnits: row.Settled},
			Held:      money{MinorUnits: row.Held},
			Available: money{MinorUnits: row.Available},
		},
		Version:   row.Version,
		OpenedAt:  wireInstants(row.OpenedAt),
		ClosedAt:  wireInstants(row.ClosedAt),
		CreatedAt: mustWireTime(row.CreatedAt),
		UpdatedAt: mustWireTime(row.UpdatedAt),
	}
}

// renderLedgerEntry converts one leg. Both deltas keep their sign — sign is
// meaningful on a delta — and neither is formatted as money. The price snapshot
// is present exactly on a consume leg, and its absence is rendered as null
// rather than as a zero-price snapshot a client could read as "this leg cost
// nothing".
func renderLedgerEntry(row LedgerEntryRecord) ledgerEntryRecord {
	entry := ledgerEntryRecord{
		ID:               row.ID,
		FundingBucketID:  row.FundingBucketID,
		Kind:             row.Kind,
		Sequence:         row.Sequence,
		SettledDelta:     money{MinorUnits: row.SettledDelta},
		HeldDelta:        money{MinorUnits: row.HeldDelta},
		SettlementID:     wireInstants(row.SettlementID),
		ReservationID:    wireInstants(row.ReservationID),
		CommandKey:       wireInstants(row.CommandKey),
		AdjustmentReason: wireInstants(row.AdjustmentReason),
		OperatorID:       wireInstants(row.OperatorID),
		CreatedAt:        mustWireTime(row.CreatedAt),
	}
	if row.Price != nil {
		entry.Price = &priceSnapshot{
			RevisionID:      row.Price.RevisionID,
			InputUnitPrice:  money{MinorUnits: row.Price.InputUnitPrice},
			OutputUnitPrice: money{MinorUnits: row.Price.OutputUnitPrice},
		}
	}
	return entry
}

// renderFinding converts one recorded divergence. The evidence crosses as the
// raw JSON the check wrote — validated once here to prove it IS json, and then
// emitted by the encoder as the bytes it was — and is never interpreted: no key
// is looked up, no figure is summed, no shape is imposed. Its vocabulary is
// that check's business and not this contract's.
//
// json.RawMessage is what makes "as the bytes it was" true rather than
// aspirational: the encoder writes a RawMessage through verbatim, where a
// decoded map[string]any would be re-encoded from Go's map iteration and a
// hand-rolled interface{} would be re-encoded by whatever the column happened
// to parse into. Neither preserves key order, and key order is the only thing
// left that distinguishes two findings recording the same figures.
func renderFinding(row FindingRecord) findingRecord {
	record := findingRecord{
		ID:          row.ID,
		CheckKind:   row.CheckKind,
		SubjectKind: row.SubjectKind,
		SubjectID:   row.SubjectID,
		Severity:    row.Severity,
		Status:      row.Status,
		Detail:      row.Detail,
		DetectedAt:  mustWireTime(row.DetectedAt),
		LastSeenAt:  mustWireTime(row.LastSeenAt),
		ResolvedAt:  wireInstants(row.ResolvedAt),
	}
	if row.Observed != "" {
		// An empty evidence is honest — a singleton check has no figures to
		// compare — and is rendered as an absent field rather than as `{}`, which
		// a client could read as a check that compared two empty things.
		evidence := json.RawMessage(row.Observed)
		if !json.Valid(evidence) {
			panic("console-api http: a finding's observed evidence is not JSON: " + row.Observed)
		}
		record.Observed = evidence
	}
	return record
}

// renderReconciliationRun converts one pass. A pass that started and never
// finished is rendered with a null finished_at, which is the only thing that
// distinguishes a wedged worker from an idle one.
func renderReconciliationRun(row ReconciliationRunRecord) reconciliationRunRecord {
	return reconciliationRunRecord{
		ID:                row.ID,
		Scope:             row.Scope,
		Status:            row.Status,
		WindowFrom:        mustWireTime(row.WindowFrom),
		WindowTo:          mustWireTime(row.WindowTo),
		StartedAt:         mustWireTime(row.StartedAt),
		FinishedAt:        wireInstants(row.FinishedAt),
		BucketsScanned:    row.BucketsScanned,
		FindingsOpened:    row.FindingsOpened,
		FindingsUnchanged: row.FindingsUnchanged,
	}
}

// renderAccountOverview renders the composition the server computed. The two
// counts and the open finding count are the application's figures as they
// stand; the two lists are the rows it bounded. Nothing is added, and nothing
// is derived from a list's length — a count a caller did not ask for is not
// computed here, and the lists are bounded by the use case rather than by a
// total this DTO could carry.
func renderAccountOverview(overview AccountOverviewResult) accountOverviewResponse {
	response := accountOverviewResponse{
		Account: accountRecord{
			ID:        overview.Account.ID,
			Name:      overview.Account.Name,
			State:     overview.Account.State,
			CreatedAt: mustWireTime(overview.Account.CreatedAt),
			UpdatedAt: mustWireTime(overview.Account.UpdatedAt),
		},
		UserCount:         overview.UserCount,
		ActiveAPIKeyCount: overview.ActiveAPIKeyCount,
		Subscriptions:     make([]subscriptionRecord, 0, len(overview.Subscriptions)),
		PAYGBalances:      make([]fundingBucketRecord, 0, len(overview.PAYGBalances)),
		OpenFindingCount:  overview.OpenFindingCount,
	}
	for _, row := range overview.Subscriptions {
		response.Subscriptions = append(response.Subscriptions, renderSubscription(row))
	}
	for _, row := range overview.PAYGBalances {
		response.PAYGBalances = append(response.PAYGBalances, renderFundingBucket(row))
	}
	return response
}
