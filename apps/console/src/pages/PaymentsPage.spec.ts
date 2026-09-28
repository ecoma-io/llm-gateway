// Payments: the browser is not a financial boundary, and the tests say so.
//
// Most of this file is about what the screen REFUSES to do. It refuses to
// collect instrument data — there is no card field anywhere on it and no field
// at all. It refuses to decide that a payment succeeded: the only writer of a
// payment's status is a signature-verified webhook from the provider's servers,
// so coming back from a checkout is a re-read and the screen says in as many
// words that returning does not mark anything paid. And it refuses to price a
// top-up: the request names an OFFER, the server prices it, and the assertion
// over the whole outgoing body is what holds that.
//
// The other half is the idempotency key's lifetime, driven through the real
// rule in `modules/payments/idempotency.ts` rather than through a stub — the
// screen's own click handler is what decides reuse vs regenerate, and a stub
// there would be testing the stub. The two failures it guards against are a
// customer charged twice for one top-up and a customer whose second top-up is
// silently swallowed by the first key, and neither is visible from a test that
// only checks a key was sent.
import { flushPromises, mount, type VueWrapper } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { createMemoryHistory } from "vue-router";

const seam = vi.hoisted(() => ({
  fetchTopUpOffers: vi.fn(),
  fetchPaymentIntents: vi.fn(),
  createPaymentForOffer: vi.fn(),
  getSessionResult: vi.fn(),
}));

vi.mock("@/lib/api", async () => {
  const actual = await vi.importActual<typeof import("@/lib/api")>("@/lib/api");
  return { ...actual, ...seam };
});

/**
 * The one side effect a test cannot observe in jsdom: leaving the origin.
 *
 * `window.location.assign` is not implemented there, so the page's navigation
 * lives in a module of its own and is mocked here. That is not convenience —
 * sending the customer to the PROVIDER rather than collecting anything here is
 * the single most important thing this screen does, and a page that navigated
 * inline could only be tested by accident.
 */
const checkout = vi.hoisted(() => ({ sendToCheckout: vi.fn() }));

vi.mock("@/modules/payments/checkout", () => ({ sendToCheckout: checkout.sendToCheckout }));

import PaymentsPage from "@/pages/PaymentsPage.vue";
import { createConsoleRouter } from "@/router";
import { describe as describeViolations, inspect } from "@/lib/arch/roster";
import { CONSOLE_BEHAVIOUR, type ApiErrorCode } from "@/lib/failure-matrix";
import type { ApiFailure, ApiResult } from "@/lib/api";
import type {
  PaymentIntent,
  PaymentIntentPage,
  TopUpOfferList,
} from "@ecoma-io/llm-gateway-console-api-client";

const ok = <T>(data: T): ApiResult<T> => ({ ok: true, data });

function apiFailure(code: ApiErrorCode): ApiFailure {
  return {
    kind: "api",
    unauthenticated: code === "unauthenticated",
    envelope: { error: { code, message: "the server's own words" }, request_id: "req-payments-1" },
  };
}

/** Every code this console can produce, read off the matrix rather than copied. */
const REACHABLE = Object.entries(CONSOLE_BEHAVIOUR)
  .filter(([, behaviour]) => behaviour.reachable)
  .map(([code]) => code as ApiErrorCode);

/**
 * The price list, with the three shapes that make the rendering testable.
 *
 * A low-price offer, a high-price one, and a currency with no minor unit at
 * all. The last is the one that catches a renderer assuming two decimal
 * places: yen written as hundredths of a yen is a price a hundred times too
 * small, printed beside the name of the thing it buys.
 *
 * Every offer carries a `label`, because `TopUpOffer.label` is required — an
 * offer the deployment declares without one is refused where it is declared —
 * so there is no absent-label button branch left to cover. The two EUR offers
 * carry distinct labels, so the exact-text assertion below shows each button
 * naming its OWN offer rather than one label rendered twice.
 */
const OFFERS: TopUpOfferList = {
  items: [
    {
      id: "offer-small",
      amount_minor_units: 2_500,
      currency: "EUR",
      minor_unit_exponent: 2,
      label: "Small top-up",
    },
    {
      id: "offer-large",
      amount_minor_units: 10_000,
      currency: "EUR",
      minor_unit_exponent: 2,
      label: "Large top-up",
    },
    {
      id: "offer-yen",
      amount_minor_units: 1_250,
      currency: "JPY",
      minor_unit_exponent: 0,
      label: "Yen pack",
    },
  ],
};

