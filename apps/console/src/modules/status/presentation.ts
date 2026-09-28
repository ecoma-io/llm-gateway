// Status is never colour-only (ADR 0012 §7). One presentation map per domain,
// keyed exhaustively over the GENERATED enum with `satisfies Record<Enum, …>`,
// so a state added to `console.yaml` lands in the client's types and this file
// stops type-checking until someone gives it a label.
//
// Three channels per state, and the count is the design: colour, an
// `aria-hidden` icon, and a human label. Two of the three survive greyscale
// and two of the three survive a screen reader that ignores colour entirely —
// a single channel fails one of those readers every time, so a status
// presented by colour alone is a status some operators cannot read.
//
// The label is a SENTENCE, never the contract token. `ok` is the contract's
// word for a healthy probe and nobody outside the contract knows what it
// means; a badge reading `ok` tells an operator nothing they could act on.
// That is the defect ADR 0012 §7 names on `GatewayStatusPage.vue:87`, and it
// is the same defect every one of these maps is written against.
import {
  CircleCheck,
  CircleDashed,
  CircleSlash,
  CircleX,
  Clock,
  Info,
  KeyRound,
  CirclePause,
  CirclePlay,
  type LucideIcon,
  TriangleAlert,
  UserPlus,
  XCircle,
} from "@lucide/vue";
import type {
  Account,
  ApiKey,
  Entitlement,
  Finding,
  FundingBucket,
  LedgerEntry,
  PaymentIntentState,
  ReconciliationRun,
  Subscription,
  User,
} from "@ecoma-io/llm-gateway-console-api-client";

/**
 * One state's presentation. `tone` is the colour channel and it is never the
 * only one; `icon` is `aria-hidden` at the call site so it adds a shape
 * without adding a second spoken name; `label` is what a reader is told.
 */
export interface StatusPresentation {
  readonly label: string;
  readonly icon: LucideIcon;
  /**
   * Loom's `Badge`/`Alert` tone. Held to the five the primitives declare, and
   * every value here is a deliberate choice about meaning rather than a
   * mapping from a colour name — `neutral` means "this is a fact about a
   * lifecycle, not a problem", which is why `active` and `acknowledged` wear
   * it and `resolved` wears success.
   */
  readonly tone: "neutral" | "info" | "success" | "warning" | "destructive";
}

/**
 * The probe's status. `HealthStatus["status"]` is a bare `string`, NOT an
 * enum, so there is no union to key exhaustively — which is why this one map
 * is a function with an explicit fallback instead of a table, and why the
 * fallback is destructive rather than neutral. A probe that reported
 * something this console has never heard of is not a probe in a healthy
 * state, and a badge that guesses "fine" for an unrecognised token is the
 * console asserting a fact the contract never made.
 */
export function probeStatusPresentation(status: string | undefined): StatusPresentation {
  return status === "ok"
    ? { label: "Healthy", icon: CircleCheck, tone: "success" }
    : {
        label: status === undefined ? "Not reported" : "Not healthy",
        icon: TriangleAlert,
        tone: "destructive",
      };
}

/**
 * A user's lifecycle. `invited` is a live state with NO credential behind it
 * (ADR 0012 §2), so its label says so rather than calling the row active —
 * an operator who reads "active" for an invited row will believe a login
 * exists that does not.
 */
export const USER_STATE_PRESENTATION = {
  invited: { label: "Invited", icon: UserPlus, tone: "info" },
  active: { label: "Active", icon: CircleCheck, tone: "success" },
  removed: { label: "Removed", icon: CircleSlash, tone: "neutral" },
} as const satisfies Record<User["state"], StatusPresentation>;

/** An API key's ownership record. Never the credential — there is none to show. */
export const API_KEY_STATE_PRESENTATION = {
  active: { label: "Active", icon: CircleCheck, tone: "success" },
  revoked: { label: "Revoked", icon: XCircle, tone: "neutral" },
} as const satisfies Record<ApiKey["state"], StatusPresentation>;

/**
 * An account's lifecycle. `suspended` and `closed` both stop the account
 * working, and they are given different labels because the operator's next
 * move differs: a suspension is reversible, a closure is not.
 */
export const ACCOUNT_STATE_PRESENTATION = {
  active: { label: "Active", icon: CircleCheck, tone: "success" },
  suspended: { label: "Suspended", icon: CirclePause, tone: "warning" },
  closed: { label: "Closed", icon: CircleSlash, tone: "destructive" },
} as const satisfies Record<Account["state"], StatusPresentation>;

/**
 * A subscription's state. `cancelled` is terminal and already happened — a
 * scheduled cancellation is DATA (`cancel_at`), not a state, so this map has
 * no "cancelling" member to render (the contract's own note on the field).
 */
