// The dashboard, against the composed overview the server sent.
//
// The rule this screen exists to hold is NEGATIVE — it must not do arithmetic
// on the figures — so the assertion that matters is the one that a sum would
// fail. A test asserting the numbers are present passes just as happily when a
// `+` appears between them.
import { flushPromises, mount } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { createMemoryHistory, createRouter } from "vue-router";

const api = vi.hoisted(() => ({ fetchAccountOverview: vi.fn() }));
vi.mock("@/lib/api", () => api);

import DashboardPage from "@/pages/DashboardPage.vue";
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

describe("DashboardPage", () => {
  beforeEach(() => {
    api.fetchAccountOverview.mockReset();
  });

  it("renders the composed overview as the server sent it", async () => {
    api.fetchAccountOverview.mockResolvedValue({ ok: true, data: OVERVIEW });

    const wrapper = await mountPage();

    expect(wrapper.text()).toContain("Northwind");
    expect(wrapper.text()).toContain("12");
    expect(wrapper.text()).toContain("Active");
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
