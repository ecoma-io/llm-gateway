// The idempotency key's lifetime, tested in both directions.
//
// A key has to be stable enough that a retry is the same request and short
// enough that a second deliberate top-up is a second payment, and the two
// failures are not symmetrical: regenerating on a retry charges a customer
// twice for one act, and reusing across two acts swallows the second act and
// answers "already done" while the customer waits for money that is not coming.
// Neither shows up in a test that only checks a key was sent, so every case
// below is about which of two keys it is.
//
// The rule under test is deliberately NOT "same offer" alone. It is whether the
// PREVIOUS attempt is finished. A destination means it is; so does a cancelled
// payment with none, because the provider refused the destination under that
// payment's immutable transfer identity. `created` with no destination is the
// one answer that keeps it open — the provider was never reached, and the retry
// has to keep its key so it converges rather than opening a second payment.
import { describe, expect, it, vi } from "vitest";

import type { ApiFailure, Failure } from "@/lib/api";
import {
  beginTopUp,
  retireTopUp,
  retireTopUpAfterFailure,
  type TopUpAttempt,
} from "@/modules/payments/idempotency";
import type { TransferInstructions } from "@/modules/payments/instructions";
import type { PaymentIntent } from "@ecoma-io/llm-gateway-console-api-client";

/** A destination as the provider states one, so the retire tests carry a real answer. */
const INSTRUCTIONS: TransferInstructions = {
  transfer_code: "00112233445566",
  bank_name: "Example Bank",
  account_holder: "Example Ltd",
  qr_url: "https://pay.example.test/qr/y1",
};

/** A payment as the API returns one, with the two fields this module reads. */
function payment({
  status = "awaiting_transfer",
  transfer_instructions = INSTRUCTIONS,
}: {
  readonly status?: PaymentIntent["status"];
  readonly transfer_instructions?: TransferInstructions | null;
} = {}): PaymentIntent {
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

/** A server refusal with the one code that can end an attempt. */
function apiFailure(code: "conflict" | "upstream_unavailable"): ApiFailure {
  return {
    kind: "api",
    unauthenticated: false,
    envelope: { error: { code, message: "the server's own words" }, request_id: "req-payments-1" },
  };
}

const TRANSPORT_FAILURE: Failure = { kind: "transport", error: new Error("network unavailable") };

/** A generator a test can read, so a REUSE is asserted by identity and not by luck. */
function counter() {
  let n = 0;
  return vi.fn(() => `key-${++n}`);
}

describe("beginTopUp", () => {
  it("mints a key when there is no open attempt", () => {
    const mint = counter();
    expect(beginTopUp(undefined, "offer-a", mint)).toEqual({ offer: "offer-a", key: "key-1" });
    expect(mint).toHaveBeenCalledTimes(1);
  });

  it("REUSES the live attempt's key for the same offer", () => {
    // The retry. A dropped response, a second click, a provider that was down:
    // all of them are the SAME act, and the key is what says so on the wire.
    const mint = counter();
    const first = beginTopUp(undefined, "offer-a", mint);
    const retry = beginTopUp(first, "offer-a", mint);

    expect(retry.key).toBe(first.key);
    // And it is the same attempt object, not a copy with the same string: the
    // reuse is a decision this function made, not a coincidence of two minted
    // values happening to match.
    expect(retry).toBe(first);
    expect(mint).toHaveBeenCalledTimes(1);
  });

  it("mints a NEW key for a different offer, because that is a different act", () => {
    const mint = counter();
    const first = beginTopUp(undefined, "offer-a", mint);
    const other = beginTopUp(first, "offer-b", mint);

    expect(other.key).not.toBe(first.key);
    expect(other).toEqual({ offer: "offer-b", key: "key-2" });
    expect(mint).toHaveBeenCalledTimes(2);
  });

  it("defaults to the platform's own generator, and to a value the contract admits", () => {
    // No scheme is invented here. `crypto.randomUUID()` is the platform's, it
    // is 36 characters against the contract's 128-character bound, and this
    // test is what says the default is that generator rather than a hand-rolled
    // one that would have to be maintained.
    const attempt = beginTopUp(undefined, "offer-a");
    expect(attempt.key).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/);
    expect(attempt.key.length).toBeLessThanOrEqual(128);
    // Two mints without an intervening answer are two keys, which is how a
    // customer gets a second payment when they deliberately start a second one.
    expect(beginTopUp(undefined, "offer-a").key).not.toBe(attempt.key);
  });
});