const CHECKOUT_URL = "https://pay.example.test/checkout/y1";

/** A payment the OLDEST way round: the newest first, as the operation returns them. */
function payment(overrides: Partial<PaymentIntent> = {}): PaymentIntent {
  return {
    id: "pay-1",
    status: "checkout_open",
    amount_minor_units: 2_500,
    currency: "EUR",
    minor_unit_exponent: 2,
    checkout_url: CHECKOUT_URL,
    created_at: "2026-09-20T09:00:00Z",
    expires_at: "2026-09-20T09:30:00Z",
    ...overrides,
  };
}

const PAYMENTS: PaymentIntentPage = {
  items: [
    payment(),
    payment({
      id: "pay-2",
      status: "succeeded",
      amount_minor_units: 10_000,
      checkout_url: "https://pay.example.test/checkout/y2",
      created_at: "2026-09-18T09:00:00Z",
    }),
    payment({
      id: "pay-3",
      status: "created",
      amount_minor_units: 1_250,
      currency: "JPY",
      // The exponent travels with the PAYMENT, not with the offer: this one is
      // a yen payment, so its 1,250 minor units are 1,250 yen and not 12.50 of
      // anything. A table that read the exponent off the live price list, or
      // that assumed two places, would render this row as a different amount.
      minor_unit_exponent: 0,
      checkout_url: null,
      created_at: "2026-09-17T09:00:00Z",
    }),
  ],
  next_cursor: "c-payments-2",
  has_more: false,
};

/** The `idempotency_key` of every create call, in order. */
function sentKeys(): readonly string[] {
  return seam.createPaymentForOffer.mock.calls.map(
    ([request]) => (request as { idempotency_key: string }).idempotency_key,
  );
}

/** The top-up buttons, matched on the word the screen builds their names from. */
function topUpButtons(wrapper: VueWrapper) {
  return wrapper.findAll("button").filter((button) => button.text().startsWith("Add "));
}

beforeAll(() => {
  // Loom's `SegmentedControl` sizes its indicator on mount and jsdom ships no
  // `ResizeObserver`. This screen renders none today; the stub keeps a filter
  // added later from discovering the gap as a crash.
  class NoopResizeObserver {
    observe(): void {}
    unobserve(): void {}
    disconnect(): void {}
  }
  vi.stubGlobal("ResizeObserver", NoopResizeObserver);
});

const mounted: VueWrapper[] = [];

beforeEach(() => {
  for (const mock of Object.values(seam)) mock.mockReset();
  checkout.sendToCheckout.mockReset();
  seam.getSessionResult.mockResolvedValue(
    ok({ class: "user", account_id: "a0000000-0000-4000-8000-0000000000a1" }),
  );
  seam.fetchTopUpOffers.mockResolvedValue(ok(OFFERS));
  seam.fetchPaymentIntents.mockResolvedValue(ok(PAYMENTS));
  document.body.innerHTML = "";
});

afterEach(() => {
  while (mounted.length > 0) mounted.pop()!.unmount();
  document.body.innerHTML = "";
});

async function mountAt(
  path: string,
): Promise<{ wrapper: VueWrapper; router: ReturnType<typeof createConsoleRouter> }> {
  const pinia = createPinia();
  setActivePinia(pinia);
  const router = createConsoleRouter(createMemoryHistory());
  await router.push(path);
  await router.isReady();
  const wrapper = mount(PaymentsPage, {
    attachTo: document.body,
    global: { plugins: [router, pinia] },
  });
  mounted.push(wrapper);
  await flushPromises();
  await flushPromises();
  return { wrapper, router };
}

