// Accounting: the screen that exists to hold "it computes nothing".
//
// Every other page's tests are about what a screen renders. This one is mostly
// about what it REFUSES to render, which is awkward to test and easy to break
// by accident. A `settled − held` in a template is one line and looks helpful;
// it makes the console a second ledger with no audit behind it, and the day it
// disagrees with the server there is no way to say which is right. So the
// assertions below are mostly about ABSENCE — of a total, of a merged balance,
// of a read that was never asked for — and each of those is an assertion that
// goes red the moment somebody adds a convenience.
//
// The fixtures are chosen to make the absence sharp. Two buckets with three
// different figures each, so a combined balance is a set of numbers that appear
// nowhere on the screen; a `consume` leg of `-40` next to a `grant` of
// `+100,000`, so a sign flipped for readability is a digit that changes.
import { flushPromises, mount, type VueWrapper } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { createMemoryHistory, type Router } from "vue-router";

const seam = vi.hoisted(() => ({
  fetchFundingBuckets: vi.fn(),
  fetchLedgerEntries: vi.fn(),
  getSessionResult: vi.fn(),
}));

vi.mock("@/lib/api", async () => {
  const actual = await vi.importActual<typeof import("@/lib/api")>("@/lib/api");
  return { ...actual, ...seam };
});

import AccountingPage from "@/pages/AccountingPage.vue";
import { createConsoleRouter } from "@/router";
import { describe as describeViolations, inspect } from "@/lib/arch/roster";
import { CONSOLE_BEHAVIOUR, type ApiErrorCode } from "@/lib/failure-matrix";
import type { ApiFailure, ApiResult } from "@/lib/api";
import type { FundingBucketPage, LedgerEntryPage } from "@ecoma-io/llm-gateway-console-api-client";

const ok = <T>(data: T): ApiResult<T> => ({ ok: true, data });

function apiFailure(code: ApiErrorCode): ApiFailure {
  return {
    kind: "api",
    unauthenticated: code === "unauthenticated",
    envelope: {
      error: { code, message: "the server's own words" },
      request_id: "req-accounting-1",
    },
  };
}

/** Every code this console can produce, read off the matrix rather than copied. */
const REACHABLE = Object.entries(CONSOLE_BEHAVIOUR)
  .filter(([, behaviour]) => behaviour.reachable)
  .map(([code]) => code as ApiErrorCode);

const PAYG = "b0000000-0000-4000-8000-0000000000b1";
const CYCLE = "b0000000-0000-4000-8000-0000000000b2";

/**
 * Two buckets whose three figures are all distinct from each other and from
 * each other's.
 *
 * The numbers are chosen so every total a screen might compute is a string that
 * appears nowhere: 125,000 (settled across both), 21,000 (held across both),
 * 104,000 (available across both), 150,000 (settled + held + available for the
 * PAYG bucket alone), and the payable sum.
 *
 * More important than the totals is the `120,000`/`20,000`/`100,000` triple:
 * it happens to satisfy `settled − held = available`, and that is DELIBERATE,
 * because an identity-honest server is the normal case and a test that only
 * ever met an identity-violating bucket would not know a screen that derived
 * `available` from `settled − held` was reading the right answer. The screen
 * asserts the IDENTITY too (see the test below), so this file holds both: the
 * fixture reads as a real balance, and the screen is proved to render three
 * server fields rather than compute the third.
 */
