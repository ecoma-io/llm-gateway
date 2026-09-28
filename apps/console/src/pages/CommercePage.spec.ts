// Commerce: three kinds of thing that are not the same, and no arithmetic at all.
//
// The screen exists to keep a subscription, a grant and a balance apart. A
// screen called "billing" with one table of figures invites the reader to do the
// sum themselves, and this one is built so the sum is not available to do: the
// only money on it is a grant, rendered from one optional field, one row at a
// time. So most of this file is about what the page REFUSES to produce.
//
// Two of those refusals are the kind that look like over-caution until you know
// the contract. A grant with `granted_minor_units` ABSENT is not a grant of
// zero — the field is optional because a grant may be scoped to something this
// plane does not meter, and rendering the absence as `0` asserts the first when
// the server said the second. And a `cancel_at` in the future is DATA, not a
// state: the subscription is still active and usable until that instant passes,
// so a row must not be dressed as `cancelled` on the strength of a date.
import { flushPromises, mount, type VueWrapper } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { createMemoryHistory } from "vue-router";

const seam = vi.hoisted(() => ({
  fetchSubscriptions: vi.fn(),
  fetchEntitlements: vi.fn(),
  getSessionResult: vi.fn(),
}));

vi.mock("@/lib/api", async () => {
  const actual = await vi.importActual<typeof import("@/lib/api")>("@/lib/api");
  return { ...actual, ...seam };
});

import CommercePage from "@/pages/CommercePage.vue";
import { createConsoleRouter } from "@/router";
import { describe as describeViolations, inspect } from "@/lib/arch/roster";
import { CONSOLE_BEHAVIOUR, type ApiErrorCode } from "@/lib/failure-matrix";
import type { ApiFailure, ApiResult } from "@/lib/api";
import type { EntitlementPage, SubscriptionPage } from "@ecoma-io/llm-gateway-console-api-client";

const ok = <T>(data: T): ApiResult<T> => ({ ok: true, data });

function apiFailure(code: ApiErrorCode): ApiFailure {
  return {
    kind: "api",
    unauthenticated: code === "unauthenticated",
    envelope: { error: { code, message: "the server's own words" }, request_id: "req-commerce-1" },
  };
}

/** Every code this console can produce, read off the matrix rather than copied. */
const REACHABLE = Object.entries(CONSOLE_BEHAVIOUR)
  .filter(([, behaviour]) => behaviour.reachable)
  .map(([code]) => code as ApiErrorCode);

/**
 * A far-off `cancel_at`, chosen by YEAR rather than by a clock.
 *
 * The screen's own sentence reads the current year out of `new Date()`, so a
 * timestamp fixed at a literal date would go stale on the first new year and
 * turn this file into a test of the calendar. `new Date(Date.UTC(...))` here
 * reads a field of the machine, never a duration, so the value is the same on
 * every run — which is what the user asked of the fixtures and is also the only
 * way the sentence below can be asserted without a "skip if December" branch.
 */
const FAR_FUTURE = new Date(Date.UTC(2999, 0, 2, 3, 4, 5)).toISOString();

const SUBSCRIPTIONS: SubscriptionPage = {
  items: [
    {
      id: "50000000-0000-4000-8000-0000000005a1",
      account_id: "a0000000-0000-4000-8000-0000000000a1",
      plan_version_id: "40000000-0000-4000-8000-0000000004a1",
      state: "active",
      starts_at: "2026-08-01T00:00:00Z",
      current_period_start: "2026-09-01T00:00:00Z",
      current_period_end: "2026-10-01T00:00:00Z",
      // A cancellation the server has ACCEPTED for a future instant. The
      // subscription is active right now and stays usable until then.
      cancel_at: FAR_FUTURE,
      created_at: "2026-08-01T00:00:00Z",
    },
  ],
  next_cursor: "",
  has_more: false,
};

