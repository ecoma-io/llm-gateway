// The dashboard, against the composed overview the server sent.
//
// The rule this screen exists to hold is NEGATIVE — it must not do arithmetic
// on the figures — so the assertion that matters is the one that a sum would
// fail. A test asserting the numbers are present passes just as happily when a
// `+` appears between them.
import { flushPromises, mount, type VueWrapper } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { createMemoryHistory, createRouter } from "vue-router";

const api = vi.hoisted(() => ({ fetchAccountOverview: vi.fn() }));
vi.mock("@/lib/api", () => api);

import DashboardPage from "@/pages/DashboardPage.vue";
import StatusBadge from "@/modules/status/StatusBadge.vue";
import { ACCOUNT_STATE_PRESENTATION } from "@/modules/status/presentation";
import type { AccountOverview } from "@ecoma-io/llm-gateway-console-api-client";

/** A bucket whose three balances disagree with any sum, on purpose. */
const BUCKET = {
  id: "b0000000-0000-4000-8000-0000000000b1",
  kind: "account" as const,
  account_id: "a0000000-0000-4000-8000-0000000000a1",
  status: "active" as const,
  balances: {
    settled: { minor_units: 120_000 },
    held: { minor_units: 20_000 },
    available: { minor_units: 100_000 },
  },
  version: 4,
  created_at: "2026-09-01T00:00:00Z",
};

const OVERVIEW: AccountOverview = {
  account: {
    id: "a0000000-0000-4000-8000-0000000000a1",
    name: "Northwind",
    state: "active",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-09-01T00:00:00Z",
  },
  user_count: 12,
  active_api_key_count: 3,
  open_finding_count: 2,
  payg_balances: [BUCKET],
  subscriptions: [
    {
      id: "s0000000-0000-4000-8000-0000000000s1",
      account_id: "a0000000-0000-4000-8000-0000000000a1",
      plan_version_id: "pv-1",
      state: "active",
      created_at: "2026-02-01T00:00:00Z",
    },
  ],
};

function failure(code: "internal" | "service_unavailable" | "not_found") {
  return {
    ok: false as const,
    failure: {
      kind: "api" as const,
      unauthenticated: false,
      envelope: { error: { code, message: "server's words" }, request_id: "req-dash-1" },
    },
  };
}

async function mountPage() {
  setActivePinia(createPinia());
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/", component: { template: "<div />" } },
      { path: "/accounting", component: { template: "<div />" } },
    ],
  });
  await router.push("/");
  await router.isReady();
  const wrapper = mount(DashboardPage, { global: { plugins: [router] } });
  await flushPromises();
  await flushPromises();
  return wrapper;
}

/**
 * The figure a Loom `Card` shows, read from the card's BODY.
 *
 * `Card` has no heading element and no `aria-label` — its title is a `<p>` in
 * the header slot — and it cannot be found by COMPONENT either: Loom's
 * primitives compile to fragments, so `Card` is in the source but never a
 * component instance in the tree, and `findAll(Card)` fails with
 * `Invalid selector [object Object]`. What is left is the card's own element,
 * recognised by the first paragraph under its header being the title.
 *
 * The card's BODY, not the whole card, is the return value, and that is the
 * part that matters: the header also carries the `description` slot, and a
 * `count(...)` description repeats the figure. Reading the whole card and
 * testing `toContain("3")` would then be satisfied by the DESCRIPTION even if
 * the body rendered the wrong number — which is the substring defect this
 * helper exists to end. The body is the last `div` of the card, and the figure
 * is the only `<p>` in it, so its text is the figure and nothing else.
 *
 * The property this has and a string alternative does not: a card whose title
 * was mistyped, or whose figure was read out of the wrong field, is a MISS
 * rather than a pass. `"Live users" + "12"` concatenated into one `toContain`
 * also matches a card titled "Live users1" showing 2.
 */
function cardFigure(wrapper: VueWrapper, title: string): string {
  const found = wrapper
    .findAll("div")
    .filter(
      (candidate) => candidate.element.querySelector(":scope > div > p")?.textContent === title,
    )
    .at(0);
  if (!found) throw new Error(`no Card titled "${title}" was rendered`);
  // The body's `div` is the last direct child of the card root; its only
  // paragraph is the figure.
  const bodyDivs = found.element.querySelectorAll(":scope > div");
  const body = bodyDivs[bodyDivs.length - 1];
  const figure = body?.querySelector("p")?.textContent ?? "";
  return figure.trim();
}

/**
 * The whole card's text, for the claims that are about more than the figure —
 * the account card's name and its state badge, which are not a figure and are
 * not in a body paragraph.
 */
function card(wrapper: VueWrapper, title: string) {
  const found = wrapper
    .findAll("div")
    .filter(
      (candidate) => candidate.element.querySelector(":scope > div > p")?.textContent === title,
    )
    .at(0);
  if (!found) throw new Error(`no Card titled "${title}" was rendered`);
  return found;
}

/** The text of one `aria-label`ed region, so a figure is asserted where it belongs. */
function region(wrapper: VueWrapper, label: string): string {
  return wrapper.find(`section[aria-label="${label}"]`).text();
}