export const SUBSCRIPTION_STATE_PRESENTATION = {
  pending: { label: "Pending", icon: Clock, tone: "info" },
  active: { label: "Active", icon: CirclePlay, tone: "success" },
  suspended: { label: "Suspended", icon: CirclePause, tone: "warning" },
  cancelled: { label: "Cancelled", icon: CircleSlash, tone: "neutral" },
  expired: { label: "Expired", icon: Clock, tone: "neutral" },
} as const satisfies Record<Subscription["state"], StatusPresentation>;

/**
 * An entitlement grant's state. The row carries NO balance column (the
 * contract says so on the schema), so the label describes the GRANT's
 * lifecycle and nothing else — a reader looking for "how much is left" is
 * sent to the funding bucket, which is where that figure lives.
 */
export const ENTITLEMENT_STATE_PRESENTATION = {
  active: { label: "Granted", icon: CircleCheck, tone: "success" },
  expired: { label: "Expired", icon: Clock, tone: "neutral" },
} as const satisfies Record<Entitlement["state"], StatusPresentation>;

/** A funding bucket's status. */
export const FUNDING_BUCKET_STATUS_PRESENTATION = {
  active: { label: "Open", icon: CircleCheck, tone: "success" },
  closed: { label: "Closed", icon: CircleSlash, tone: "neutral" },
} as const satisfies Record<FundingBucket["status"], StatusPresentation>;

/**
 * A finding's status. `open` is the one that needs a human, so it is the only
 * one wearing a warning tone; `acknowledged` is a decision already taken and
 * `resolved` is the outcome, and dressing either as a problem trains
 * operators to ignore the column that matters.
 */
export const FINDING_STATUS_PRESENTATION = {
  open: { label: "Open", icon: TriangleAlert, tone: "warning" },
  acknowledged: { label: "Acknowledged", icon: Info, tone: "info" },
  resolved: { label: "Resolved", icon: CircleCheck, tone: "success" },
} as const satisfies Record<Finding["status"], StatusPresentation>;

/**
 * A finding's severity. `info` is neutral rather than informational-blue: an
 * informational finding is still a divergence, and tinting it as good news
 * would be a claim the ledger has not made.
 */
export const FINDING_SEVERITY_PRESENTATION = {
  info: { label: "Info", icon: Info, tone: "neutral" },
  warning: { label: "Warning", icon: TriangleAlert, tone: "warning" },
  critical: { label: "Critical", icon: CircleX, tone: "destructive" },
} as const satisfies Record<Finding["severity"], StatusPresentation>;

/**
 * A reconciliation run's status. A `running` pass is neutral rather than
 * informational: it is the expected state of a healthy worker, and a
 * dashboard that paints every in-flight pass blue is painting the normal
 * case as an event. A `failed` pass is destructive and there is deliberately
 * no "stale" or "overdue" member — a staleness threshold cannot be computed
 * from this plane's own rows (ADR 0012 §5), so a run that has simply not run
 * renders as what it is: no run.
 */
export const RECONCILIATION_RUN_STATUS_PRESENTATION = {
  running: { label: "Running", icon: CircleDashed, tone: "neutral" },
  completed: { label: "Completed", icon: CircleCheck, tone: "success" },
  failed: { label: "Failed", icon: CircleX, tone: "destructive" },
} as const satisfies Record<ReconciliationRun["status"], StatusPresentation>;

/**
 * A ledger leg's kind. This is the one map whose members are words an
 * operator already uses for money moving, and the labels are the plain
 * English rather than the enum's names: a leg the API calls a `consume` is a
 * spend, and reading a ledger is what an operator is doing here.
 */
export const LEDGER_KIND_PRESENTATION = {
  grant: { label: "Grant", icon: CircleCheck, tone: "success" },
  topup: { label: "Top-up", icon: CircleCheck, tone: "success" },
  hold: { label: "Hold", icon: Clock, tone: "info" },
  release: { label: "Release", icon: CircleSlash, tone: "neutral" },
  consume: { label: "Spend", icon: KeyRound, tone: "warning" },
  adjustment: { label: "Adjustment", icon: Info, tone: "info" },
} as const satisfies Record<LedgerEntry["kind"], StatusPresentation>;

/**
 * Which way a leg moved money. The ledger stores a SIGN on the deltas rather
 * than a `direction` column — the contract's own schema says a sign is
 * meaningful on a `delta` and never on a balance — so the sign IS the
 * direction and a zero delta is a leg that moved nothing.
 *
 * This is a function over a number rather than a table over an enum because
 * there is no enum to key: `LedgerEntry` declares two `Money` deltas and
 * nothing else. The sign is read, never computed — a leg's `settled_delta` is
 * the value the ledger stored.
 */