const ENTITLEMENTS: EntitlementPage = {
  items: [
    {
      id: "60000000-0000-4000-8000-0000000006a1",
      subscription_id: "50000000-0000-4000-8000-0000000005a1",
      grant_definition_id: "70000000-0000-4000-8000-0000000007a1",
      cycle: 1,
      scope: "models:inference",
      // An ABSENT grant. The field is optional because a grant may be scoped
      // to something this plane does not meter; this one is.
      granted_minor_units: undefined,
      state: "active",
      period_start: "2026-09-01T00:00:00Z",
      period_end: "2026-10-01T00:00:00Z",
      created_at: "2026-09-01T00:00:00Z",
    },
    {
      id: "60000000-0000-4000-8000-0000000006a2",
      subscription_id: "50000000-0000-4000-8000-0000000005a1",
      grant_definition_id: "70000000-0000-4000-8000-0000000007a2",
      cycle: 1,
      scope: "models:inference",
      // A grant of ZERO, which is a real and meaningful thing: the server
      // decided this cycle grants nothing. Not the same fact as the row above,
      // and not allowed to render the same way.
      granted_minor_units: 0,
      state: "active",
      period_start: "2026-09-01T00:00:00Z",
      period_end: "2026-10-01T00:00:00Z",
      created_at: "2026-09-01T00:00:00Z",
    },
    {
      id: "60000000-0000-4000-8000-0000000006a3",
      subscription_id: "50000000-0000-4000-8000-0000000005a1",
      grant_definition_id: "70000000-0000-4000-8000-0000000007a3",
      cycle: 2,
      granted_minor_units: 100000,
      state: "active",
      period_start: "2026-10-01T00:00:00Z",
      period_end: "2026-11-01T00:00:00Z",
      created_at: "2026-10-01T00:00:00Z",
    },
  ],
  next_cursor: "",
  has_more: false,
};

/** The three lists' rendered money, in document order. */
function renderedFigures(wrapper: VueWrapper): readonly string[] {
  return wrapper
    .findAll("table")[1]!
    .findAll("tbody [data-cell]")
    .map((cell) => cell.text());
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
  seam.getSessionResult.mockResolvedValue(
    ok({ class: "user", account_id: "a0000000-0000-4000-8000-0000000000a1" }),
  );
  seam.fetchSubscriptions.mockResolvedValue(ok(SUBSCRIPTIONS));
  seam.fetchEntitlements.mockResolvedValue(ok(ENTITLEMENTS));
  document.body.innerHTML = "";
});

// One sweep for every `attachTo` wrapper, unconditional: a test that failed
// half way through is exactly the one that would have left its wrapper alive for
// the next test's `beforeEach` to re-arm.
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
  const wrapper = mount(CommercePage, {
    attachTo: document.body,
    global: { plugins: [router, pinia] },
  });
  mounted.push(wrapper);
  await flushPromises();
  await flushPromises();
  return { wrapper, router };
}