const BUCKETS: FundingBucketPage = {
  items: [
    {
      id: PAYG,
      kind: "account",
      account_id: "a0000000-0000-4000-8000-0000000000a1",
      status: "active",
      balances: {
        settled: { minor_units: 120000 },
        held: { minor_units: 20000 },
        available: { minor_units: 100000 },
      },
      version: 7,
      opened_at: "2026-08-01T00:00:00Z",
      created_at: "2026-08-01T00:00:00Z",
    },
    {
      id: CYCLE,
      kind: "entitlement",
      entitlement_id: "60000000-0000-4000-8000-0000000006a1",
      status: "active",
      // A cycle's bucket that has a HOLD against it. `5,000 − 1,000` is `4,000`
      // — the identity again — which is what makes this row the natural place
      // to read `available` as a subtraction.
      balances: {
        settled: { minor_units: 5000 },
        held: { minor_units: 1000 },
        available: { minor_units: 4000 },
      },
      version: 2,
      opened_at: "2026-09-01T00:00:00Z",
      created_at: "2026-09-01T00:00:00Z",
    },
  ],
  next_cursor: "c-buckets-2",
  has_more: false,
};

const LEDGER: LedgerEntryPage = {
  items: [
    {
      id: "80000000-0000-4000-8000-0000000008a1",
      funding_bucket_id: PAYG,
      kind: "consume",
      sequence: 3,
      // The server's own sign. A `consume` takes money OUT, and a client that
      // flipped this for readability would be a client whose ledger disagrees
      // with the domain over what a `consume` means.
      settled_delta: { minor_units: -40 },
      held_delta: { minor_units: 0 },
      created_at: "2026-09-05T00:00:00Z",
    },
    {
      id: "80000000-0000-4000-8000-0000000008a2",
      funding_bucket_id: PAYG,
      kind: "grant",
      sequence: 1,
      settled_delta: { minor_units: 100000 },
      held_delta: { minor_units: 0 },
      created_at: "2026-09-01T00:00:00Z",
    },
  ],
  next_cursor: "c-ledger-2",
  has_more: false,
};

beforeAll(() => {
  // The ledger's kind control is a Loom `SegmentedControl`, which sizes its
  // selection indicator on mount; jsdom ships no `ResizeObserver`. The
  // indicator's geometry is not what this screen is about.
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
  seam.fetchFundingBuckets.mockResolvedValue(ok(BUCKETS));
  seam.fetchLedgerEntries.mockResolvedValue(ok(LEDGER));
  document.body.innerHTML = "";
});

// One sweep for every `attachTo` wrapper, unconditional: a test that failed
// half way through is exactly the one that would have left its wrapper alive —
// and this screen's ledger is still subscribed to the route — for the next
// test's `beforeEach` to re-arm.
afterEach(() => {
  while (mounted.length > 0) mounted.pop()!.unmount();
  document.body.innerHTML = "";
});

async function mountAt(path: string): Promise<{ wrapper: VueWrapper; router: Router }> {
  const pinia = createPinia();
  setActivePinia(pinia);
  const router = createConsoleRouter(createMemoryHistory());
  await router.push(path);
  await router.isReady();
  const wrapper = mount(AccountingPage, {
    attachTo: document.body,
    global: { plugins: [router, pinia] },
  });
  mounted.push(wrapper);
  await flushPromises();
  await flushPromises();
  return { wrapper, router };
}

