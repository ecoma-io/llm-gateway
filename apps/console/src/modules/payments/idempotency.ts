// The idempotency key, and the one rule that makes it correct: one key, one
// act — where "one act" is decided by the customer and not by the network.
//
// `createPaymentIntent` requires a key, and the contract is explicit about what
// it means: every attempt at the SAME logical top-up carries the same value —
// the first try, the retry after a dropped response, the retry after a provider
// outage — and a request repeating a key is answered with the payment that key
// already names rather than with a second one. So the key is not a
// request-level nonce. It is the caller's identity for a top-up, and it has to
// outlive any single attempt.
//
// The failure this module exists to prevent runs in BOTH directions, and each
// is a customer who paid and does not have what they paid for:
//
//   - **A key regenerated on a retry** is the same payment made twice. The
//     customer's first request reached the server, the answer did not come
//     back, they press the button again, and the second request names a
//     different act — so the server prices a second payment and the customer is
//     charged twice for one top-up, or is shown a destination to send money to
//     for a payment that already succeeded.
//   - **A key REUSED across two deliberate top-ups** is a payment silently
//     swallowed. The customer funds the account, comes back, decides to fund it
//     again, and the second request converges on the payment the first key
//     already names — a 201 that says "already done" while the customer waits
//     for money that is not coming. This is the quieter of the two and the one
//     that is easier to write by accident, because reusing a key always looks
//     like the safe choice.
//
// The rule that separates them is not "same offer" and not "same session". It
// is whether the PREVIOUS attempt is finished. An attempt is finished when the
// server has handed back a payment the customer can act on — the contract's
// `transfer_instructions`, non-null exactly when this plane holds a destination
// to send money to. Until then the key is live and a retry is a retry; from then
// on the act is over and the next click is a new one.
//
// `retireTopUp` therefore reads the ANSWER rather than the failure. A `503`
// from a provider that never answered leaves the payment durable in `created`
// with no `transfer_instructions` — the contract says the field is null exactly
// while the payment is `created` — so the key stays live and the retry the
// customer makes converges on the payment this platform already wrote, which is
// the outcome the server's own key semantics are built to absorb. A `201`
// carrying instructions retires it, so the same customer funding the same offer
// again later gets a new payment rather than a rendering of the old one.
import type { Failure } from "@/lib/api";
import type { PaymentIntent } from "@ecoma-io/llm-gateway-console-api-client";

/**
 * One top-up the customer has committed to and whose answer is still open.
 *
 * Held in memory for the life of the screen and nowhere else. It is a key, not
 * a secret — the contract bounds it at 128 characters and it authorises nothing
 * on its own — but it is still a fact about a money-moving request, and ADR
 * 0012 §2 names exactly one permitted storage key in this app (`loom:theme`).
 * A key in `localStorage` would outlive the session, outlive the customer's
 * intent, and survive into a later tab where it would silently converge a fresh
 * top-up on an old payment. Nothing about a payment is stored anywhere.
 */
export interface TopUpAttempt {
  /** The offer this key was minted for. Two different offers are two different acts. */
  readonly offer: string;
  /** The key itself, as `createPaymentIntent` receives it. */
  readonly key: string;
}

/**
 * The key for the attempt this commit belongs to.
 *
 * Reuses the live attempt's key when, and only when, it is an attempt at the
 * SAME offer. Anything else is a new act and gets a new key: a different offer
 * is a different purchase, and a commit arriving after the previous attempt
 * retired is the customer deliberately funding themselves again.
 *
 * `mint` is injected so a test can assert the REUSE without depending on a
 * random source, and so the production default is the platform's own generator
 * rather than a scheme invented here. `crypto.randomUUID()` is 36 characters,
 * comfortably inside the contract's 128-character bound, and `randomUUID` is
 * available in every browser this console supports and in the test environment
 * it is checked by.
 */
export function beginTopUp(
  pending: TopUpAttempt | undefined,
  offer: string,
  mint: () => string = () => crypto.randomUUID(),
): TopUpAttempt {
  if (pending !== undefined && pending.offer === offer) return pending;
  return { offer, key: mint() };
}

/**
 * The attempt still live after one answer, or `undefined` when the act is over.
 *
 * Two tests, and the second one is the reason this function is no longer the
 * single sentence it was.
 *
 * **A payment carrying instructions is over.** `transfer_instructions !== null`
 * means the provider WAS reached and a destination was obtained, and whether
 * that destination is one the customer can still pay into or one whose payment
 * is already settled does not matter here: either way the attempt has finished.
 * Retiring the key on it is what makes a second top-up a second payment.
 *
 * **So is a payment whose attempt this platform has abandoned.** The contract
 * used to say the field was null "exactly while the payment is `created`", and
 * this function used to be that test alone: null meant the provider had not
 * answered yet, and keeping the key is what makes a retry converge. That is no
 * longer the whole truth — a payment the provider refused a destination for is
 * `cancelled` with no destination, because the destination it was refused is
 * one the provider holds under this payment's own transfer identity and cannot
 * be shown to anyone. It is not `created`, a retry under this key resolves to
 * it and is told the attempt is over, and the contract's answer is a NEW
 * payment under a NEW key. Keeping the key here would leave the customer
 * clicking an offer that can never open anything: the reuse rule below hands
 * back the same key for the same offer, so the next click repeats the refusal,
 * and a deployment with one offer has no other button to press. `created` is
 * therefore the state to keep the key in — it is the one state that means
 * "opened, no destination, a second attempt is the same attempt" — and every
 * other answer ends the act.
 */
export function retireTopUp(
  pending: TopUpAttempt | undefined,
  payment: PaymentIntent,
): TopUpAttempt | undefined {
  if (payment.transfer_instructions !== null) return undefined;
  return payment.status === "created" ? pending : undefined;
}

/**
 * The attempt still live after a REFUSAL, or `undefined` when the refusal has
 * ended the act.
 *
 * Exactly one refusal ends it, and it is the one the contract says does:
 * `conflict`. Both of its causes are durable states rather than moments — an
 * account that may not fund itself, and a payment whose transfer identity the
 * provider already holds under a destination nobody was ever shown — and
 * neither is something another attempt under the same key can clear. The
 * contract's own answer to the second is a new top-up, and a console that kept
 * the key would make that impossible to follow: `beginTopUp` hands the same key
 * back for the same offer, so the customer's next click would repeat the
 * refusal, and a deployment selling one offer has no other button to press.
 *
 * Every other refusal keeps the key, and that is the whole reason this is a
 * separate function rather than a broader rule. A `503` is the provider not
 * having answered, and the payment it left behind is one a retry under this key
 * converges on; a transport failure is not even a contract answer. Neither is
 * evidence that the act is over, and treating either as such would be the
 * second-payment bug the key exists to prevent.
 *
 * Kept beside `retireTopUp` rather than in the page, because it is the same
 * question asked of the other kind of answer: is this act still open?
 */
export function retireTopUpAfterFailure(
  pending: TopUpAttempt | undefined,
  failure: Failure | undefined,
): TopUpAttempt | undefined {
  if (failure === undefined || failure.kind !== "api") return pending;
  return failure.envelope.error.code === "conflict" ? undefined : pending;
}