/**
 * Where one payment stands, as a customer reads it.
 *
 * Keyed exhaustively over the generated `PaymentIntentState` union, and the
 * union is the contract's own ten members rather than a summary of them: a
 * state added to `console.yaml` lands here as a compile error until somebody
 * labels it, which is the property the file header states and the one this map
 * has to satisfy like every other.
 *
 * The labels are sentences about the MONEY, and two of them are load-bearing in
 * a way the others are not:
 *
 *   - `expired` does NOT say the payment will never be funded, because the
 *     contract's own state machine allows `expired → succeeded`. Expiry is a
 *     LOCAL decision — this platform stopped waiting — and a delivery arriving
 *     after it is still honoured, because the provider alone gets to say
 *     whether the customer paid. A label reading "Not funded" would be the
 *     console promising a customer something the provider can still contradict.
 *   - `quarantined` is not a failure of the payment. It is what a delivery that
 *     authenticated and could not be interpreted is recorded as, so the label
 *     says what actually happened to the delivery and leaves the money's own
 *     status alone.
 */
export const PAYMENT_STATE_PRESENTATION = {
  created: { label: "Checkout not open yet", icon: Clock, tone: "info" },
  checkout_open: { label: "Waiting at your provider", icon: CircleDashed, tone: "info" },
  requires_action: {
    label: "Your provider needs another step",
    icon: TriangleAlert,
    tone: "warning",
  },
  succeeded: { label: "Funded", icon: CircleCheck, tone: "success" },
  failed: { label: "Not funded", icon: CircleX, tone: "destructive" },
  cancelled: { label: "Not funded — cancelled", icon: CircleSlash, tone: "neutral" },
  // Deliberately NOT "not funded": see the note above. This platform stopped
  // waiting; the provider has not spoken last.
  expired: { label: "We stopped waiting", icon: Clock, tone: "neutral" },
  partially_refunded: { label: "Partly refunded", icon: Info, tone: "info" },
  // "Fully refunded" and NOT "Refunded": the label is a phrase a customer reads
  // and never the contract's own token, and `refunded` capitalised is the token
  // again. It is also the pair the badge above has to be tellable from — what
  // the customer keeps changed in one case and completely in the other.
  refunded: { label: "Fully refunded", icon: CircleSlash, tone: "neutral" },
  quarantined: { label: "A provider message could not be read", icon: XCircle, tone: "warning" },
} as const satisfies Record<PaymentIntentState, StatusPresentation>;

/**
 * The same ten states, as the sentence a screen shows when it has to explain
 * one. A badge carries a label; a reader who has just come back from a
 * provider's checkout needs the paragraph, and it is the paragraph that says
 * out loud that returning from checkout proves nothing.
 *
 * Exhaustive for the same reason and in the same way as the map above — the
 * two are one claim split in two, and a state with a badge and no sentence
 * would be a state the console renders without explaining.
 */
export const PAYMENT_STATE_EXPLANATION = {
  created:
    "We have recorded this payment but its checkout is not open yet. Nothing has been charged.",
  checkout_open:
    "The provider's checkout is open and the payment is not finished. If you have just come back from it, we are still waiting for the provider to confirm — coming back does not mark anything paid.",
  requires_action:
    "Your provider has asked for one more step before this payment can complete. Nothing is charged until the provider reports it.",
  succeeded: "Your provider confirmed this payment, and the amount was credited to this account.",
  failed: "Your provider reported that this payment did not go through. Nothing was credited.",
  cancelled: "This payment was cancelled before it completed. Nothing was credited.",
  expired:
    "We stopped waiting for this checkout, so it can no longer be opened. A confirmation arriving late from your provider is still honoured, so this payment may yet be funded — your provider is the only party that says whether you paid.",
  partially_refunded:
    "Your provider gave part of this payment back. What you keep changed; what was funded did not.",
  refunded: "Your provider gave this payment back. What you keep changed; what was funded did not.",
  quarantined:
    "One message from your provider authenticated but could not be read, so an operator has to look at it. That is a fact about the message, not about your payment — the payment's status is whatever the money says it is.",
} as const satisfies Record<PaymentIntentState, string>;

/**
 * The states in which the provider has not yet spoken about the money, which is
 * the set a customer who has just returned from a checkout is most likely to be
 * looking at. `quarantined` is deliberately absent: it is an operator-facing
 * fact about a delivery and not a payment waiting on anything.
 */
export const PAYMENT_AWAITING_PROVIDER_STATES: ReadonlySet<PaymentIntentState> = new Set([
  "created",
  "checkout_open",
  "requires_action",
]);

export function ledgerDirectionPresentation(
  settledDelta: number,
  heldDelta: number,
): StatusPresentation {
  if (settledDelta > 0) {
    return { label: "In", icon: CircleCheck, tone: "success" };
  }
  if (settledDelta < 0) {
    return { label: "Out", icon: CircleX, tone: "warning" };
  }
  if (heldDelta > 0) {
    return { label: "Held", icon: Clock, tone: "info" };
  }
  if (heldDelta < 0) {
    return { label: "Released", icon: CircleSlash, tone: "neutral" };
  }
  return { label: "No movement", icon: Info, tone: "neutral" };
}