describe("CommercePage", () => {
  it("keeps the console's architecture roster", async () => {
    // The roster is read off the RENDERED document, so this is a claim about
    // what the page actually produced rather than about what it imported. A
    // list screen that rendered no table, or a table nobody declared a layer
    // for, or a pager with no table to move, is a defect the reader would
    // experience and no type would catch.
    const { wrapper } = await mountAt("/commerce");
    await flushPromises();

    expect(describeViolations(inspect(wrapper.element.ownerDocument, "/commerce"))).toBe("");
  });

  it("keeps three lists apart, because they are three different things", async () => {
    const { wrapper } = await mountAt("/commerce");

    // A subscription, a grant and a balance are not one table. A screen that
    // merged them would be asking the reader to hold the difference in their
    // head, which is the reading the header says this page exists to avoid.
    expect(wrapper.findAll("table > caption").map((caption) => caption.text())).toEqual([
      "Subscriptions in this account",
      "Grants in this account, one row per cycle",
    ]);
    // And the two lists carry different facts per row: a subscription row ends
    // in a date and a grant row in a figure, so a reader cannot read one table
    // as the other by shape.
    expect(
      wrapper
        .findAll("table")[0]!
        .findAll("th[scope='col']")
        .map((h) => h.text()),
    ).toEqual(["Subscription", "Plan version", "State", "Period ends", "Ends"]);
    expect(
      wrapper
        .findAll("table")[1]!
        .findAll("th[scope='col']")
        .map((h) => h.text()),
    ).toContain("Granted");
  });

  describe("an absent grant is not a grant of zero", () => {
    it("renders the two as different facts on the same screen", async () => {
      const { wrapper } = await mountAt("/commerce");
      const rows = wrapper.findAll("table")[1]!.findAll("tbody tr");
      expect(rows).toHaveLength(3);

      // Row one: the field is absent, so the amount is UNRENDERABLE and says
      // so out loud rather than showing digits. The screen adds a cell label so
      // the reader can tell which of the two cases they are looking at.
      const absent = rows[0]!;
      expect(absent.text()).toContain("—");
      expect(absent.text()).toContain("minor units");
      expect(absent.text()).not.toMatch(/\b0\b/);
      // The unrenderable amount carries an accessible name, because "—" read
      // aloud is a dash and not a fact about the grant.
      const sentinel = absent.get("[aria-label]");
      expect(sentinel.attributes("aria-label")).toMatch(/amount too large to display exactly/i);

      // Row two: a real zero, rendered as digits. Same column, same unit, a
      // different claim — the server decided this cycle grants nothing, which
      // is an answer rather than a gap.
      const zero = rows[1]!;
      expect(zero.text()).toContain("0");
      expect(zero.text()).not.toContain("—");
      expect(zero.find("[aria-label]").exists()).toBe(false);

      // The two rows must not be distinguishable by accident: if the absent
      // grant had rendered as `0`, this screen would be reporting a metered
      // dimension the server never said existed.
      expect(absent.text()).not.toBe(zero.text());
    });

    it("keeps the sentinel out of the accessible name of a real amount", async () => {
      // The third grant is a plain 100,000 with no `aria-label` override, so a
      // screen reader hears the digits the sighted reader sees and the unit is
      // named once by the column. An override here would be a screen claiming
      // an amount it can render exactly is one it cannot.
      const { wrapper } = await mountAt("/commerce");
      const third = wrapper.findAll("table")[1]!.findAll("tbody tr")[2]!;
      expect(third.text()).toContain("100,000");
      expect(third.find("[aria-label]").exists()).toBe(false);
    });
  });

  it("renders a scheduled end as data, never as a cancelled state", async () => {
    const { wrapper } = await mountAt("/commerce");
    const row = wrapper.findAll("table")[0]!.get("tbody tr");

    // The state cell is the server's `state`, full stop. A `cancel_at` in the
    // future says an END was agreed; it does not say the arrangement stopped,
    // and a row badged `Cancelled` beside a still-valid period would tell an
    // operator to stop paying for something that is still running.
    const stateCell = row.get('[data-cell="r0c2"]');
    expect(stateCell.text()).toContain("Active");
    expect(stateCell.text()).not.toMatch(/cancel/i);
    expect(SUBSCRIPTIONS.items[0]!.state).toBe("active");

    // The scheduled end gets its OWN column, because a reader asking "when does
    // this end" must not have to read a date out of a status badge.
    const endsCell = row.get('[data-cell="r0c4"]');
    expect(endsCell.text()).toContain("2999");
    expect(wrapper.findAll("table")[0]!.get("thead").text()).toContain("Ends");

    // And the row is a scheduled end in the sense the screen defines: the
    // instant is still in the FUTURE, and it is stated as such. A `cancel_at`
    // already passed is not a scheduled cancellation, and the sentence must
    // not appear for one.
    expect(FAR_FUTURE > new Date().toISOString()).toBe(true);
    const sentence = wrapper.text().match(/An "Ends" date in the future[^.]*\./)?.[0];
    expect(sentence, 'the "Ends" column needs its own explanation').toBeDefined();
    expect(sentence).toMatch(/scheduled cancellation/i);
    expect(sentence).toMatch(/stays active and usable until that instant passes/i);

    // The sentence is keyed on the ROW'S OWN instant, compared against the
    // clock — not on a year the page invents.
    //
    // It used to compare against `getUTCFullYear() - 1`, which renders a year
    // in the PAST: a `cancel_at` of last December was described as "the
    // subscription stays active and usable until that instant passes", and no
    // date a reader held could make that true. A year is not a proxy for "has
    // this moment passed"; the moment is the field, so the field is what is
    // compared. The negative half is the part that matters: a PAST `cancel_at`
    // is a real cancellation and must not be described as a scheduled one.
    expect(sentence).not.toMatch(/\d{4}/);
  });

  it("does not call a cancellation that has already happened a scheduled one", async () => {
    seam.fetchSubscriptions.mockResolvedValue(
      ok({
        ...SUBSCRIPTIONS,
        items: [{ ...SUBSCRIPTIONS.items[0]!, cancel_at: "2020-01-01T00:00:00Z" }],
      }),
    );

    const { wrapper } = await mountAt("/commerce");

    // The instant is in the past, so the sentence is a lie in this state and
    // must be absent. The date is still rendered as data — that part never
    // depended on the sentence.
    expect(wrapper.text()).not.toMatch(/scheduled cancellation/i);
    expect(wrapper.findAll("table")[0]!.get("tbody").text()).toContain("2020");
  });

  it("adds nothing to the figures it was given", async () => {
    const { wrapper } = await mountAt("/commerce");

    // Three grants, three figures, one column, and the sum is not among them.
    // 100,000 is the largest grant on the screen; 200,000 is what a page that
    // added the two non-zero rows would print, and 100,000's own digits make
    // the string test on a doubled figure a sharp one rather than a lucky one.
    const figures = renderedFigures(wrapper);
    expect(figures).toHaveLength(3 * 7);
    for (const absent of ["200,000", "300,000", "100,000.00"]) {
      expect(wrapper.text(), absent).not.toContain(absent);
    }

    // The unit is named once per row, in the cell — three rows, three labels.
    // A screen that printed the grant once per column, or once per cycle, would
    // be a different number of claims about the same field.
    expect(
      wrapper
        .findAll("table")[1]!
        .text()
        .match(/minor units/g),
    ).toHaveLength(3);

    // The page says the absence out loud. This is a card, not a computed
    // figure, so a reviewer reads the promise here and the promises above are
    // the assertions that hold it up.
    const card = wrapper.findAll("div").find((node) => node.text().includes("no plan total"));
    expect(card?.text()).toMatch(/no plan total/i);
    expect(card?.text()).toMatch(/no per-cycle projection/i);
    expect(card?.text()).toMatch(/no balance of its own/i);
  });

  it("keeps a grant per cycle a per-row figure and never a per-subscription one", async () => {
    // The two non-zero grants are 0 and 100,000 on ONE subscription, and a
    // subscription figure the console could compute would be 100,000 — the same
    // digits as row three. So the pin is structural rather than textual: one
    // grant, one cell, and the entitlement table has no column for a total.
    const { wrapper } = await mountAt("/commerce");
    const headers = wrapper
      .findAll("table")[1]!
      .findAll("th[scope='col']")
      .map((header) => header.text());
    expect(headers).toEqual([
      "Entitlement",
      "Subscription",
      "Cycle",
      "Scope",
      "Granted",
      "State",
      "Period ends",
    ]);
    // The subscriptions table has no money column at all, which is the
    // structural statement that a subscription is not a figure.
    expect(
      wrapper
        .findAll("table")[0]!
        .findAll("th[scope='col']")
        .map((header) => header.text()),
    ).not.toContain("Granted");
  });

  it("sends neither list a filter, because neither operation declares one", async () => {
    await mountAt("/commerce");
    // Both reads carry a bare query. `listSubscriptions` and
    // `listEntitlements` take no filter, and a parameter here would be a scope
    // this plane has no operation for.
    expect(seam.fetchSubscriptions.mock.lastCall?.[0]).toEqual({ query: {} });
    expect(seam.fetchEntitlements.mock.lastCall?.[0]).toEqual({ query: {} });
  });

  // ADR 0012 §6, this screen's own slice. Driven off the table so a code the
  // contract gains is a code this test has never rendered.
  describe("the api-failure matrix", () => {
    it("renders the failure view rather than a table for every code the console can produce", async () => {
      for (const code of REACHABLE) {
        seam.fetchSubscriptions.mockResolvedValue({ ok: false, failure: apiFailure(code) });
        seam.fetchEntitlements.mockResolvedValue({ ok: false, failure: apiFailure(code) });

        const { wrapper } = await mountAt("/commerce");

        // Two lists, two failures, and neither table survives: a row rendered
        // under a banner is the state a screen must not be able to reach.
        expect(wrapper.findAll('[role="alert"]'), code).toHaveLength(2);
        expect(wrapper.findAll("table"), code).toHaveLength(0);
        expect(wrapper.findAll("table > caption"), code).toHaveLength(0);
        expect(wrapper.find('[role="alert"]').text(), code).toContain(
          CONSOLE_BEHAVIOUR[code].title,
        );

        // The empty messages are not what stands in for either. "This account
        // has no subscription" is a claim about the account's arrangements that
        // a read which never arrived cannot support.
        expect(wrapper.text(), code).not.toContain("This account has no subscription.");
        expect(wrapper.text(), code).not.toContain("No grant has been made to this account.");

        const offersRetry = wrapper
          .findAll("button")
          .some((button) => /try again|retry/i.test(button.text()));
        const offersSignIn = new Set(
          wrapper
            .findAll("a")
            .filter((link) => /sign in/i.test(link.text()))
            .map((link) => link.attributes("href")),
        );
        expect(offersRetry, code).toBe(CONSOLE_BEHAVIOUR[code].recovery === "retry");
        expect(offersSignIn.size > 0, code).toBe(CONSOLE_BEHAVIOUR[code].recovery === "sign-in");
        for (const href of offersSignIn) {
          expect(href, code).toContain("/sign-in");
        }

        expect(wrapper.text().includes("req-commerce-1"), code).toBe(
          CONSOLE_BEHAVIOUR[code].showRequestId,
        );
      }
    });

    it("leaves the OTHER list standing when one of the two fails", async () => {
      // The screen has two independent resources, and a subscriptions outage is
      // not a statement about grants. Rendering both failure views would make
      // one read's failure look like a whole-screen failure.
      seam.fetchSubscriptions.mockResolvedValue({ ok: false, failure: apiFailure("internal") });
      seam.fetchEntitlements.mockResolvedValue(ok(ENTITLEMENTS));

      const { wrapper } = await mountAt("/commerce");

      expect(wrapper.findAll('[role="alert"]')).toHaveLength(1);
      expect(wrapper.findAll("table")).toHaveLength(1);
      expect(wrapper.find("table > caption").text()).toBe(
        "Grants in this account, one row per cycle",
      );
      expect(wrapper.text()).not.toContain("This account has no subscription.");
    });
  });

  it("renders an account with neither a subscription nor a grant as two empty answers", async () => {
    // Both lists empty is a real state — a pay-as-you-go account — and neither
    // empty state may borrow the other's words or the matrix's.
    const emptySubscription = { ...SUBSCRIPTIONS, items: [] } as SubscriptionPage;
    const emptyEntitlement = { ...ENTITLEMENTS, items: [] } as EntitlementPage;
    seam.fetchSubscriptions.mockResolvedValue(ok(emptySubscription));
    seam.fetchEntitlements.mockResolvedValue(ok(emptyEntitlement));

    const { wrapper } = await mountAt("/commerce");

    expect(wrapper.findAll('[role="alert"]')).toHaveLength(0);
    expect(wrapper.findAll("th[scope='row']")).toHaveLength(0);
    expect(wrapper.text()).toContain("This account has no subscription.");
    expect(wrapper.text()).toContain("No grant has been made to this account.");
  });

  it("explains an empty list in its own cell, below the sentence that answers it", async () => {
    // The explanation is ABOUT the list — what a subscription is, that a
    // pay-as-you-go balance lives on another screen — and it used to be joined
    // to the answer with a full stop, so a screen-reader user reading the empty
    // cell one row at a time read a paragraph where there should have been a
    // sentence. Two elements, one block each, is what makes the boundary a
    // rendered thing rather than a full stop.
    const emptySubscription = { ...SUBSCRIPTIONS, items: [] } as SubscriptionPage;
    const emptyEntitlement = { ...ENTITLEMENTS, items: [] } as EntitlementPage;
    seam.fetchSubscriptions.mockResolvedValue(ok(emptySubscription));
    seam.fetchEntitlements.mockResolvedValue(ok(emptyEntitlement));

    const { wrapper } = await mountAt("/commerce");

    const cell = wrapper
      .findAll("td[colspan]")
      .find((candidate) => candidate.text().includes("This account has no subscription."));
    expect(cell).toBeDefined();
    // The answer is the first block and the explanation the second, so the
    // answer is the part a reader who only wants the answer has to hear.
    const blocks = cell!.findAll("span");
    expect(blocks.length).toBeGreaterThanOrEqual(2);
    expect(blocks[0]!.text()).toBe("This account has no subscription.");
    expect(blocks[1]!.text()).toContain("recurring arrangement on a plan version");
    expect(blocks[1]!.text()).toContain("accounting screen");

    const grantCell = wrapper
      .findAll("td[colspan]")
      .find((candidate) => candidate.text().includes("No grant has been made"));
    expect(grantCell).toBeDefined();
    const grantBlocks = grantCell!.findAll("span");
    expect(grantBlocks[0]!.text()).toBe("No grant has been made to this account.");
    expect(grantBlocks[1]!.text()).toContain("one cycle of one plan");
  });
});