describe("AccountingPage", () => {
  it("keeps the console's architecture roster", async () => {
    // The roster is read off the RENDERED document, so this is a claim about
    // what the page actually produced rather than about what it imported. A
    // list screen that rendered no table, or a table nobody declared a layer
    // for, or a pager with no table to move, is a defect the reader would
    // experience and no type would catch.
    const { wrapper } = await mountAt("/accounting");
    await flushPromises();

    expect(describeViolations(inspect(wrapper.element.ownerDocument, "/accounting"))).toBe("");
  });

  describe("balances", () => {
    it("renders each of the three figures from its own field, and no fourth", async () => {
      const { wrapper } = await mountAt("/accounting");

      // One cell per bucket, three money figures in it, read in the order the
      // contract states them: settled, held, available. Nothing in the cell is
      // derived from a sibling — a cell that printed `available` as
      // `settled − held` would look identical here and would be a second ledger.
      const rows = wrapper.findAll("table")[0]!.findAll("tbody tr");
      expect(rows).toHaveLength(2);
      // The INLINE spans, not every span in the cell: the cell wraps its three
      // figures in one more, whose own text is their concatenation, and
      // asserting on the outer span too would make a screen that printed a
      // seventh figure inside it pass this count and fail the "no total" test
      // below for the wrong reason.
      const figures = (rowIndex: number) =>
        wrapper
          .findAll("table")[0]!
          .findAll(`[data-cell="r${rowIndex}c2"] > span > span`)
          .map((span) => span.text());
      expect(figures(0)).toEqual(["120,000", "/", "20,000", "/", "100,000"]);
      expect(figures(1)).toEqual(["5,000", "/", "1,000", "/", "4,000"]);

      // Five in, five out. Not `toContain`: a screen that printed a sixth
      // figure beside them is a screen with a total, and the count is what
      // notices.
      expect(figures(0)).toHaveLength(5);
      expect(figures(1)).toHaveLength(5);

      // The six figures are all there, and the totals that a sum would print are
      // not. Each of these is a specific number this screen could have derived
      // and does not.
      const text = wrapper.text();
      for (const total of ["125,000", "21,000", "104,000", "150,000", "230,000"]) {
        expect(text, total).not.toContain(total);
      }

      // The caption names the order the three figures appear in, so the cell's
      // slashes are not a reader having to guess which is which.
      expect(wrapper.find("table > caption").text()).toMatch(
        /settled, held and available in minor units/i,
      );
    });

    it("renders the server's available even where the identity makes it look derived", async () => {
      // The measure of a screen that "computes nothing" is not that the numbers
      // it shows are wrong — it is that you cannot tell whether they were
      // computed, because for an honest bucket the two answers are the same
      // digits. So the fixture is given a bucket whose three figures do NOT
      // satisfy `settled − held = available`, and the screen is shown rendering
      // the server's `available` rather than the subtraction. A screen that
      // derived the third column would agree with this file on every other
      // test and disagree here, which is the only place the claim is visible.
      //
      // The contract's own rule is that the server's three identities are its
      // to keep and a client that recomputes one is a second, unaudited ledger
      // — so disagreeing with the server on screen is the correct behaviour,
      // not a bug to be reconciled away.
      seam.fetchFundingBuckets.mockResolvedValue({
        ok: true,
        data: {
          ...BUCKETS,
          items: [
            BUCKETS.items[0]!,
            {
              ...BUCKETS.items[1]!,
              // A bucket whose `available` is NOT `settled − held`. The
              // database does not produce this; the point is that the console
              // renders what it is given and never quietly corrects it.
              balances: {
                settled: { minor_units: 7777 },
                held: { minor_units: 1111 },
                available: { minor_units: 6999 },
              },
            },
          ],
        },
      });

      const { wrapper } = await mountAt("/accounting");
      const figures = wrapper
        .findAll("table")[0]!
        .findAll('[data-cell="r1c2"] > span > span')
        .map((span) => span.text());

      expect(figures).toEqual(["7,777", "/", "1,111", "/", "6,999"]);
      // The subtraction is 6,666 and the sum is 8,888. Neither is on the
      // screen, and that is the whole point of the fixture.
      expect(wrapper.text()).not.toContain("6,666");
      expect(wrapper.text()).not.toContain("8,888");
    });

    it("shows a pay-as-you-go bucket and a cycle's bucket as two separate balances", async () => {
      const { wrapper } = await mountAt("/accounting");

      // The two kinds are not one balance, and the screen names both. A cycle
      // that rolls must not take the pay-as-you-go balance with it, which is
      // why a cross-bucket view would be a lie about ownership rather than a
      // convenience.
      const kinds = [0, 1].map((row) =>
        wrapper.findAll("table")[0]!.get(`[data-cell="r${row}c1"]`).text(),
      );
      expect(kinds).toEqual(["account", "entitlement"]);

      // Each row carries its OWN three figures and the other row's are not
      // reachable from it. 120,000 and 5,000 are in different cells of
      // different rows, and a merged balance would have needed a figure that is
      // in neither.
      const paygCell = wrapper.findAll("table")[0]!.get('[data-cell="r0c2"]').text();
      const cycleCell = wrapper.findAll("table")[0]!.get('[data-cell="r1c2"]').text();
      expect(paygCell).toBe("120,000/20,000/100,000");
      expect(cycleCell).toBe("5,000/1,000/4,000");
      expect(paygCell).not.toContain("5,000");
      expect(cycleCell).not.toContain("120,000");
    });

    it("says the rule out loud, because the absence is invisible", async () => {
      const { wrapper } = await mountAt("/accounting");
      // A screen that renders no total leaves the reader wondering whether the
      // total was forgotten. The sentence under the table is the answer, and it
      // is asserted here for the same reason the assertions above are: so that
      // deleting the sentence to save a line is a failing test.
      const note = wrapper.findAll("p").find((node) => node.text().includes("does not subtract"));
      expect(note?.text()).toMatch(/does not subtract one to obtain another/i);
      expect(note?.text()).toMatch(/no total across buckets/i);
      expect(note?.text()).toMatch(/separate balance with a separate owner/i);
    });
  });

  describe("the ledger reads for a bucket, and only for a bucket", () => {
    it("shows the ledger for the bucket the URL names", async () => {
      const { wrapper, router } = await mountAt(`/accounting?bucket=${PAYG}`);

      // The path parameter is the bucket id, verbatim from the route. This is
      // the one read on this surface that is not a query, and the assertion is
      // on `path` rather than on `query` because that is where the contract
      // puts it.
      expect(seam.fetchLedgerEntries.mock.lastCall?.[0]).toEqual({
        path: { funding_bucket_id: PAYG },
        query: {},
      });

      // The ledger that comes back is named for the bucket it belongs to, so a
      // reader who opened a second bucket can tell at a glance which history
      // they are looking at.
      expect(wrapper.findAll("table > caption")[1]!.text()).toBe(`Ledger legs for bucket ${PAYG}`);
      expect(router.currentRoute.value.query.bucket).toBe(PAYG);
    });

    it("does not render a ledger at all until a bucket is chosen", async () => {
      const { wrapper } = await mountAt("/accounting");

      // "No ledger" and "a bucket with no legs" are different sentences. The
      // first is about what the reader has not done yet; the second is a claim
      // about money that did not move. A `404` would be worse than both: a
      // screen reporting its own state as a server fact.
      expect(wrapper.findAll("table")).toHaveLength(1);
      expect(wrapper.text()).toMatch(/Choose a bucket above to read the legs that moved it/i);
      expect(wrapper.text()).toMatch(/never a merge of several/i);
      expect(wrapper.findAll('[role="alert"]')).toHaveLength(0);
      expect(wrapper.text()).not.toContain("This bucket has no leg matching that kind.");
      expect(wrapper.text()).not.toContain("req-accounting-1");
    });

    it("explains an empty list in its own cell, below the sentence that answers it", async () => {
      // The explanation of a domain is ABOUT the list, and it used to be joined
      // to the answer in one string, so the cell a screen reader reads row by
      // row carried a paragraph. Two blocks, one sentence each, is the shape
      // that keeps the answer short.
      seam.fetchFundingBuckets.mockResolvedValue({
        ok: true,
        data: { ...BUCKETS, items: [] } as FundingBucketPage,
      });
      seam.fetchLedgerEntries.mockResolvedValue({
        ok: true,
        data: { items: [], next_cursor: undefined, has_more: false },
      });

      const { wrapper } = await mountAt(`/accounting?bucket=${PAYG}`);

      const cell = wrapper
        .findAll("td[colspan]")
        .find((candidate) => candidate.text().includes("This account holds no funding bucket."));
      expect(cell).toBeDefined();
      const blocks = cell!.findAll("span");
      expect(blocks[0]!.text()).toBe("This account holds no funding bucket.");
      expect(blocks[1]!.text()).toContain("money set aside for spend");
    });

    it("does not read the ledger at all until a bucket is chosen", async () => {
      // The requirement, now the behaviour. `usePagedList` takes an `enabled`
      // gate, and the screen passes `selectedBucketId !== ""` — so with no
      // bucket chosen the console asks about nothing.
      //
      // This was a real defect once, and the assertion below is written so it
      // cannot come back quietly: the previous version fired `resource.run()`
      // from `onMounted` unconditionally, so the reader never SAW the `404`
      // (a `v-if` gated the rendering) while the request went out asking about
      // `funding_bucket_id: ""` — twice per visit, on a screen a reader almost
      // always reaches before choosing anything. The page's own comment claimed
      // the composable "is called only once a bucket is chosen", and that claim
      // was false at the layer it was placed at.
      const { wrapper } = await mountAt("/accounting");

      // The whole point: no call, rather than a call whose answer is hidden.
      expect(seam.fetchLedgerEntries).not.toHaveBeenCalled();

      // And the reader is still told what to do, rather than shown a spinner
      // for a read that was never made.
      expect(wrapper.findAll('[role="alert"]')).toHaveLength(0);
      expect(wrapper.text()).toMatch(/Choose a bucket above/i);
    });

    it("reads only for the bucket that was chosen, after a click", async () => {
      const { wrapper, router } = await mountAt("/accounting");
      expect(router.currentRoute.value.query.bucket).toBeUndefined();

      // The reader picks the SECOND bucket — the one the first assertion above
      // shows is not the default — so a screen that ignored the click and read
      // the first would be caught here rather than passing on the first bucket.
      await wrapper.findAll("table")[0]!.findAll("tbody tr button")[1]!.trigger("click");
      await flushPromises();
      await flushPromises();

      expect(router.currentRoute.value.query.bucket).toBe(CYCLE);
      expect(seam.fetchLedgerEntries.mock.lastCall?.[0]).toEqual({
        path: { funding_bucket_id: CYCLE },
        query: {},
      });
      // The choice is a route, not a store, so a reload and a shared link land
      // on the same ledger. And the row that was picked says so.
      expect(wrapper.findAll("table > caption")[1]!.text()).toBe(`Ledger legs for bucket ${CYCLE}`);
      expect(
        wrapper
          .findAll("table")[0]!
          .findAll("tbody tr button")
          .map((button) => button.attributes("aria-pressed")),
      ).toEqual(["false", "true"]);
    });

    it("claims the account holds no money only after the server has said so", async () => {
      // The four-state resource, and the fifth state a two-state derivation
      // invents. `usePagedList` issues its read from `onMounted`, which runs after
      // the first render, so `loading.value` is false exactly when the console
      // knows least.
      //
      // The ledger is the one worth naming, because it is the screen's most
      // correct read — the `enabled` gate, the `ledger_after` key, the sign taken
      // from the server — and it still had the wrong empty sentence. Getting the
      // cursor key right and the state word wrong on the same table are
      // unrelated failures, and one of them being fixed says nothing about the
      // other.
      seam.fetchFundingBuckets.mockReturnValue(new Promise(() => {}));
      seam.fetchLedgerEntries.mockReturnValue(new Promise(() => {}));

      const { wrapper } = await mountAt(`/accounting?bucket=${PAYG}`);
      const bodies = wrapper.findAll("tbody").map((body) => body.text());
      expect(bodies).toHaveLength(2);
      for (const body of bodies) {
        expect(body).toMatch(/Loading/);
      }
      // A claim about the account's MONEY, which on this screen is the one
      // category of sentence that has to come from the server.
      expect(wrapper.text()).not.toContain("This account holds no funding bucket.");
      expect(wrapper.text()).not.toContain("This bucket has no leg matching that kind.");
    });

    it("keeps the two lists on one route from sharing a cursor", async () => {
      const { wrapper } = await mountAt(`/accounting?bucket=${PAYG}&after=c-buckets-2`);

      // `after` is the BUCKET list's cursor and `ledger_after` the ledger's.
      // One route carries both tables, and a shared key would send a cursor the
      // server issued for one collection against the other operation's — the
      // `400 invalid_request` the two-key shape exists to avoid.
      expect(seam.fetchFundingBuckets.mock.lastCall?.[0]).toEqual({
        query: { after: "c-buckets-2" },
      });
      // The ledger is handed a cursor from ITS OWN key or none at all. Sending
      // the bucket list's `after` here was a real defect: the second cursor key
      // fixed the route-key collision, but the read still took every query key
      // on the route, so the ledger received a cursor the server had minted for
      // a different collection — which the contract answers with `400
      // invalid_request`, and the console then answers by restarting the ledger
      // at its first page. The reader would lose their place on a table they
      // were not reading, because of a pager they had not touched.
      expect(seam.fetchLedgerEntries.mock.lastCall?.[0]).toEqual({
        path: { funding_bucket_id: PAYG },
        query: {},
      });

      // And with the ledger's OWN cursor in the URL, that is the one it sends.
      seam.fetchLedgerEntries.mockClear();
      await mountAt(`/accounting?bucket=${PAYG}&after=c-buckets-2&ledger_after=c-ledger-3`);
      expect(seam.fetchLedgerEntries.mock.lastCall?.[0]).toEqual({
        path: { funding_bucket_id: PAYG },
        query: { after: "c-ledger-3" },
      });

      // The pager, on the other hand, is correct and says so: the bucket list's
      // "First" is `/accounting` — the bucket list declares no filters, so its
      // pager carries none and the reader's chosen bucket does not ride along
      // into a page change on the list that did not own it.
      const hrefs = wrapper.findAll("nav a").map((link) => link.attributes("href"));
      expect(hrefs).toContain("/accounting");
      void wrapper;
    });
  });

  describe("a leg's sign is the server's", () => {
    it("renders a consume as negative and a grant as positive", async () => {
      const { wrapper } = await mountAt(`/accounting?bucket=${PAYG}`);
      const rows = wrapper.findAll("table")[1]!.findAll("tbody tr");
      expect(rows).toHaveLength(2);

      // The `Settled Δ` column is where the sign lives. `-40` is the server's
      // own value for money leaving; `+100,000` is the server's own value for
      // money arriving, and the leading `+` is there so a greyscale reader can
      // still tell direction from the sign alone.
      expect(rows[0]!.get('[data-cell="r0c3"]').text()).toBe("-40");
      expect(rows[1]!.get('[data-cell="r1c3"]').text()).toBe("+100,000");
      // And the direction column says the same thing in words, from the deltas
      // rather than from the kind.
      expect(rows[0]!.get('[data-cell="r0c2"]').text()).toMatch(/out/i);
      expect(rows[1]!.get('[data-cell="r1c2"]').text()).toMatch(/in/i);
    });

    it("renders a held delta of zero as a plain zero, with no sign to read", async () => {
      const { wrapper } = await mountAt(`/accounting?bucket=${PAYG}`);
      // A zero delta is not a movement, so it gets no `+` and no `−`: a
      // sign on a zero is a claim that something moved.
      for (const row of [0, 1]) {
        expect(wrapper.findAll("table")[1]!.get(`[data-cell="r${row}c4"]`).text()).toBe("0");
      }
    });
  });

  // ADR 0012 §6, this screen's own slice. Driven off the table so a code the
  // contract gains is a code this test has never rendered.
  describe("the api-failure matrix", () => {
    it("renders the ledger's failure over the ledger alone", async () => {
      for (const code of REACHABLE) {
        seam.fetchFundingBuckets.mockResolvedValue(ok(BUCKETS));
        seam.fetchLedgerEntries.mockResolvedValue({ ok: false, failure: apiFailure(code) });

        const { wrapper } = await mountAt(`/accounting?bucket=${PAYG}`);

        // Two tables on one route, one of them failed: exactly one failure
        // view, the buckets table standing, and the LEDGER table gone. A
        // ledger rendered under a banner is the state the resource header names
        // as unreachable.
        const captions = wrapper.findAll("table > caption").map((caption) => caption.text());
        expect(wrapper.findAll('[role="alert"]'), code).toHaveLength(1);
        expect(captions, code).toEqual([
          "Funding buckets, with settled, held and available in minor units",
        ]);
        expect(wrapper.find('[role="alert"]').text(), code).toContain(
          CONSOLE_BEHAVIOUR[code].title,
        );
        // The empty ledger is not what stands in for a failed one.
        expect(wrapper.text(), code).not.toContain("This bucket has no leg matching that kind.");

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
        expect(wrapper.text().includes("req-accounting-1"), code).toBe(
          CONSOLE_BEHAVIOUR[code].showRequestId,
        );
      }
    });

    it("renders the buckets' failure over the buckets alone, ledger and all", async () => {
      for (const code of REACHABLE) {
        seam.fetchFundingBuckets.mockResolvedValue({ ok: false, failure: apiFailure(code) });
        seam.fetchLedgerEntries.mockResolvedValue(ok(LEDGER));

        const { wrapper } = await mountAt(`/accounting?bucket=${PAYG}`);

        // The ledger is a table like any other and the screen picks which one
        // to drop — the one whose read failed. Here that is the bucket list,
        // and a ledger with no bucket above it is exactly the case a screen
        // must handle rather than render a ledger for a bucket it cannot show.
        expect(wrapper.findAll('[role="alert"]'), code).toHaveLength(1);
        expect(
          wrapper.findAll("table > caption").map((c) => c.text()),
          code,
        ).toEqual([`Ledger legs for bucket ${PAYG}`]);
        expect(wrapper.find('[role="alert"]').text(), code).toContain(
          CONSOLE_BEHAVIOUR[code].title,
        );
        expect(wrapper.text(), code).not.toContain("This account holds no funding bucket.");

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
        expect(wrapper.text().includes("req-accounting-1"), code).toBe(
          CONSOLE_BEHAVIOUR[code].showRequestId,
        );
      }
    });

    it("renders the buckets' failure with no ledger at all when no bucket was chosen", async () => {
      for (const code of REACHABLE) {
        seam.fetchFundingBuckets.mockResolvedValue({ ok: false, failure: apiFailure(code) });
        seam.fetchLedgerEntries.mockResolvedValue(ok(LEDGER));

        const { wrapper } = await mountAt("/accounting");

        // With no bucket named there is no ledger to keep, so the screen is a
        // banner and the sentence that says which bucket to choose — not a
        // banner, the sentence, and a table of legs for a bucket the reader
        // cannot see.
        expect(wrapper.findAll('[role="alert"]'), code).toHaveLength(1);
        expect(wrapper.findAll("table"), code).toHaveLength(0);
        expect(wrapper.text(), code).toMatch(/Choose a bucket above/i);
        expect(wrapper.find('[role="alert"]').text(), code).toContain(
          CONSOLE_BEHAVIOUR[code].title,
        );

        const offersRetry = wrapper
          .findAll("button")
          .some((button) => /try again|retry/i.test(button.text()));
        expect(offersRetry, code).toBe(CONSOLE_BEHAVIOUR[code].recovery === "retry");
        expect(wrapper.text().includes("req-accounting-1"), code).toBe(
          CONSOLE_BEHAVIOUR[code].showRequestId,
        );
      }
    });
  });
});