describe("PaymentsPage", () => {
  it("keeps the console's architecture roster", async () => {
    const { wrapper } = await mountAt("/payments");
    await flushPromises();
    expect(describeViolations(inspect(wrapper.element.ownerDocument, "/payments"))).toBe("");
  });

  it("renders the payments list with the server's own fields, one row each", async () => {
    const { wrapper } = await mountAt("/payments");

    const table = wrapper.findAll("table").at(-1)!;
    expect(table.find("caption").text()).toBe("Payments made by this account, newest first");
    expect(table.findAll("th[scope='col']").map((header) => header.text())).toEqual([
      "Created",
      "Amount",
      "Currency",
      "Status",
      "Checkout expires",
      "Checkout",
    ]);
    expect(table.findAll("th[scope='row']")).toHaveLength(3);

    // The status cell is the presentation map's phrase and never the contract's
    // token: `checkout_open` is not a sentence a customer can act on.
    const statuses = wrapper.findAll("tbody tr").map((row) => row.get('[data-cell$="c3"]').text());
    expect(statuses[0]).toContain("Waiting at your provider");
    expect(statuses[1]).toContain("Paid");
    expect(statuses[2]).toContain("Checkout not open yet");
    // And the token is nowhere in the CELL that would have shown it. Scanned
    // per cell rather than over the page, because the page's own prose is
    // allowed to contain the English word ("nothing here decides that a payment
    // succeeded") — it is the contract's own TOKEN that must never reach a
    // reader, and the cell is where a token would surface if the map were
    // bypassed.
    for (const [index, token] of ["checkout_open", "succeeded", "created"].entries()) {
      expect(statuses[index], token).not.toContain(token);
    }

    // The amount cell places the decimal point from the exponent the PAYMENT
    // carried, and the third row is the one that proves it: a yen payment's
    // 1,250 minor units are 1,250 yen, so a cell that assumed two decimal
    // places — or that read the exponent off the live price list instead of off
    // the row — would render it as `12.50`.
    const amounts = wrapper.findAll("tbody tr").map((row) => row.get('[data-cell$="c1"]').text());
    expect(amounts).toEqual(["25.00", "100.00", "1,250"]);

    // The timestamp is the contract's own string in a `<time datetime>`, and the
    // expiry is the PAYMENT's field rather than a deadline this page computed.
    expect(table.find("tbody tr time").attributes("datetime")).toBe("2026-09-20T09:00:00Z");
    expect(table.text()).toContain("2026-09-20T09:30:00Z");
  });

  it("names each offer's price in the offer's own currency and exponent", async () => {
    const { wrapper } = await mountAt("/payments");

    expect(topUpButtons(wrapper).map((button) => button.text())).toEqual([
      "Add 25.00 EUR — Small top-up",
      "Add 100.00 EUR — Large top-up",
      "Add 1,250 JPY — Yen pack",
    ]);
    // No symbol, and no currency table: the ISO code is the contract's own name
    // for the unit, and the exponent is the server's own answer to where the
    // decimal point goes.
    expect(wrapper.text()).not.toMatch(/[€$£]/);
  });

  it("posts the offer and nothing else, so the client cannot price its own top-up", async () => {
    seam.createPaymentForOffer.mockResolvedValue(ok(payment()));

    const { wrapper } = await mountAt("/payments");
    await topUpButtons(wrapper)[0]!.trigger("click");
    await flushPromises();

    expect(seam.createPaymentForOffer).toHaveBeenCalledTimes(1);
    const request = seam.createPaymentForOffer.mock.calls[0]![0] as Record<string, unknown>;
    // Asserted over the WHOLE request rather than over the two keys expected:
    // `CreatePaymentIntentRequest` carries no amount and no currency, and the
    // day one is added to this call is the day a client can charge itself one
    // minor unit.
    expect(Object.keys(request).sort()).toEqual(["idempotency_key", "offer"]);
    expect(request.offer).toBe("offer-small");
    expect(request).not.toHaveProperty("amount_minor_units");
    expect(request).not.toHaveProperty("currency");
  });

  it("hands the provider's URL to the browser, verbatim and unfetched", async () => {
    seam.createPaymentForOffer.mockResolvedValue(ok(payment()));

    const { wrapper } = await mountAt("/payments");
    await topUpButtons(wrapper)[1]!.trigger("click");
    await flushPromises();

    // The URL goes to the browser and nowhere else: this console does not fetch
    // it, does not parse a session id out of it, and does not decide whether it
    // looks reasonable. `sendToCheckout` is the whole of what happens to it.
    expect(checkout.sendToCheckout).toHaveBeenCalledTimes(1);
    expect(checkout.sendToCheckout).toHaveBeenCalledWith(CHECKOUT_URL);

    // And the same URL is a real link on the row, so a customer who stays on
    // this screen — or comes back to it — can reach their checkout again. The
    // contract says a returning customer is sent back to this same URL. The row
    // carrying it is the first one, which is `checkout_open`: a link the screen
    // offers only while the provider still has something to confirm.
    const hrefs = wrapper
      .findAll("a")
      .map((link) => link.attributes("href"))
      .filter((href) => href !== undefined);
    expect(hrefs).toContain(CHECKOUT_URL);
  });

  it("does not send the browser to a checkout the provider has already settled", async () => {
    // The converged answer the contract's `idempotency_key` note warns about: a
    // repeated key is answered with the payment it already names, and that
    // payment "may be in any state by then, including one that has already
    // succeeded". A customer who has paid must not be walked back onto a
    // provider page for a payment that is over — so neither is the browser
    // sent there automatically NOR is the link left live on the row. It reads
    // as closed instead, which is the one answer that is true of every settled
    // state: the provider's page can no longer change this payment.
    seam.createPaymentForOffer.mockResolvedValue(
      ok(payment({ status: "succeeded", checkout_url: "https://pay.example.test/checkout/y2" })),
    );

    const { wrapper } = await mountAt("/payments");
    await topUpButtons(wrapper)[0]!.trigger("click");
    await flushPromises();

    expect(checkout.sendToCheckout).not.toHaveBeenCalled();
    expect(wrapper.text()).toContain("Paid");
    const hrefs = wrapper
      .findAll("a")
      .map((link) => link.attributes("href"))
      .filter((href) => href !== undefined);
    expect(hrefs).not.toContain("https://pay.example.test/checkout/y2");
    expect(wrapper.text()).toContain("Checkout closed");
  });

  it("says 'not open yet' rather than rendering a dead link for a payment with no checkout", async () => {
    const { wrapper } = await mountAt("/payments");

    // The third row is `created` with a null `checkout_url` — the durable
    // payment the contract writes BEFORE it calls the provider, so a 503 has
    // something to converge on. There is nowhere to send the customer yet, and
    // an anchor with no href is a link a keyboard user can focus and cannot use.
    const cell = wrapper.findAll("tbody tr")[2]!.get('[data-cell$="c5"]');
    expect(cell.text()).toContain("Not open yet");
    expect(cell.find("a").exists()).toBe(false);
    // The row is still rendered: the payment exists, and hiding it would be the
    // console deciding that a payment with no checkout is not a payment.
    expect(cell.text().trim()).not.toBe("");
  });

  describe("the idempotency key", () => {
    it("is REUSED while the top-up is unfinished, so a retry is the same request", async () => {
      // A dropped response, then a 503 from a provider that never answered.
      // Neither is an answer, so neither ends the act: the contract says the
      // payment is durable in `created` with a null `checkout_url` and that a
      // retry under the same key converges on it.
      seam.createPaymentForOffer.mockResolvedValueOnce({
        ok: false,
        failure: { kind: "transport", error: new TypeError("Failed to fetch") },
      });
      seam.createPaymentForOffer.mockResolvedValueOnce({
        ok: false,
        failure: apiFailure("upstream_unavailable"),
      });
      seam.createPaymentForOffer.mockResolvedValueOnce(ok(payment()));

      const { wrapper } = await mountAt("/payments");
      await topUpButtons(wrapper)[0]!.trigger("click");
      await flushPromises();
      await topUpButtons(wrapper)[0]!.trigger("click");
      await flushPromises();
      await topUpButtons(wrapper)[0]!.trigger("click");
      await flushPromises();

      const keys = sentKeys();
      expect(keys).toHaveLength(3);
      // One act, one key — three times. A key regenerated on either retry would
      // be a second payment for one top-up.
      expect(keys[1]).toBe(keys[0]);
      expect(keys[2]).toBe(keys[0]);
    });

    it("is RETIRED once the customer has a checkout, so funding again funds again", async () => {
      seam.createPaymentForOffer.mockResolvedValue(ok(payment()));

      const { wrapper } = await mountAt("/payments");
      await topUpButtons(wrapper)[0]!.trigger("click");
      await flushPromises();
      await topUpButtons(wrapper)[0]!.trigger("click");
      await flushPromises();

      const keys = sentKeys();
      expect(keys).toHaveLength(2);
      // The second click on the same offer is a NEW top-up: the first one ended
      // when the provider's URL came back, and a reused key here would answer
      // "already done" while the customer waited for money that is not coming.
      expect(keys[1]).not.toBe(keys[0]);
    });

    it("is NEW for a different offer, because that is a different purchase", async () => {
      seam.createPaymentForOffer.mockResolvedValue(ok(payment()));

      const { wrapper } = await mountAt("/payments");
      await topUpButtons(wrapper)[0]!.trigger("click");
      await flushPromises();
      await topUpButtons(wrapper)[1]!.trigger("click");
      await flushPromises();

      const requests = seam.createPaymentForOffer.mock.calls.map(
        ([request]) => request as { offer: string; idempotency_key: string },
      );
      expect(requests.map((request) => request.offer)).toEqual(["offer-small", "offer-large"]);
      expect(requests[1]!.idempotency_key).not.toBe(requests[0]!.idempotency_key);
    });

    it("is not put in the URL, where it would outlive the customer's intent", async () => {
      // A key in the query string is copied into history and into any link the
      // customer shares, and ADR 0012 §2 permits exactly one storage key in this
      // app — which is not a payment's. The URL after a top-up is the URL before
      // it, whatever else changed.
      seam.createPaymentForOffer.mockResolvedValue(ok(payment()));

      const { wrapper, router } = await mountAt("/payments");
      await topUpButtons(wrapper)[0]!.trigger("click");
      await flushPromises();

      expect(router.currentRoute.value.fullPath).toBe("/payments");
      expect(JSON.stringify(router.currentRoute.value.query)).not.toContain("key");
    });
  });

  it("disables the top-up controls while a request is in flight, so a click cannot double-fire", async () => {
    // The idempotency key is the BACKSTOP, not the only defence. The first
    // defence is that the customer cannot press the button twice: every top-up
    // button is disabled for the whole of the in-flight window, and the handler
    // refuses a second request that got past them anyway.
    seam.createPaymentForOffer.mockReturnValue(new Promise(() => {}));

    const { wrapper } = await mountAt("/payments");
    const buttons = topUpButtons(wrapper);
    expect(buttons).toHaveLength(3);
    // Before the click they are live, so the assertion below is a change rather
    // than a page that renders every control disabled.
    for (const button of buttons) expect(button.attributes("disabled")).toBeUndefined();

    await buttons[0]!.trigger("click");
    await flushPromises();

    expect(seam.createPaymentForOffer).toHaveBeenCalledTimes(1);
    // ALL of them, not only the one that was pressed: a customer who clicks a
    // different offer while the first is in flight would otherwise open two
    // payments, and the key is per-attempt and cannot converge two offers.
    for (const button of buttons) expect(button.attributes("disabled")).toBeDefined();
    // The pressed one says it is working rather than merely unavailable.
    expect(buttons[0]!.attributes("aria-busy")).toBeDefined();

    // The guard itself, exercised rather than assumed: a second click on a
    // button that was somehow live still sends one request.
    await buttons[0]!.trigger("click");
    await flushPromises();
    expect(seam.createPaymentForOffer).toHaveBeenCalledTimes(1);
  });

  describe("the deployment publishes no offer", () => {
    it("renders no top-up control at all, and says why", async () => {
      seam.fetchTopUpOffers.mockResolvedValue(ok({ items: [] } satisfies TopUpOfferList));

      const { wrapper } = await mountAt("/payments");

      // ABSENT, not broken. A button that cannot work is worse than no button:
      // it is a promise the deployment has not made.
      expect(topUpButtons(wrapper)).toHaveLength(0);
      expect(wrapper.text()).not.toMatch(/no offers? (are|is) available/i);
      expect(wrapper.text()).toMatch(/publishes? no top-up offer/i);
      // And the sentence says it is a valid deployment rather than an error, so
      // a customer of a deployment that funds outside this console is not told
      // something has gone wrong.
      expect(wrapper.text()).toMatch(/valid deployment/i);

      // The payments list is untouched by it: an account's history is not a
      // statement about what is currently for sale.
      expect(wrapper.findAll("table")).toHaveLength(1);
      expect(wrapper.findAll("th[scope='row']")).toHaveLength(3);
    });

    it("does not say it before the deployment has been asked", async () => {
      // The read is fired from `onMounted`, after the first render, so there is
      // a window in which `loading` is false and nothing has answered. A page
      // that read "this deployment publishes no offers" there would be stating
      // a fact about a deployment it had not heard from — on the one screen
      // where every claim is about money.
      seam.fetchTopUpOffers.mockReturnValue(new Promise(() => {}));

      const { wrapper } = await mountAt("/payments");
      expect(wrapper.text()).not.toMatch(/publishes? no top-up offer/i);
      expect(topUpButtons(wrapper)).toHaveLength(0);
      expect(wrapper.text()).toMatch(/Reading this deployment's top-up offers/i);
    });

    it("reports an offer read that failed rather than reporting no offers", async () => {
      seam.fetchTopUpOffers.mockResolvedValue({ ok: false, failure: apiFailure("internal") });

      const { wrapper } = await mountAt("/payments");

      expect(wrapper.text()).not.toMatch(/publishes? no top-up offer/i);
      expect(wrapper.find('[role="alert"]').text()).toContain(CONSOLE_BEHAVIOUR.internal.title);
      // The payments table is its own read and its own answer.
      expect(wrapper.findAll("table")).toHaveLength(1);
    });
  });

  describe("coming back from the provider's checkout", () => {
    it("says it is waiting for the provider, and that returning proves nothing", async () => {
      const { wrapper } = await mountAt("/payments");

      // The screen the customer lands on when the browser sends them back while
      // the provider has not spoken. The status is the server's, unchanged —
      // `checkout_open` is exactly where it was left — and the copy says so out
      // loud rather than letting the return read as a confirmation.
      expect(wrapper.text()).toMatch(/waiting for your provider to confirm/i);
      expect(wrapper.text()).toMatch(/retur\w* (from|to) (a |the )?checkout/i);
      expect(wrapper.text()).toMatch(/does not mark anything paid/i);
      // The claim is made about the provider's own message, which is the only
      // thing that can move a payment.
      expect(wrapper.text()).toMatch(/signature-verified/i);
    });

    it("does not treat the customer's return as a state change", async () => {
      const { wrapper } = await mountAt("/payments");
      const before = wrapper.findAll("tbody tr").map((row) => row.text());

      // Nothing about the browser's return is persisted, so a re-read is the
      // only thing that happens — and a re-read of the same server state renders
      // the same page. If this screen had inferred a funded state from the
      // return, the second render would differ from the first.
      await flushPromises();
      await flushPromises();
      expect(wrapper.findAll("tbody tr").map((row) => row.text())).toEqual(before);
      // The re-read happened — so the screen did look — and the rows did not
      // move, because the server's answer did not move. The status cell is the
      // contract's `checkout_open` rendered as the phrase it earns, and the
      // provider link is still there to be used again.
      expect(seam.fetchPaymentIntents.mock.calls.length).toBeGreaterThan(0);
      expect(before[0]).toContain("Waiting at your provider");
    });

    it("does not render the waiting card when the provider has spoken", async () => {
      seam.fetchPaymentIntents.mockResolvedValue(
        ok({
          ...PAYMENTS,
          items: [
            payment({ id: "pay-9", status: "succeeded" }),
            payment({ id: "pay-10", status: "refunded" }),
          ],
        }),
      );

      const { wrapper } = await mountAt("/payments");

      // A settled payment is not waiting for anything. The card is keyed on the
      // states the provider has not spoken about, so it disappears the moment
      // none of them is on the list — a permanent "we are waiting" banner would
      // teach a customer to doubt a confirmation they have already got.
      expect(wrapper.text()).not.toMatch(/waiting for your provider to confirm/i);
      expect(wrapper.text()).toContain("Paid");
      expect(wrapper.text()).toContain("Fully refunded");
    });

    it("never claims success on the strength of the create call alone", async () => {
      // The create answered with a payment the provider has not confirmed. The
      // page may say what that payment's status IS — because that is what the
      // server said — and it may not say the money arrived.
      seam.createPaymentForOffer.mockResolvedValue(
        ok(payment({ status: "created", checkout_url: null })),
      );

      const { wrapper } = await mountAt("/payments");
      await topUpButtons(wrapper)[0]!.trigger("click");
      await flushPromises();

      expect(wrapper.text()).toContain("Checkout not open yet");
      // What the answer paragraph may say, and it is the explanation for
      // `created` rather than a sentence about money that moved. Asserted
      // positively because the negative is unreadable — a bare "must not match
      // /succeeded/" also bans the page's own disclaimer prose, which has to be
      // able to say that returning from a checkout does not mark anything paid.
      expect(wrapper.text()).toContain(
        "We have recorded this payment but its checkout is not open yet. Nothing has been charged.",
      );
      // The succeeded explanation credits the account, and it is the sentence
      // that would appear if this page had read its own request as a payment.
      expect(wrapper.text()).not.toMatch(/has been credited/i);
      expect(wrapper.text()).not.toMatch(/your payment is confirmed/i);
      // And with no checkout to visit, the browser is not sent anywhere.
      expect(checkout.sendToCheckout).not.toHaveBeenCalled();
    });
  });

  it("pages with the server's cursor, in the URL, so Back undoes a page change", async () => {
    seam.fetchPaymentIntents.mockResolvedValue({
      ok: true,
      data: { ...PAYMENTS, has_more: true, next_cursor: "c-payments-2" },
    });

    const { wrapper, router } = await mountAt("/payments");
    expect(seam.fetchPaymentIntents.mock.lastCall?.[0]).toEqual({ query: {} });

    const next = wrapper.findAll("nav a").find((link) => link.text() === "Next");
    expect(next, "the pager offers a Next when the server says there is more").toBeDefined();
    await next!.trigger("click");
    await flushPromises();

    // The cursor is the server's string, sent back verbatim, and the URL is the
    // only place it lives — which is what makes Back a page change rather than a
    // page number the console invented.
    expect(router.currentRoute.value.query.after).toBe("c-payments-2");
    expect(seam.fetchPaymentIntents.mock.lastCall?.[0]).toEqual({
      query: { after: "c-payments-2" },
    });
    expect(next!.attributes("href")).toContain("after=c-payments-2");

    await router.back();
    await flushPromises();
    await flushPromises();
    expect(router.currentRoute.value.query.after).toBeUndefined();
    expect(seam.fetchPaymentIntents.mock.lastCall?.[0]).toEqual({ query: {} });
  });

  it("renders every failure the console can produce through the matrix", async () => {
    // ADR 0012 §6, this screen's own slice, driven off the table so a code the
    // contract gains is a code this test has never rendered. The commit is the
    // one write on this screen, and its refusal is the one a customer meets
    // after choosing an offer.
    for (const code of REACHABLE) {
      seam.createPaymentForOffer.mockResolvedValue({ ok: false, failure: apiFailure(code) });

      const { wrapper } = await mountAt("/payments");
      await topUpButtons(wrapper)[0]!.trigger("click");
      await flushPromises();

      const alert = wrapper.find('[role="alert"]');
      expect(alert.exists(), code).toBe(true);
      expect(alert.text(), code).toContain(CONSOLE_BEHAVIOUR[code].title);
      // The offer is still offered: a refusal is about one attempt, and the
      // chooser stays where the customer can pick again.
      expect(topUpButtons(wrapper).length, code).toBeGreaterThan(0);

      const offersRetry = wrapper
        .findAll("button")
        .some((button) => /try again|retry/i.test(button.text()));
      expect(offersRetry, code).toBe(CONSOLE_BEHAVIOUR[code].recovery === "retry");
      const signIn = new Set(
        wrapper
          .findAll("a")
          .filter((link) => /sign in/i.test(link.text()))
          .map((link) => link.attributes("href")),
      );
      expect(signIn.size > 0, code).toBe(CONSOLE_BEHAVIOUR[code].recovery === "sign-in");
      for (const href of signIn) expect(href, code).toContain("/sign-in");

      expect(wrapper.text().includes("req-payments-1"), code).toBe(
        CONSOLE_BEHAVIOUR[code].showRequestId,
      );
    }
  });

  it("offers no way to repair a conflict, because no edit to the request would help", async () => {
    // The row the payment surface added, and the distinction the screen has to
    // honour: `conflict` is the SERVER's state refusing a well-formed request.
    // A retry-with-changes affordance here would invite the customer to rebuild
    // a payload that is not the problem — and the one thing a client must not do
    // with this code is send it again.
    seam.createPaymentForOffer.mockResolvedValue({
      ok: false,
      failure: apiFailure("conflict"),
    });

    const { wrapper } = await mountAt("/payments");
    await topUpButtons(wrapper)[0]!.trigger("click");
    await flushPromises();

    const alert = wrapper.find('[role="alert"]');
    expect(alert.text()).toContain(CONSOLE_BEHAVIOUR.conflict.title);
    expect(alert.text()).not.toMatch(/try again|retry/i);
    expect(wrapper.findAll("button").some((button) => /try again|retry/i.test(button.text()))).toBe(
      false,
    );
    // The request was well-formed and is not to be edited: no amount field, no
    // form, nothing to change.
    expect(wrapper.findAll("input")).toHaveLength(0);
    expect(wrapper.findAll("form")).toHaveLength(0);
  });

  it("offers a retry that re-attempts the SAME offer under the same key", async () => {
    // The matrix's `upstream_unavailable` row declares `retry`, and the
    // contract's own note on that `503` says the payment is durable and that a
    // retry under the same key converges on it. So the button the matrix puts
    // on screen has to actually send the request again — a Try again that did
    // nothing would be an affordance promising a second attempt and silently
    // withholding it, and one that minted a new key would open a second payment
    // for one act.
    seam.createPaymentForOffer.mockResolvedValueOnce({
      ok: false,
      failure: apiFailure("upstream_unavailable"),
    });
    seam.createPaymentForOffer.mockResolvedValueOnce(ok(payment()));

    const { wrapper } = await mountAt("/payments");
    await topUpButtons(wrapper)[0]!.trigger("click");
    await flushPromises();

    const retry = wrapper
      .findAll("button")
      .find((button) => /try again|retry/i.test(button.text()));
    expect(retry, "the 503 row offers a retry").toBeDefined();
    await retry!.trigger("click");
    await flushPromises();

    expect(seam.createPaymentForOffer).toHaveBeenCalledTimes(2);
    const requests = seam.createPaymentForOffer.mock.calls.map(
      ([request]) => request as { offer: string; idempotency_key: string },
    );
    expect(requests.map((request) => request.offer)).toEqual(["offer-small", "offer-small"]);
    expect(requests[1]!.idempotency_key).toBe(requests[0]!.idempotency_key);
  });

  it("collects no instrument data of any kind", async () => {
    const { wrapper } = await mountAt("/payments");

    // The non-negotiable, asserted on the rendered document rather than on the
    // source: the card is entered on the PROVIDER's page, and there is no field
    // for one here. Not a number, not a CVV, not an expiry, not a cardholder's
    // name, and no "billing address" form — a field that collects instrument
    // data is the failure regardless of what it is labelled.
    expect(wrapper.findAll("input")).toHaveLength(0);
    expect(wrapper.findAll("select")).toHaveLength(0);
    expect(wrapper.findAll("textarea")).toHaveLength(0);
    expect(wrapper.findAll("form")).toHaveLength(0);
    // The PROSE is not scanned for the word "card": this screen explains, in two
    // places, that the card is entered on the provider's page and that the
    // console never sees a number — which is the sentence a rule against the
    // word would delete. What is scanned is the language of COLLECTION, which
    // only appears where there is a field to put the answer in.
    expect(wrapper.text()).not.toMatch(
      /\benter your\b|\byour card number\b|\bcvv\b|\bcvc\b|security code|cardholder|expiry date|billing address|\biban\b/i,
    );
  });

  it("keeps nothing about a payment in browser storage", async () => {
    // ADR 0012 §2 names exactly one permitted storage key in this app, and it is
    // the theme's. A payment's key, status or id in `localStorage` would outlive
    // the tab, outlive the session, and survive into a later visit where the key
    // would converge a fresh top-up on an old payment.
    const setItem = vi.spyOn(Storage.prototype, "setItem");
    seam.createPaymentForOffer.mockResolvedValue(ok(payment()));

    const { wrapper } = await mountAt("/payments");
    await topUpButtons(wrapper)[0]!.trigger("click");
    await flushPromises();

    expect(setItem).not.toHaveBeenCalled();
    setItem.mockRestore();
  });
});
