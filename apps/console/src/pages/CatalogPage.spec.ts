// Catalog: the plans this gateway sells, and the deferral the screen has to keep saying.
//
// This page is named for something it does not contain. ADR 0012 §5 defers the
// MODEL catalog — a model's catalogue, its prices, its availability are Data
// Plane state with no operation on `console.yaml` — so what is left is a plan
// list under a nav label that invites the wrong question. The header is the
// answer, and it is the only answer: a reviewer will come to this page
// specifically to check the deferral is visible, and a spec that only counted
// rows would go green the moment someone shortened the description to "Plans".
//
// The second claim is the one a table cannot make for itself: an EMPTY plan list
// is a legitimate answer, not a failure, and the two are the states ADR 0012 §6
// exists to keep apart. An empty gateway renders a sentence about a gateway that
// has published nothing; a broken read renders the matrix row. They must never
// render each other's words.
import { flushPromises, mount, type VueWrapper } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { createMemoryHistory } from "vue-router";

// The seam, mocked at the boundary rather than at the composable: a composable
// stub would prove this screen reads a stub and nothing about what it renders
// with the answer.
const seam = vi.hoisted(() => ({
  fetchPlans: vi.fn(),
  getSessionResult: vi.fn(),
}));

vi.mock("@/lib/api", async () => {
  const actual = await vi.importActual<typeof import("@/lib/api")>("@/lib/api");
  return { ...actual, ...seam };
});

import CatalogPage from "@/pages/CatalogPage.vue";
import { createConsoleRouter } from "@/router";
import { describe as describeViolations, inspect } from "@/lib/arch/roster";
import { CONSOLE_BEHAVIOUR, type ApiErrorCode } from "@/lib/failure-matrix";
import type { ApiFailure, ApiResult } from "@/lib/api";
import type { PlanPage } from "@ecoma-io/llm-gateway-console-api-client";

const ok = <T>(data: T): ApiResult<T> => ({ ok: true, data });

function apiFailure(code: ApiErrorCode): ApiFailure {
  return {
    kind: "api",
    unauthenticated: code === "unauthenticated",
    envelope: { error: { code, message: "the server's own words" }, request_id: "req-catalog-1" },
  };
}

/** Every code this console can produce, read off the matrix rather than copied. */
const REACHABLE = Object.entries(CONSOLE_BEHAVIOUR)
  .filter(([, behaviour]) => behaviour.reachable)
  .map(([code]) => code as ApiErrorCode);

const TWO_PLANS: PlanPage = {
  items: [
    {
      id: "p0000000-0000-4000-8000-0000000000p1",
      name: "payg",
      created_at: "2026-08-01T00:00:00Z",
      updated_at: "2026-08-01T00:00:00Z",
    },
    {
      id: "p0000000-0000-4000-8000-0000000000p2",
      name: "committed-use",
      created_at: "2026-08-02T00:00:00Z",
    },
  ],
  next_cursor: "c-plans-2",
  has_more: false,
};

const ONE_PLAN_MORE: PlanPage = {
  items: [TWO_PLANS.items[0]!],
  next_cursor: "c-plans-2",
  has_more: true,
};

const NO_PLANS: PlanPage = { items: [], next_cursor: "", has_more: false };

beforeAll(() => {
  // Loom's `SegmentedControl` sizes its indicator on mount and jsdom ships no
  // `ResizeObserver`. This screen renders none today; the stub is here so a
  // filter added later does not discover the gap as a crash.
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
  seam.fetchPlans.mockResolvedValue(ok(TWO_PLANS));
  document.body.innerHTML = "";
});

// One sweep for every `attachTo` wrapper, unconditional: a test that failed
// half way through is exactly the one that would have left its wrapper alive
// for the next test's `beforeEach` to re-arm.
afterEach(() => {
  while (mounted.length > 0) mounted.pop()!.unmount();
  document.body.innerHTML = "";
});

async function mountAt(path: string): Promise<{ wrapper: VueWrapper; lastQuery: unknown }> {
  const pinia = createPinia();
  setActivePinia(pinia);
  const router = createConsoleRouter(createMemoryHistory());
  await router.push(path);
  await router.isReady();
  const wrapper = mount(CatalogPage, {
    attachTo: document.body,
    global: { plugins: [router, pinia] },
  });
  mounted.push(wrapper);
  await flushPromises();
  await flushPromises();
  return { wrapper, lastQuery: seam.fetchPlans.mock.lastCall?.[0] };
}

