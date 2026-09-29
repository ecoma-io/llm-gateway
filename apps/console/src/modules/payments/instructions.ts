// The destination a customer pays into, and the one question the contract
// leaves to this side: whether to SHOW it.
//
// `PaymentTransferInstructions` is the provider's own description of where the
// money goes — an account number, the bank that holds it, the name it is held
// in, and an image the provider drew — and the contract is explicit that every
// member is "returned verbatim and never parsed": this console renders them and
// does not read an amount out of the QR's URL, does not match the bank name
// against anything, and does not compose its own QR from the other three. Any
// of those would be reimplementing an encoding the provider owns and would
// break the day that encoding changed.
//
// The field is a copy of what was recorded and NOT a judgement about the
// payment: a payment whose destination has been recorded keeps it in every
// later state, because the account it names is the account the money is
// resolved by, and a payment whose destination disappeared from its own record
// would be one whose deliveries could not be explained to the customer who sent
// them. So the contract does not answer whether to display it, and says so —
// "Whether a destination should be SHOWN is a question about the status beside
// this field, and it is a client's to answer rather than this field's to
// pre-answer." This module is that answer, and it is a module of its own for
// the reason the navigation it replaces was one: the rule is a decision with a
// reason, and a rule buried in a template is a rule no test can read.
//
// Both halves of the rule, because each is a customer who pays wrongly:
//
//   - A payment with no instructions has no destination to name. That is the
//     durable `created` a `503` leaves behind: the provider was never reached,
//     so nothing was obtained to send money to, and a screen that rendered an
//     empty account number would be inventing a destination.
//   - A payment the provider has SETTLED is not one to pay into. Its
//     destination is still recorded, and rendering it would invite a second
//     transfer into an account whose payment is already over — the same defect
//     the console refused when a settled payment's hosted page was rendered as
//     closed rather than offered. So a destination is shown exactly while the
//     provider has not yet spoken about the money, which is the set
//     `PAYMENT_AWAITING_PROVIDER_STATES` names.
import { PAYMENT_AWAITING_PROVIDER_STATES } from "@/modules/status/presentation";
import type { PaymentIntent } from "@ecoma-io/llm-gateway-console-api-client";

/**
 * Where to send the money, as the provider stated it.
 *
 * Derived from the payment's own field rather than imported from the client's
 * public surface, and the derivation is the contract's own: this is exactly the
 * non-null arm of `PaymentIntent.transfer_instructions`. A schema change
 * therefore reaches this name without a second declaration to keep in step,
 * which is the point of deriving it rather than restating four fields.
 */
export type TransferInstructions = NonNullable<PaymentIntent["transfer_instructions"]>;

/**
 * The instructions to render for one payment, or `undefined` when there is
 * nothing a customer could act on.
 *
 * A pure function of the payment, so both the page and the table cell can ask
 * the same question and cannot answer it differently. It reads the status the
 * server sent and the destination the server sent, and decides nothing else: it
 * does not validate the account number, does not reach for the QR, and does not
 * care whether the customer has already paid.
 */
export function showTransferInstructions(payment: PaymentIntent): TransferInstructions | undefined {
  if (!PAYMENT_AWAITING_PROVIDER_STATES.has(payment.status)) return undefined;
  return payment.transfer_instructions ?? undefined;
}
