// The package's public surface: a curated re-export of the generated one.
// Consumers see the operations the console actually calls, their option and
// response types, and the shared client instance — never the generator's file
// layout, which is free to change with a pinned version bump. Anything not
// listed here is not API.
//
// Two generated operations are deliberately NOT re-exported, and neither is
// an oversight to correct: the operations published here are behaviour this
// package then owes its consumer, and an export nobody imports is a promise
// kept for a caller who does not exist.
//
// `getVersion` is withheld on those ordinary terms. `/version` remains
// reachable through the generated SDK, so nothing is lost — it becomes public
// API the day the console probes it, which is a one-line change.
//
// `receiveProviderWebhook` is withheld on stronger ones, and it is the only
// operation in `console.yaml` that is withheld because a console must not
// call it rather than because nothing needs it. It is the payment provider's
// endpoint: the provider's servers call it, it is authenticated by a
// signature over the raw request body and not by a session cookie, and a
// console page's only correct use of it is none. Its body type,
// `WebhookAcknowledgement`, is withheld with it, because a console that could
// name the shape of an answer from that endpoint is a console being told it
// has business there.
//
// Every other generated operation IS re-exported, because the console calls
// all nineteen: the two probes, the three session operations, the
// server-composed overview, the two writes that produce something new — a
// minted credential and an opened payment — the ten paged reads, and the
// top-up price list. `console.yaml` declares twenty-one operationIds, and this
// file re-exports every one of them but the two withheld above. The eleven
// `list*` operations are the ten paged reads — one per `*Page` schema in
// `shared/console.yaml` — plus `listTopUpOffers`, which is deliberately not
// paged and is published all the same: it is what a top-up chooser renders, so
// it is a read the console cannot render a screen without. A cursor names a
// position in a history and a fixed price list has no position to name, which
// is why its schema has no `next_cursor` to offer rather than a page of one.
// `getAccountOverview` is not among the paged reads either, because
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
  createPaymentIntent,
  getAccountOverview,
  getHealth,
  getReadiness,
  getSession,
  listApiKeys,
  listEntitlements,
  listFindings,
  listFundingBuckets,
  listLedgerEntries,
  listPaymentIntents,
  listPlans,
  listReconciliationRuns,
  listSubscriptions,
  listTopUpOffers,
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
  CreatePaymentIntentData,
  CreatePaymentIntentRequest,
  CreatePaymentIntentResponse,
  // The currency code both a price and a payment carry, published as the
  // contract's own named schema rather than as a bare `string`: the console
  // renders one in a chooser and in a receipt, and a hand-written mirror of
  // "three letters, uppercase" is a second place that rule would have to be
  // remembered.
  CurrencyCode,
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
  ListPaymentIntentsData,
  ListPaymentIntentsResponse,
  ListPlansData,
  ListPlansResponse,
  ListReconciliationRunsData,
  ListReconciliationRunsResponse,
  ListSubscriptionsData,
  ListSubscriptionsResponse,
  ListTopUpOffersData,
  ListTopUpOffersResponse,
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
  PaymentIntent,
  PaymentIntentPage,
  PaymentIntentState,
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
  TopUpOffer,
  TopUpOfferList,
  User,
  UserPage,
} from "./generated";