describe("CatalogPage", () => {
  it("keeps the console's architecture roster", async () => {
    // The roster is read off the RENDERED document, so this is a claim about
    // what the page actually produced rather than about what it imported. A
    // list screen that rendered no table, or a table nobody declared a layer
    // for, or a pager with no table to move, is a defect the reader would
    // experience and no type would catch.
    const { wrapper } = await mountAt("/catalog");
    await flushPromises();

    expect(describeViolations(inspect(wrapper.element.ownerDocument, "/catalog"))).toBe("");
  });

  it("renders the deferral where a reader lands, before any row", async () => {
    const { wrapper } = await mountAt("/catalog");

    const header = wrapper.find("header");
    expect(header.exists()).toBe(true);
    expect(header.get("h1").text()).toBe("Catalog");
    // Both halves of the deferral, or the sentence reads as an apology for a
    // missing feature rather than a statement of which plane owns the facts.
    const description = header.get("p").text();
    expect(description).toContain("model catalog");
    expect(description).toMatch(/data plane/i);
    expect(description).toMatch(/no operation here yet/i);
    // And it is the FIRST thing on the screen: a deferral an operator has to
    // scroll to find is a deferral they will ask about in a ticket.
    expect(wrapper.text().indexOf("model catalog")).toBeLessThan(
      wrapper.text().indexOf(TWO_PLANS.items[0]!.name),
    );
  });

  it("claims the catalog is empty only after the server has said so", async () => {
    // The four-state resource, and the fifth state a two-state derivation
    // invents. `usePagedList` issues its read from `onMounted`, which runs AFTER
    // the first render, so `loading.value` is false while the console is
    // painting a table it knows nothing about — and on THIS screen the
    // sentence that two-state derivation produced is a real answer to a real
    // question ("This gateway has published no plan."), which is exactly what
    // makes it the wrong one to give speculatively.
    //
    // The read below never answers, so no amount of flushing can talk the test
    // out of the window; the assertion is about the DOM the console has
    // actually produced.
    seam.fetchPlans.mockReturnValue(new Promise(() => {}));

    const { wrapper } = await mountAt("/catalog");
    expect(wrapper.get("tbody").text()).toMatch(/Loading/);
    expect(wrapper.text()).not.toContain("This gateway has published no plan.");
  });

  it("renders an empty plan list as an answer, never as a failure", async () => {
    seam.fetchPlans.mockResolvedValue(ok(NO_PLANS));
    const { wrapper } = await mountAt("/catalog");

    // A gateway that has published nothing is a real state, and the sentence
    // says which gateway and what it means. The failure view is the thing this
    // must not be mistaken for: a `500` and an empty list are different facts
    // about the system and a screen that renders one for the other is lying
    // about the one it is not.
    expect(wrapper.text()).toContain("This gateway has published no plan.");
    expect(wrapper.findAll('[role="alert"]')).toHaveLength(0);
    expect(wrapper.findAll('[role="status"]')).toHaveLength(0);
    expect(wrapper.findAll("table")).toHaveLength(1);
    // The table's own empty state is a full-width cell inside the body, so the
    // assertion is over the ROW HEADERS rather than over rows: an empty list has
    // a row-shaped placeholder and no data row, and counting rows would either
    // fail on the component or pass on a table that rendered one row.
    expect(wrapper.findAll("th[scope='row']")).toHaveLength(0);
    expect(wrapper.findAll("[data-cell]")).toHaveLength(0);
    expect(wrapper.get("tbody").text()).toBe("This gateway has published no plan.");
    // The matrix's own vocabulary stays out of an empty list: nothing here says
    // anything about failures, because nothing failed.
    expect(wrapper.text()).not.toContain("req-catalog-1");
  });

  it("offers Next only when the server said there is more, and never a number", async () => {
    seam.fetchPlans.mockResolvedValue(ok(TWO_PLANS));
    const { wrapper } = await mountAt("/catalog");

    // `has_more` is the server's answer and this screen has no other opinion.
    // A Next built from `next_cursor` alone would offer a hop into nothing on
    // the last page, which is the console's one self-inflicted failure.
    expect(TWO_PLANS.has_more).toBe(false);
    expect(wrapper.find("nav a").exists()).toBe(false);

    seam.fetchPlans.mockResolvedValue(ok(ONE_PLAN_MORE));
    const { wrapper: paged } = await mountAt("/catalog");

    const links = paged.findAll("nav a");
    expect(links).toHaveLength(1);
    expect(links[0]!.text()).toBe("Next");
    // No page numbers anywhere: the cursor is the server's string and this
    // screen cannot decode one into a position, so a number would be a figure
    // it invented.
    expect(paged.find("nav").text()).not.toMatch(/\d/);
  });

  it("paginates without a filter, because a plan is not an account's", async () => {
    seam.fetchPlans.mockResolvedValue(ok(ONE_PLAN_MORE));
    const { lastQuery } = await mountAt("/catalog");

    // The screen declares no filters, so the read carries a cursor and nothing
    // else. Any parameter here would be a scope the operation does not have.
    expect(lastQuery).toEqual({ query: {} });
    expect(seam.fetchPlans.mock.lastCall?.[0]).toEqual({ query: {} });
  });

  it("drops an undeclared filter the URL was handed rather than sending it", async () => {
    // `?account_id=` is not something this page offers and not something
    // `listPlans` accepts. A hand-edited URL has to read as "no filter" — the
    // vocabulary check is empty here, which is the same check as elsewhere.
    const { lastQuery } = await mountAt("/catalog?account_id=a0000000-0000-4000-8000-0000000000a1");
    expect(lastQuery).toEqual({ query: {} });
  });

  // ADR 0012 §6, this screen's own slice. Driven off the table so a code the
  // contract gains is a code this test has never rendered.
  describe("the api-failure matrix", () => {
    it("renders the failure view rather than a table for every code the console can produce", async () => {
      for (const code of REACHABLE) {
        seam.fetchPlans.mockResolvedValue({ ok: false, failure: apiFailure(code) });

        const { wrapper } = await mountAt("/catalog");

        // The one list, so exactly one failure view, and no table beneath it: a
        // table under a banner is the state the resource header names as the
        // one a screen must not be able to reach.
        expect(wrapper.findAll('[role="alert"]'), code).toHaveLength(1);
        expect(wrapper.findAll("table"), code).toHaveLength(0);
        expect(wrapper.find('[role="alert"]').text(), code).toContain(
          CONSOLE_BEHAVIOUR[code].title,
        );

        // And the empty message is NOT what stands in for it. "This gateway has
        // published no plan" would be a claim about the catalog's contents
        // made by a read that never arrived.
        expect(wrapper.text(), code).not.toContain("This gateway has published no plan.");

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

        // The correlation appears exactly where the row says it may.
        expect(wrapper.text().includes("req-catalog-1"), code).toBe(
          CONSOLE_BEHAVIOUR[code].showRequestId,
        );
      }
    });
  });
});
