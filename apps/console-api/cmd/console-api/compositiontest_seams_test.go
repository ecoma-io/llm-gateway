package main

import (
	"context"
	"errors"

	"github.com/ecoma-io/llm-gateway/apps/console-api/internal/adapters/inbound/http"
)

// The two seams a composition test has to supply.
//
// The readiness and version tests drive the process's handler, and they are not
// about the console: /readyz asks the store and /version answers the build
// stamp, and neither reaches a session or a read. The seams are therefore
// filled with stand-ins that REFUSE, which is the honest answer for a handler
// under test with no database behind it — and is a stricter check than a
// success would be, because a probe handler that accidentally depended on a
// console read would answer 500 here and fail the test rather than pass it
// quietly.
//
// They are not the process's fail-closed stand-ins any more. Those were deleted
// when the real wiring landed, and that is the right shape for them: a process
// with a real store must not be able to fall back to refusing.

var errNotWiredIntoThisTest = errors.New("console-api: this test is not exercising the console surface")

// refuseEverySessionUseCase is the session seam's answer in a handler test.
type refuseEverySessionUseCase struct{}

func (refuseEverySessionUseCase) SignIn(context.Context, http.SignInInput) (http.SessionResult, error) {
	return http.SessionResult{}, errNotWiredIntoThisTest
}

func (refuseEverySessionUseCase) Session(context.Context, http.SessionToken) (http.SessionResult, error) {
	return http.SessionResult{}, errNotWiredIntoThisTest
}

func (refuseEverySessionUseCase) SignOut(context.Context, http.SessionToken) error {
	return errNotWiredIntoThisTest
}

func (refuseEverySessionUseCase) MintAPIKey(context.Context, http.MintAPIKeyInput) (http.MintedAPIKeyResult, error) {
	return http.MintedAPIKeyResult{}, errNotWiredIntoThisTest
}

// refuseEveryConsoleRead is the read seam's answer in a handler test, and it
// is one method per read so that a read added to the seam without a stand-in
// here is a COMPILE error rather than a test that passes by not covering it.
type refuseEveryConsoleRead struct{}

func (refuseEveryConsoleRead) AccountOverview(context.Context, string) (http.AccountOverviewResult, error) {
	return http.AccountOverviewResult{}, errNotWiredIntoThisTest
}

func (refuseEveryConsoleRead) ListUsers(context.Context, string, string, string, int) (http.UserPageResult, error) {
	return http.UserPageResult{}, errNotWiredIntoThisTest
}

func (refuseEveryConsoleRead) ListAPIKeys(context.Context, string, string, int) (http.APIKeyPageResult, error) {
	return http.APIKeyPageResult{}, errNotWiredIntoThisTest
}

func (refuseEveryConsoleRead) ListPlans(context.Context, string, int) (http.PlanPageResult, error) {
	return http.PlanPageResult{}, errNotWiredIntoThisTest
}

func (refuseEveryConsoleRead) ListSubscriptions(context.Context, string, string, int) (http.SubscriptionPageResult, error) {
	return http.SubscriptionPageResult{}, errNotWiredIntoThisTest
}

func (refuseEveryConsoleRead) ListEntitlements(context.Context, string, string, int) (http.EntitlementPageResult, error) {
	return http.EntitlementPageResult{}, errNotWiredIntoThisTest
}

func (refuseEveryConsoleRead) ListFundingBuckets(context.Context, string, string, int) (http.FundingBucketPageResult, error) {
	return http.FundingBucketPageResult{}, errNotWiredIntoThisTest
}

func (refuseEveryConsoleRead) ListLedgerEntries(context.Context, string, string, string, string, int) (http.LedgerEntryPageResult, error) {
	return http.LedgerEntryPageResult{}, errNotWiredIntoThisTest
}

func (refuseEveryConsoleRead) ListFindings(context.Context, string, string, string, int) (http.FindingPageResult, error) {
	return http.FindingPageResult{}, errNotWiredIntoThisTest
}

func (refuseEveryConsoleRead) ListReconciliationRuns(context.Context, string, int) (http.ReconciliationRunPageResult, error) {
	return http.ReconciliationRunPageResult{}, errNotWiredIntoThisTest
}
