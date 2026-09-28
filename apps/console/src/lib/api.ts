// The console's ONLY network seam (ADR 0012 §7, AGENTS.md rule 3). Every call
// to the Control Plane API leaves this file, every call goes through the
// generated client, and every failure arrives here already parsed into the
// contract's own `ErrorEnvelope` — the discriminated vocabulary
// `CONSOLE_BEHAVIOUR` is keyed over. A second seam anywhere in the app is a
// second opinion about what a 503 means, and the failure matrix exists to
// guarantee there is exactly one.
//
// Same-origin by default: the browser uses the console's own origin, and the
// session is an `HttpOnly` cookie the browser attaches and the page can never
// read. `credentials: "include"` is therefore not an authentication feature —
// it is what makes the cookie reach the server at all — and no path in this file
// reads, stores or re-sends a credential the page can see.
//
// The one thing the page does read is the double-submit token, and it is not a
// credential: it carries no authority without the session cookie, it is read
// from `document.cookie` and discarded, and it is only ever echoed in a header
// on an unsafe request. See `csrfHeader` below, which is the only place that
// touches it.
// `VITE_API_BASE_URL` remains the optional seam for pointing the console at a
// Control Plane API on another origin; there is deliberately no such seam for
// the runtime (ADR 0006 §4).
import {
  client,
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
  type AccountOverview,
  type ApiKeyPage,
  type EntitlementPage,
  type Error as ApiError,
  type ErrorEnvelope,
  type FindingPage,
  type FundingBucketPage,
  type GetAccountOverviewData,
  type GetSessionResponse,
  type HealthStatus,
  type LedgerEntryPage,
  type ListApiKeysData,
  type ListEntitlementsData,
  type ListFindingsData,
  type ListFundingBucketsData,
  type ListLedgerEntriesData,
  type ListPlansData,
  type ListReconciliationRunsData,
  type ListSubscriptionsData,
  type ListUsersData,
  type MintApiKeyResponse,
  type PlanPage,
  type Principal,
  type ReconciliationRunPage,
  type SignInRequest,
  type SignInResponse,
  type SubscriptionPage,
  type UserPage,
} from "@ecoma-io/llm-gateway-console-api-client";

const apiBaseUrl = import.meta.env.VITE_API_BASE_URL ?? window.location.origin;

client.setConfig({
  baseUrl: apiBaseUrl,
  // The session lives in an `HttpOnly` cookie: the page has no token to put in
  // an `Authorization` header and never will, and the credential is attached by
  // the browser on a same-origin request. `include` is what says so. Nothing
  // here will ever build a signed request to a foreign origin — the cookie's
  // `SameSite=Strict` already refuses to ride one.
  credentials: "include",
  responseStyle: "fields",
  // Failures are RETURNED, never thrown. A `throw new Error(status)` discards
  // the very `code` that decides how the console behaves, and a wrapper that
  // re-derives a code from a status number is the drift this file exists to
  // prevent.
  throwOnError: false,
});

/**
 * How a call that reached the gateway and was refused reads here: the
 * contract's envelope, intact, plus the one fact the envelope does not carry.
 *
 * `unauthenticated` is the 401 stated separately because the shell routes on
 * it — a session that ended is a screen to be sent to, not an error to read —
 * and it must not have to re-derive "401 means ended" for every operation.
 */
export type ApiFailure = {
  readonly kind: "api";
  readonly envelope: ErrorEnvelope;
  readonly unauthenticated: boolean;
};

/**
 * A failure that never reached the contract's vocabulary at all: a fetch that
 * threw, or a 2xx whose body is not what the operation promised.
 *
 * It is a result rather than an exception, and it is deliberately NOT a
 * fabricated `ErrorEnvelope`. Inventing `internal` would show an operator a
 * `request_id` nobody can look up; inventing `upstream_unavailable` would
 * promise a retry against an upstream that was never asked. This arm renders
 * as an honest "try again" — the one thing a caller can do about a body that
 * is not a contract body — and it names no code it was not given.
 */
export type TransportFailure = {
  readonly kind: "transport";
  readonly error: unknown;
};

export type Failure = ApiFailure | TransportFailure;

/**
 * The query a paged read takes, with the generated `url` removed.
 *
 * The generated `*Data` types carry the operation's path as a required field
 * because the SDK needs it to build the URL; a caller of the seam knows the URL
 * already and must not be able to send it somewhere else. Every list wrapper
 * below takes its OWN generated type rather than one shared alias, because the
 * paged reads are not uniform: `listUsers` filters on `state` and the other
 * seven do not, so a single alias derived from any one of them hands the
 * others a parameter their operation does not accept — and a query sent with
 * one is a parameter the server refused with `400 invalid_request` while the
 * type said it was fine. That is AGENTS.md rule 3's drift, arriving through
 * the front door, so each wrapper is typed against the contract's own shape.
 */