describe("DashboardPage", () => {
  beforeEach(() => {
    api.fetchAccountOverview.mockReset();
  });

  it("renders the composed overview as the server sent it", async () => {
    api.fetchAccountOverview.mockResolvedValue({ ok: true, data: OVERVIEW });

    const wrapper = await mountPage();

    // The account's own name, on the account card. The state badge is selected
    // by COMPONENT: "Active" also appears in the "Active API keys" card title
    // two elements away, so a substring read over the whole page would be
    // satisfied by a card title rather than by a state.
    const accountCard = card(wrapper, "Account");
    expect(accountCard.text()).toContain("Northwind");
    const stateBadge = accountCard.getComponent(StatusBadge);
    expect(stateBadge.text()).toBe(ACCOUNT_STATE_PRESENTATION.active.label);
    // The state is the PRESENTATION's word for it, and for `active` that word
    // is the contract token — so the negative is stated for the states where
    // the two differ rather than as a claim that would be vacuous here.
    expect(stateBadge.text()).not.toBe("suspended");

    // The three counts, each read as the WHOLE of the card body and compared
    // for EQUALITY rather than for containment. `toContain` here is still the
    // old defect one level down: a body of "312" contains "3", so a card
    // rendering a concatenation rather than a figure would satisfy it, and a
    // `count(...)` description repeating the figure would satisfy it too.
    expect(cardFigure(wrapper, "Live users")).toBe("12");
    expect(cardFigure(wrapper, "Active API keys")).toBe("3");
    expect(cardFigure(wrapper, "Open reconciliation findings")).toBe("2");
  });

  it("renders each count from its own field, so a card cannot show another's figure", async () => {
    // The negative a substring assertion cannot express. The dashboard shows
    // three counts off three fields; a screen that read `user_count` into all
    // three would satisfy `toContain("12")` three times over. The counts here
    // are far apart, so a field read into the wrong card is a DIFFERENT number
    // rather than the same one — which is what makes the negative meaningful.
    api.fetchAccountOverview.mockResolvedValue({
      ok: true,
      data: { ...OVERVIEW, user_count: 7, active_api_key_count: 4, open_finding_count: 9 },
    });

    const wrapper = await mountPage();

    expect(cardFigure(wrapper, "Live users")).toBe("7");
    expect(cardFigure(wrapper, "Active API keys")).toBe("4");
    expect(cardFigure(wrapper, "Open reconciliation findings")).toBe("9");
    // And the figures the fixture used to carry are gone from the page, so a
    // card that rendered both its own figure and its neighbour's is not
    // satisfying the assertions above.
    const figures = region(wrapper, "Account figures");
    expect(figures).not.toContain("12");
  });

  it("renders each balance from its own field and never sums them", async () => {
    api.fetchAccountOverview.mockResolvedValue({ ok: true, data: OVERVIEW });

    const wrapper = await mountPage();
    const text = wrapper.text();

    // All three figures, each from the field that carries it.
    expect(text).toContain("120,000");
    expect(text).toContain("20,000");
    expect(text).toContain("100,000");
    // The arithmetic a reader could do — 120,000 − 20,000 — is exactly the one
    // the console must not present, because `available` is the server's figure
    // and a client that computed it would be a second, unaudited ledger.
    expect(text).not.toContain("140,000");
    expect(text).not.toContain("160,000");
    // And there is no total across the three, because they answer three
    // different questions.
    expect(text).not.toMatch(/total/i);
  });

  it("renders no balance at all when the account has no pay-as-you-go bucket", async () => {
    api.fetchAccountOverview.mockResolvedValue({
      ok: true,
      data: { ...OVERVIEW, payg_balances: [] },
    });

    const wrapper = await mountPage();

    // An absent bucket is an empty answer, not a failure and not a zero.
    expect(wrapper.text()).toContain("no pay-as-you-go bucket");
    expect(wrapper.text()).not.toContain("120,000");
  });

  it("renders a missing optional count as absent rather than as zero", async () => {
    // A screen that showed `0` for an absent `user_count` would be asserting
    // that the account has no users, which is a claim the server never made.
    api.fetchAccountOverview.mockResolvedValue({
      ok: true,
      data: { account: OVERVIEW.account },
    });

    const wrapper = await mountPage();

    expect(wrapper.text()).toContain("Not reported");
  });

  it("keeps a subscription and a pay-as-you-go bucket as separate arrangements", async () => {
    api.fetchAccountOverview.mockResolvedValue({ ok: true, data: OVERVIEW });

    const wrapper = await mountPage();
    const text = wrapper.text();

    expect(text).toContain("Subscriptions");
    expect(text).toContain("Pay-as-you-go balances");
    // The word that would collapse the two into one figure is the word that
    // must not appear.
    expect(text).not.toMatch(/plan total|combined total/i);
  });

  it("renders an empty account without calling it a failure", async () => {
    api.fetchAccountOverview.mockResolvedValue({
      ok: true,
      data: { account: OVERVIEW.account, subscriptions: [], payg_balances: [] },
    });

    const wrapper = await mountPage();

    expect(wrapper.text()).toContain("no subscription");
    expect(wrapper.find('[role="alert"]').exists()).toBe(false);
  });

  it("renders a failure through FailureView, with the matrix's own wording", async () => {
    api.fetchAccountOverview.mockResolvedValue(failure("internal"));

    const wrapper = await mountPage();
    const alert = wrapper.find('[role="alert"]');

    // The 500 is a dead end: the matrix says `none`, so no retry is offered and
    // the request id IS shown.
    expect(alert.exists()).toBe(true);
    expect(alert.text()).toContain("Something went wrong on our side");
    expect(alert.text()).toContain("req-dash-1");
    expect(wrapper.text()).not.toContain("Try again");
  });

  it("offers a retry for a failure the matrix says is retryable", async () => {
    api.fetchAccountOverview.mockResolvedValue(failure("service_unavailable"));

    const wrapper = await mountPage();

    expect(wrapper.find('[role="alert"]').text()).toContain("Try again");
  });
});
