// Whether a destination is shown, tested in both directions.
//
// `showTransferInstructions` is the console's answer to the one question the
// contract explicitly leaves to the client — "Whether a destination should be
// SHOWN is a question about the status beside this field, and it is a client's
// to answer" — so both branches are customer-visible and each is a way to pay
// wrongly:
//
//   - A destination is shown only while the provider has not settled the
//     payment. A settled payment keeps its recorded destination in every later
//     state, and rendering it would invite a second transfer into an account
//     whose payment is already over — the mistake that costs a customer money.
//   - A payment with no recorded destination has none to show. That is the
//     durable `created` behind a `503`, and a screen that rendered an empty
//     account number would be inventing a destination the provider never named.
//
// The instructions are returned verbatim and never parsed, so the assertion is
// object IDENTITY and not a description of the members. A test that read the
// account number back would be the parsing the contract forbids, and would keep
// passing on the day this function started rebuilding the value.
import { describe, expect, it } from "vitest";

import { showTransferInstructions } from "@/modules/payments/instructions";
import type { TransferInstructions } from "@/modules/payments/instructions";
import type { PaymentIntent, PaymentIntentState } from "@ecoma-io/llm-gateway-console-api-client";

/** A destination as the provider states one. */
const INSTRUCTIONS: TransferInstructions = {
  transfer_code: "00112233445566",
  bank_name: "Example Bank",
  account_holder: "Example Ltd",
  qr_url: "https://pay.example.test/qr/y1",
};

/** A payment carrying one destination, in whatever state the test is about. */
function payment(
  status: PaymentIntentState,
  transfer_instructions: TransferInstructions | null,
): PaymentIntent {
  return {
    id: "y0000000-0000-4000-8000-0000000000y1",
    status,
    amount_minor_units: 2_500,
    currency: "EUR",
    minor_unit_exponent: 2,
    transfer_instructions,
    created_at: "2026-09-20T09:00:00Z",
    expires_at: "2026-09-20T09:30:00Z",
  };
}

/**
 * The states in which the provider has already spoken. A payment here has a
 * recorded destination and must not be presented with one to pay, because the
 * money it names has already been decided.
 */
const SETTLED: readonly PaymentIntentState[] = [
  "succeeded",
  "failed",
  "cancelled",
  "expired",
  "partially_refunded",
  "refunded",
  "quarantined",
];

describe("showTransferInstructions", () => {
  it("hands back the provider's own instructions, unread, while the payment can still be paid", () => {
    for (const status of ["awaiting_transfer", "requires_action"] as const) {
      // Identity, not equality: the value is returned verbatim, and a copy or a
      // re-derivation would be the parsing the contract forbids. The contract
      // spends a payment's whole life in `awaiting_transfer` for this
      // deployment's provider, and `requires_action` is kept for the providers
      // that ask for one more step — neither is a reason to hide the account.
      expect(showTransferInstructions(payment(status, INSTRUCTIONS)), status).toBe(INSTRUCTIONS);
    }
  });

  it("shows nothing once the provider has settled the payment, though the destination is recorded", () => {
    // The contract says the field is kept "in every later state, because the
    // account it names is the account the money is resolved by" — so the field
    // being present is not permission to show it. Paying into a settled
    // payment's account again is money sent for nothing.
    for (const status of SETTLED) {
      expect(showTransferInstructions(payment(status, INSTRUCTIONS)), status).toBeUndefined();
    }
  });

  it("shows nothing for a payment whose destination was never obtained", () => {
    // The durable `created` a `503` leaves behind: the provider was never
    // reached, so there is no account to name yet and nothing to render. The
    // second assertion is the defensive half — the contract says the field is
    // null "exactly while the payment is `created`", but a null destination is
    // nothing to show whatever the status says, and this function is total
    // rather than trusting one clause of the contract to be the only null it
    // ever meets.
    expect(showTransferInstructions(payment("created", null))).toBeUndefined();
    expect(showTransferInstructions(payment("awaiting_transfer", null))).toBeUndefined();
  });
});
