// The failure matrix, as data (ADR 0012 §6). Every code in the contract's
// `Error["code"]` union is given a declared console behaviour here, and this
// file is the only place one is declared — a page that wants to render a
// failure reads its behaviour out of this table rather than deciding.
//
// The union is GENERATED from `api/openapi/shared/errors.yaml`, so a code
// added to the contract lands in the client's types and `CONSOLE_BEHAVIOUR`
// stops type-checking until someone writes the UI for it. That is AGENTS.md
// rule 6 — behavioural changes arrive with tests — satisfied mechanically
// rather than by a reviewer noticing, and it is why the key type below is
// `Record<Error["code"], …>` and not a plain `Record<string, …>`.
//
// Three codes are unreachable from this console BY CONSTRUCTION:
// `unsupported_version`, `revision_gap` and `snapshot_required` are projection
// codes belonging to the Data Plane's projection operations, and no operation
// on `console.yaml` can return one. They are still declared, because the
// exhaustiveness requirement is a property of the contract's union and not of
// today's reachability — and because a behaviour that is a rendering of the
// server's own message is better than an unhandled crash if one ever arrives.
// `assertEveryCodeIsDeclaredOrUnreachable` in the spec proves the claim from
// the UI side, so the plane boundary is pinned by a test rather than a comment.
import type { Error as ApiError } from "@ecoma-io/llm-gateway-console-api-client";

export type ApiErrorCode = ApiError["code"];

/**
 * How the console answers one failure code.
 *
 * `recovery` is the whole of the design, and it is why this is a table rather
 * than a message string:
 *
 * - `sign-in` sends the visitor to the sign-in screen. It is the only
 *   behaviour that navigates, and it exists for `unauthenticated` and nothing
 *   else: a session that ended mid-visit is not an error to read, it is a
 *   screen to be sent to.
 * - `retry` offers a retry affordance AND keeps the last good data on screen.
 *   An upstream that is briefly unreachable has not made the data you already
 *   have wrong, and blanking a dashboard to say "try again" is how an operator
 *   learns to ignore the banner.
 * - `none` offers no retry and shows the request id. It is for the failures
 *   where retrying is the wrong act, and the operator's next move is to quote
 *   an identifier to whoever can look it up.
 */
export interface CodeBehaviour {
  /**
   * What a reader is told, in the console's own words. Deliberately not the
   * server's `message`: the contract says that message is "safe to present to
   * a client", which makes it a candidate, not a requirement, and a client
   * that echoes a server string verbatim renders the server's vocabulary in a
   * place the operator has learned to trust.
   */
  readonly title: string;
  /**
   * The `Alert` tone. It is the first of the three channels a status carries
   * (colour, an `aria-hidden` icon, a human label) and is never the only one.
   */
  readonly variant: "neutral" | "info" | "success" | "warning" | "destructive";
  /**
   * The operator's next move, as this console can offer it.
   */
  readonly recovery: "sign-in" | "retry" | "none";
  /**
   * Whether the `request_id` is surfaced. It is a correlation, never an audit
   * identity, and a caller may choose it — so it is offered as something to
   * quote, never presented as proof of who did what.
   */
  readonly showRequestId: boolean;
  /**
   * Whether this code can be produced by any operation on `console.yaml` at
   * all. Declared rather than inferred so a reader can see the plane boundary
   * as a fact about the contract instead of a comment that can rot.
   */
  readonly reachable: boolean;
}

/**
 * Every code, keyed exhaustively over the generated union.
 *
 * The `satisfies` clause is load-bearing twice: it fails to COMPILE if a
 * member is missing (a new code with no behaviour) and it fails to COMPILE if
 * a key is not a member (a behaviour written for a code the contract does not
 * declare, which is the drift this whole boundary exists to prevent).
 */
export const CONSOLE_BEHAVIOUR = {
  not_found: {
    title: "Not found",
    variant: "neutral",
    recovery: "none",
    showRequestId: false,
    reachable: true,
  },
  method_not_allowed: {
    title: "Not available here",
    variant: "neutral",
    recovery: "none",
    showRequestId: false,
    reachable: true,
  },
  invalid_request: {
    // The recoverable 400 an operator reaches by paging a list and then
    // changing a filter: the cursor carries a filter fingerprint the request no
    // longer makes. It is a state, not a crash, and the answer names the fix.
    title: "This view can no longer be paged",
    variant: "warning",
    recovery: "retry",
    showRequestId: true,
    reachable: true,
  },
  unauthenticated: {
    title: "Your session has ended",
    variant: "warning",
    recovery: "sign-in",
    showRequestId: false,
    reachable: true,
  },
  cursor_expired: {
    // Distinct from `invalid_request` even though both are a 400 on a page:
    // a cursor past the retained history is a different sentence to read, and
    // the console's answer differs — this one restarts at the first page
    // because the position it named no longer exists anywhere.
    title: "This page is older than the history kept",
    variant: "warning",
    recovery: "retry",
    showRequestId: true,
    reachable: true,
  },
  unsupported_version: {
    title: "Unsupported protocol version",
    variant: "destructive",
    recovery: "none",
    showRequestId: true,
    reachable: false,
  },
  revision_gap: {
    title: "Projection gap",
    variant: "destructive",
    recovery: "none",
    showRequestId: true,
    reachable: false,
  },
  snapshot_required: {
    title: "Snapshot required",
    variant: "destructive",
    recovery: "none",
    showRequestId: true,
    reachable: false,
  },
  upstream_unavailable: {
    // 503. A retry, and the last good data stays on screen: the answer this
    // surface would return is unknown, which is a statement about the answer
    // and not about the data already rendered.
    title: "A dependency is unavailable",
    variant: "warning",
    recovery: "retry",
    showRequestId: true,
    reachable: true,
  },
  service_unavailable: {
    title: "The service is not ready",
    variant: "warning",
    recovery: "retry",
    showRequestId: true,
    reachable: true,
  },
  internal: {
    // 500. NO retry, and the request id IS shown. This is the pair
    // `upstream_unavailable` is tested against: a bug is not something the
    // operator should try again, and a shrug is the presentation that trains
    // people to ignore outages. The server's cause is never carried here —
    // `writeError` logs the request id and nothing else, and a client that
    // echoed more would be promising detail the wire does not have.
    title: "Something went wrong on our side",
    variant: "destructive",
    recovery: "none",
    showRequestId: true,
    reachable: true,
  },
} as const satisfies Record<ApiErrorCode, CodeBehaviour>;

/** The codes no operation on `console.yaml` can return. */
export const UNREACHABLE_CODES: readonly ApiErrorCode[] = Object.entries(CONSOLE_BEHAVIOUR)
  .filter(([, behaviour]) => !behaviour.reachable)
  .map(([code]) => code as ApiErrorCode) as readonly ApiErrorCode[];

/**
 * The behaviour for one code. A code the contract has not declared is not
 * possible on the wire — `ApiError.code` is a union, not a string — so this
 * takes the union and returns the declared row without a fallback: a missing
 * row is a compile error, which is the point.
 */
export function behaviourFor(code: ApiErrorCode): CodeBehaviour {
  return CONSOLE_BEHAVIOUR[code];
}