describe("retireTopUp", () => {
  it("retires the key once the answer carries a destination to pay", () => {
    // The act is over: the customer has been handed an account to send money
    // to. The same offer clicked again afterwards is a NEW top-up — this is the
    // half that stops "fund me once" from meaning "fund me forever".
    const attempt: TopUpAttempt = { offer: "offer-a", key: "key-1" };
    expect(retireTopUp(attempt, payment())).toBeUndefined();
  });

  it("keeps the key for created with no destination, so the retry converges", () => {
    // The 503 shape the contract describes: the payment exists in `created`
    // with null `transfer_instructions` because the provider never answered.
    // Retiring here would make the customer's retry a second payment instead of
    // the same one — which is precisely the convergence the key exists to
    // provide.
    const attempt: TopUpAttempt = { offer: "offer-a", key: "key-1" };
    expect(retireTopUp(attempt, payment({ status: "created", transfer_instructions: null }))).toBe(
      attempt,
    );
  });

  it("retires the key for a cancelled payment without a destination", () => {
    // A duplicate SePay order code is permanently bound at the provider, but
    // gave this platform no VA to show. Keeping its key would replay that
    // conflict forever, so the next click must mint a new payment.
    const attempt: TopUpAttempt = { offer: "offer-a", key: "key-1" };
    expect(
      retireTopUp(attempt, payment({ status: "cancelled", transfer_instructions: null })),
    ).toBeUndefined();
  });

  it("has nothing to retire when no attempt was open", () => {
    expect(retireTopUp(undefined, payment())).toBeUndefined();
    expect(
      retireTopUp(undefined, payment({ status: "created", transfer_instructions: null })),
    ).toBeUndefined();
  });
});

describe("retireTopUpAfterFailure", () => {
  it("retires the key after conflict so the next click opens a new payment", () => {
    const attempt: TopUpAttempt = { offer: "offer-a", key: "key-1" };
    expect(retireTopUpAfterFailure(attempt, apiFailure("conflict"))).toBeUndefined();
  });

  it("keeps the key for retryable and transport failures", () => {
    const attempt: TopUpAttempt = { offer: "offer-a", key: "key-1" };
    expect(retireTopUpAfterFailure(attempt, apiFailure("upstream_unavailable"))).toBe(attempt);
    expect(retireTopUpAfterFailure(attempt, TRANSPORT_FAILURE)).toBe(attempt);
    expect(retireTopUpAfterFailure(attempt, undefined)).toBe(attempt);
  });
});

describe("one top-up, from first click to a second deliberate one", () => {
  it("keeps one key across a failure and a retry, and mints a new one after success", () => {
    // The whole rule, as the sequence a customer actually performs. Stated as
    // one test because the two halves are only correct together: a module that
    // passed either of the tests above in isolation and failed this one would
    // be a module whose key lifetime is wrong at exactly one boundary.
    const mint = counter();

    const first = beginTopUp(undefined, "offer-a", mint);
    // The first request failed — a dropped response, or a 503 from a provider
    // that never answered. NO answer, so nothing retires and the key is live.
    const afterFailure = retireTopUp(
      first,
      payment({ status: "created", transfer_instructions: null }),
    );
    // The customer presses the button again for the same offer.
    const retry = beginTopUp(afterFailure, "offer-a", mint);
    expect(retry.key).toBe(first.key);
    expect(mint).toHaveBeenCalledTimes(1);

    // That retry succeeded and handed back a destination to pay into.
    const afterSuccess = retireTopUp(retry, payment());
    expect(afterSuccess).toBeUndefined();

    // The customer comes back later and funds the same offer again on purpose.
    // A new key, and therefore a new payment: reusing the old one would answer
    // with the first payment and fund nothing.
    const second = beginTopUp(afterSuccess, "offer-a", mint);
    expect(second.key).not.toBe(first.key);
    expect(mint).toHaveBeenCalledTimes(2);
  });
});

describe("what this module is not", () => {
  it("writes nothing to any storage the browser keeps", () => {
    // The key authorises a money-moving request. It is held in memory for the
    // life of the screen and nowhere else — ADR 0012 §2 names exactly one
    // permitted storage key in this app and this is not it — so this test
    // exists to make a `localStorage` line in this module a failure rather than
    // a review finding. A key that outlived the session would outlive the
    // customer's intent and converge a fresh top-up on an old payment.
    const local = vi.spyOn(Storage.prototype, "setItem");
    const mint = counter();

    const attempt = beginTopUp(undefined, "offer-a", mint);
    const retry = beginTopUp(attempt, "offer-a", mint);
    retireTopUp(retry, payment());

    expect(local).not.toHaveBeenCalled();
    local.mockRestore();
  });
});