export type ListOptions<T> = Omit<T, "url" | "body" | "path">;

/**
 * The ledger's query, and the one path-parameter read in the console's whole
 * surface. `funding_bucket_id` is the only resource id that appears in a URL
 * anywhere here, and it names a bucket — a bucket the session's account does
 * not own is a 404 the query did not return, never a 403 (ADR 0012 §2).
 */
export type LedgerQuery = Omit<ListLedgerEntriesData, "url">;

/**
 * The whole of what a wrapper hands back: the contract's response, or a
 * failure. There is no third case and nothing to catch, so a screen module
 * cannot forget one — and a page cannot be handed a `data` holding
 * `undefined`, which is how a list becomes a blank table on a screen that
 * claims to have loaded.
 */
export type ApiResult<T> =
  { readonly ok: true; readonly data: T } | { readonly ok: false; readonly failure: Failure };

/** Narrows a result to the failure arm, which is what a failure view renders. */
export function isFailure<T>(
  result: ApiResult<T>,
): result is { readonly ok: false; readonly failure: Failure } {
  return !result.ok;
}

/** The behaviour row a failure renders through, or `null` for a transport failure. */
export function failureCode(failure: Failure): ApiError["code"] | null {
  return failure.kind === "api" ? failure.envelope.error.code : null;
}

