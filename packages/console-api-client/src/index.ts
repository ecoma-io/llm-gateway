// The package's public surface: a curated re-export of the generated one.
// Consumers see the operations the console actually calls, their option and
// response types, and the shared client instance — never the generator's file
// layout, which is free to change with a pinned version bump. Anything not
// listed here is not API.
//
// `getVersion` is generated and deliberately NOT re-exported. That is not an
// oversight to correct: the operations published here are behaviour this
// package then owes its consumer, and an export nobody imports is a promise
// kept for a caller who does not exist. `/version` remains reachable through
// the generated SDK, so nothing is lost — it becomes public API the day the
// console probes it, which is a one-line change.
//
// Every other generated operation IS re-exported, because the console calls
// all sixteen: the two probes, the three session operations, the
// server-composed overview, the one key-minting write, and the nine paged
// reads. `console.yaml` declares seventeen operationIds, and this file
// re-exports every one of them but the `getVersion` withheld above. The nine
// paged reads are the nine `list*` operations, one per `*Page` schema in
// `shared/console.yaml`; `getAccountOverview` is not among them, because
// `AccountOverview` is a server-composed projection with no `items`,
// `next_cursor` or `has_more` and no reference to `PageEnvelope` at all.
// ADR 0012 §8 scopes the console to exactly the surface `console.yaml`
// declares, so there is no operation here the console may not call and none is
// withheld from it.
//
// The types are held to a different standard on purpose. `HealthStatus` is
// published although only the probes above return it, because a type is the
// contract's vocabulary rather than a behaviour: a consumer that must name a
// shape should be able to name the contract's, not a hand-written copy of it.
// On the same reasoning `Error` is published and no `ApiError` is: the
// generated error envelope and the generated code union are what a consumer
// switches on when it renders a failure, and a hand-written error class would
// be a second vocabulary the contract cannot govern.
export { client } from "./generated/client.gen";
export {
  getAccountOverview,
  getHealth,
  getReadiness,
  getSession,
  listApiKeys,
  listEntitlements,
  listFindings,
  listFundingBuckets,
  listLedgerEntries,
  listPlans,
  listReconciliationRuns,
  listSubscriptions,
  listUsers,
  mintApiKey,
  signIn,
  signOut,
} from "./generated/sdk.gen";
export type {
  Account,
  AccountOverview,
  ApiKey,
  ApiKeyPage,
  Balances,
  ClientOptions,
  Cursor,
  Entitlement,
  EntitlementPage,
  // The error envelope and its code union, published so the console's failure
  // matrix is keyed exhaustively over the CONTRACT's members rather than over a
  // hand-written copy of them (ADR 0012 §6).
  Error,
  ErrorEnvelope,
  Finding,
  FindingPage,
  FundingBucket,
  FundingBucketPage,
  GetAccountOverviewData,
  GetAccountOverviewResponse,
  GetHealthData,
  GetHealthResponse,
  GetReadinessData,
  GetReadinessResponse,
  GetSessionData,
  GetSessionResponse,
  HealthStatus,
  LedgerEntry,
  LedgerEntryPage,
  ListApiKeysData,
  ListApiKeysResponse,
  ListEntitlementsData,
  ListEntitlementsResponse,
  ListFindingsData,
  ListFindingsResponse,
  ListFundingBucketsData,
  ListFundingBucketsResponse,
  ListLedgerEntriesData,
  ListLedgerEntriesResponse,
  ListPlansData,
  ListPlansResponse,
  ListReconciliationRunsData,
  ListReconciliationRunsResponse,
  ListSubscriptionsData,
  ListSubscriptionsResponse,
  ListUsersData,
  ListUsersResponse,
  MintApiKeyData,
  MintApiKeyResponse,
  MintedApiKey,
  Money,
  Options,
  // The envelope every paged read returns, published for the same reason
  // `Error` is above: the console's pager names `next_cursor` and `has_more`,
  // and a hand-written mirror of an envelope the generator owns is a second
  // place a contract change would have to be remembered.
  PageEnvelope,
  Plan,
  PlanPage,
  PriceSnapshot,
  Principal,
  PrincipalClass,
  ReconciliationRun,
  ReconciliationRunPage,
  SignInData,
  SignInRequest,
  SignInResponse,
  SignOutResponse,
  Subscription,
  SubscriptionPage,
  User,
  UserPage,
} from "./generated";