function isErrorEnvelope(body: unknown): body is ErrorEnvelope {
  if (typeof body !== "object" || body === null) return false;
  const candidate = body as { error?: unknown; request_id?: unknown };
  if (typeof candidate.error !== "object" || candidate.error === null) return false;
  const error = candidate.error as { code?: unknown; message?: unknown };
  return typeof error.code === "string" && typeof error.message === "string";
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/**
 * The status → code fallback, for a failure whose body was not an envelope.
 *
 * Every row is a status the contract names, and each names it in that
 * operation's own error table. The table is small on purpose: the console does
 * not publish a code the contract has not described, because a code invented
 * here is one `CONSOLE_BEHAVIOUR` has no row for and one a reader would then be
 * shown a behaviour nobody wrote. A status the contract does not describe is
 * `internal` — ours, unlooked-up, quote the identifier.
 *
 * Note what is NOT here. `429` is absent because `shared/errors.yaml` declares
 * no code for it, so a throttled response has no code the failure matrix could
 * be keyed by; closing that is a contract change and a new row, not a
 * shorthand here. `503` is `service_unavailable` and not
 * `upstream_unavailable`: those are two different codes on two different
 * statuses, and inferring one from the other is precisely how an outage ends up
 * rendered with the wrong recovery affordance.
 */
function codeForStatus(status: number): ApiError["code"] {
  switch (status) {
    case 400:
      return "invalid_request";
    case 401:
      return "unauthenticated";
    case 404:
      return "not_found";
    case 405:
      return "method_not_allowed";
    case 503:
      return "service_unavailable";
    default:
      return "internal";
  }
}

function envelopeFor(status: number, body: unknown, headerRequestId: string | null): ErrorEnvelope {
  if (isErrorEnvelope(body)) {
    return { error: body.error, request_id: body.request_id };
  }
  return {
    error: {
      code: codeForStatus(status),
      message: "The gateway did not return a contracted error.",
    },
    // The correlation an operator can quote (ADR 0012 §6) is worth showing even
    // when the body that would have carried it did not parse — but an invented
    // identifier is worse than an honest one, so an absent header reads as
    // `unreported` rather than as a string that looks like an id.
    request_id: headerRequestId ?? "unreported",
  };
}

/**
 * Notified whenever any call comes back 401.
 *
 * A session that ended mid-visit is not an error each screen should discover
 * on its own: the shell routes to sign-in, and it needs to hear about the first
 * 401 rather than about whichever page happened to be mounted. The seam is the
 * only place that sees every call, so it is the only place this can be
 * observed from — and it is emitted, not caught: a listener that throws must
 * not be able to turn a 401 into a crash.
 */
type SessionEndedListener = (failure: ApiFailure) => void;

const sessionEndedListeners = new Set<SessionEndedListener>();

export function onSessionEnded(listener: SessionEndedListener): () => void {
  sessionEndedListeners.add(listener);
  return () => {
    sessionEndedListeners.delete(listener);
  };
}

function announceSessionEnded(failure: ApiFailure): void {
  for (const listener of [...sessionEndedListeners]) {
    try {
      listener(failure);
    } catch {
      // A broken listener is the listener's problem. Swallowing here is what
      // keeps an exception in the shell from becoming the console's answer to a
      // request that was merely unauthenticated.
    }
  }
}

/**
 * Runs one generated operation and turns whatever it returns into a result.
 *
 * The generated client parses a non-2xx body as JSON when it can and hands it
 * back in `error` with `throwOnError: false`, so a contract envelope arrives
 * already shaped. The cases it cannot describe are the ones handled here: a
 * fetch that threw, and a 2xx whose body is not the operation's shape (a
 * proxy's HTML error page, an empty body where a document was promised).
 *
 * `emptyBodyOk` is for `signOut`, whose 204 carries no document and whose
 * success is the status rather than a payload.
 */
async function call<Out>(
  operation: () => Promise<unknown>,
  { emptyBodyOk = false }: { emptyBodyOk?: boolean } = {},
): Promise<ApiResult<Out>> {
  let raw: { data?: unknown; error?: unknown; response?: Response };
  try {
    raw = (await operation()) as typeof raw;
  } catch (error) {
    return { ok: false, failure: { kind: "transport", error } };
  }

  const status = raw.response?.status ?? 0;
  const headerRequestId = raw.response?.headers.get("X-Request-Id") ?? null;
  const succeeded = raw.error === undefined && status >= 200 && status < 300;

  if (succeeded) {
    if (emptyBodyOk) {
      return { ok: true, data: undefined as Out };
    }
    if (isRecord(raw.data)) {
      return { ok: true, data: raw.data as Out };
    }
    return { ok: false, failure: { kind: "transport", error: raw.data ?? null } };
  }

  const failure: ApiFailure = {
    kind: "api",
    envelope: envelopeFor(status, raw.error ?? raw.data, headerRequestId),
    unauthenticated: status === 401,
  };
  if (failure.unauthenticated) announceSessionEnded(failure);
  return { ok: false, failure };
}

// ─── the double-submit token ─────────────────────────────────────────────────

/**
 * The cookie the server issues the double-submit token in, and the header the
 * server expects it echoed in. Both names are the SERVER's, spelled here once
 * rather than at each call site, and the pair is the whole of the CSRF defence
 * for an unsafe request.
 *
 * The session cookie is `HttpOnly` and the page can never read it; this cookie
 * is the opposite and must NOT be. The page's script reads this value and puts
 * it in a header, which is what a cross-origin page cannot do: it can cause a
 * request against this origin but it cannot read a cookie scoped to this
 * origin, so it never learns the value it would have to echo. Making this
 * cookie `HttpOnly` would be the fix that breaks sign-in.
 *
 * The token is not a credential. It carries no authority on its own, it is
 * meaningless without the session cookie, and it is not written to storage —
 * `document.cookie` is read and discarded, and the browser keeps it.
 */
const CSRF_COOKIE = "__Host-console_csrf";
const CSRF_HEADER = "X-Console-Csrf";

/** One cookie's value out of `document.cookie`, or `undefined` when it is absent. */
function readCookie(name: string): string | undefined {
  let found: string | undefined;
  for (const part of document.cookie.split(";")) {
    const separator = part.indexOf("=");
    if (separator < 0) continue;
    if (part.slice(0, separator).trim() !== name) continue;
    const value = part.slice(separator + 1).trim();
    // The LAST one wins, and the reason is a browser's not this function's:
    // `document.cookie` lists duplicates most-specific-path first, and the
    // server mints this token at `Path=/` — the least specific there is. So a
    // stale token left at `/` by an earlier session appears FIRST and a parser
    // that took the first match echoes it over the live one, and every unsafe
    // request is refused for a reason nothing logs. The server compares what it
    // reads off the wire against what it minted, so a stale echo is simply a
    // refusal: a sign-in that can never succeed and says so as "those details
    // did not sign anyone in".
    if (value !== "") found = value;
  }
  return found;
}

/**
 * The header carrying the double-submit token, or nothing at all when the page
 * has not been issued one.
 *
 * A MISSING token sends NO header rather than an empty one, and that is the
 * server's own rule: a request with a header and no cookie is refused exactly
 * as firmly as one with a cookie and no header, because a header alone is a
 * value a cross-origin caller chose for itself. Sending `""` would therefore be
 * strictly worse than sending nothing — it would be an attempt, and it would
 * still be refused.
 *
 * It is a header rather than a form field because a form field is submittable
 * from a cross-origin page, and a header set by `fetch()` cannot cross origins
 * without a preflight.
 */
function csrfHeader(): Record<string, string> {
  const token = readCookie(CSRF_COOKIE);
  return token === undefined ? {} : { [CSRF_HEADER]: token };
}

// ─── session ─────────────────────────────────────────────────────────────────

/**
 * Sign in. The response carries the principal and no token: the credential is
 * in a `Set-Cookie` header, and a JSON body cannot set one. Every failure —
 * no such account, no such user, a wrong credential, an `invited` row — is the
 * same 401 by design, so this wrapper distinguishes nothing the contract does
 * not.
 */
export async function signInWith(credentials: SignInRequest): Promise<ApiResult<SignInResponse>> {
  return call<SignInResponse>(() => signIn({ body: credentials, headers: csrfHeader() }));
}

export async function getSessionResult(): Promise<ApiResult<GetSessionResponse>> {
  return call<GetSessionResponse>(() => getSession());
}

/**
 * End the session. Idempotent on the server: 204 whether or not there was
 * anything to end, and nothing to render either way.
 */
export async function signOutOfSession(): Promise<ApiResult<null>> {
  const result = await call<null>(() => signOut({ headers: csrfHeader() }), { emptyBodyOk: true });
  return result.ok ? { ok: true, data: null } : result;
}

// ─── dashboard ───────────────────────────────────────────────────────────────

/** The server-composed dashboard. Its figures are composed server-side and are never arithmetic a page does. */
export async function fetchAccountOverview(
  options: Pick<GetAccountOverviewData, "headers"> = {},
): Promise<ApiResult<AccountOverview>> {
  return call<AccountOverview>(() => getAccountOverview(options));
}

// ─── identity ───────────────────────────────────────────────────────────────

export async function fetchUsers(
  options: ListOptions<ListUsersData> = {},
): Promise<ApiResult<UserPage>> {
  return call<UserPage>(() => listUsers(options));
}

export async function fetchApiKeys(
  options: ListOptions<ListApiKeysData> = {},
): Promise<ApiResult<ApiKeyPage>> {
  return call<ApiKeyPage>(() => listApiKeys(options));
}

/**
 * Mint a key. This response is the one place the plaintext token exists outside
 * the mint itself, so the identity module holds it in a non-reactive ref
 * scoped to the reveal component and clears it on unmount (ADR 0012 §3).
 */
export async function createApiKey(displayName: string): Promise<ApiResult<MintApiKeyResponse>> {
  return call<MintApiKeyResponse>(() =>
    mintApiKey({ body: { display_name: displayName }, headers: csrfHeader() }),
  );
}

// ─── commerce ───────────────────────────────────────────────────────────────

export async function fetchPlans(
  options: ListOptions<ListPlansData> = {},
): Promise<ApiResult<PlanPage>> {
  return call<PlanPage>(() => listPlans(options));
}

export async function fetchSubscriptions(
  options: ListOptions<ListSubscriptionsData> = {},
): Promise<ApiResult<SubscriptionPage>> {
  return call<SubscriptionPage>(() => listSubscriptions(options));
}

export async function fetchEntitlements(
  options: ListOptions<ListEntitlementsData> = {},
): Promise<ApiResult<EntitlementPage>> {
  return call<EntitlementPage>(() => listEntitlements(options));
}

// ─── accounting ─────────────────────────────────────────────────────────────

export async function fetchFundingBuckets(
  options: ListOptions<ListFundingBucketsData> = {},
): Promise<ApiResult<FundingBucketPage>> {
  return call<FundingBucketPage>(() => listFundingBuckets(options));
}

/** One bucket's legs. Per bucket and never merged: the ledger is a fact about where money went. */
export async function fetchLedgerEntries(
  options: LedgerQuery,
): Promise<ApiResult<LedgerEntryPage>> {
  return call<LedgerEntryPage>(() => listLedgerEntries(options));
}

// ─── reconciliation ─────────────────────────────────────────────────────────

export async function fetchFindings(
  options: ListOptions<ListFindingsData> = {},
): Promise<ApiResult<FindingPage>> {
  return call<FindingPage>(() => listFindings(options));
}

export async function fetchReconciliationRuns(
  options: ListOptions<ListReconciliationRunsData> = {},
): Promise<ApiResult<ReconciliationRunPage>> {
  return call<ReconciliationRunPage>(() => listReconciliationRuns(options));
}

// ─── probes ─────────────────────────────────────────────────────────────────

// The two unauthenticated probes the status page already owns, re-exported
// through the seam rather than imported from the package directly, so the
// "one file touches the network" rule is a single grep and not two. They are
// re-exported as the generated operations rather than wrapped, because the
// existing page reads their raw `{ data, error }` pair and rewriting that page
// is not this change's business; a caller that wants the seam's own result
// shape should prefer one of the `fetch*` wrappers above for a product
// operation, all of which parse into `ErrorEnvelope` first.
export { getHealth, getReadiness };

export type { HealthStatus, Principal };
