// Reconciliation: two whole-system lists, two independent filters, and the
// server's own bytes for the evidence.
//
// Three claims live here and all three are about scope. A run is not an
// account's and a finding is not a bucket's, so neither request may carry an
// account — that is the plane split, and a screen that quietly added a scope
// would make a report about the system look like a report about the reader's
// own money. The two filters are independent, because the contract says severity
// is written once and never moved: an operator narrowing to `critical` has not
// thereby said the finding is open, and a resolved-and-critical finding has to
// stay findable. And the evidence is rendered as the JSON it is — a console that
// summarised `observed` into its own columns would be making claims about the
// figures a second time, with nothing to check the second reading against.
import { flushPromises, mount, type VueWrapper } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { createMemoryHistory, type Router } from "vue-router";

const seam = vi.hoisted(() => ({
  fetchFindings: vi.fn(),
  fetchReconciliationRuns: vi.fn(),
  getSessionResult: vi.fn(),
}));

vi.mock("@/lib/api", async () => {
  const actual = await vi.importActual<typeof import("@/lib/api")>("@/lib/api");
  return { ...actual, ...seam };
});

import ReconciliationPage from "@/pages/ReconciliationPage.vue";
import { createConsoleRouter } from "@/router";
import { describe as describeViolations, inspect } from "@/lib/arch/roster";
import { CONSOLE_BEHAVIOUR, type ApiErrorCode } from "@/lib/failure-matrix";
import type { ApiFailure, ApiResult } from "@/lib/api";
import type {
  Finding,
  FindingPage,
  ReconciliationRunPage,
} from "@ecoma-io/llm-gateway-console-api-client";

const ok = <T>(data: T): ApiResult<T> => ({ ok: true, data });

function apiFailure(code: ApiErrorCode): ApiFailure {
  return {
    kind: "api",
    unauthenticated: code === "unauthenticated",
    envelope: { error: { code, message: "the server's own words" }, request_id: "req-recon-1" },
  };
}

/** Every code this console can produce, read off the matrix rather than copied. */
const REACHABLE = Object.entries(CONSOLE_BEHAVIOUR)
  .filter(([, behaviour]) => behaviour.reachable)
  .map(([code]) => code as ApiErrorCode);

const RUNS: ReconciliationRunPage = {
  items: [
    {
      id: 1,
      scope: "system",
      status: "completed",
      window_from: "2026-09-05T00:00:00Z",
      // Half-open, and rendered as such: a window printed as an inclusive pair
      // cannot be told from one that overlapped the last.
      window_to: "2026-09-06T00:00:00Z",
      started_at: "2026-09-06T00:01:00Z",
      finished_at: "2026-09-06T00:04:00Z",
      buckets_scanned: 4,
      findings_opened: 1,
      findings_unchanged: 2,
    },
  ],
  next_cursor: "c-runs-2",
  has_more: false,
};

const RESOLVED_CRITICAL: Finding = {
  id: "f-1",
  check_kind: "bucket_balance_identities",
  subject_kind: "funding_bucket",
  subject_id: "b0000000-0000-4000-8000-0000000000b1",
  // Severity is written once and never moved, so a CRITICAL finding that is
  // RESOLVED is a real thing and has to stay findable — the two filters are
  // independent, and this row is what makes that observable.
  severity: "critical",
  status: "resolved",
  observed: {
    settled_minor_units: 120000,
    held_minor_units: 20000,
    available_minor_units: 100000,
  },
  detail: "settled did not equal available plus held",
  detected_at: "2026-09-05T02:00:00Z",
  last_seen_at: "2026-09-05T03:00:00Z",
  resolved_at: "2026-09-05T04:00:00Z",
};

const OPEN_WARNING: Finding = {
  id: "f-2",
  check_kind: "ledger_sequence_gap",
  subject_kind: "funding_bucket",
  subject_id: "b0000000-0000-4000-8000-0000000000b2",
  severity: "warning",
  status: "open",
  // No `observed`: a finding the check could not attach figures to. The screen
  // renders a section for it and hides it rather than dropping the row, so the
  // row count and the evidence sections are not the same number.
  detail: "a sequence the pass recorded is not in the ledger",
  detected_at: "2026-09-06T02:00:00Z",
  last_seen_at: "2026-09-06T02:00:00Z",
};

const FINDINGS: FindingPage = {
  items: [RESOLVED_CRITICAL, OPEN_WARNING],
  next_cursor: "c-findings-2",
  has_more: true,
};

/** The pager links, as the addresses a reader would follow. */
function pagerHrefs(wrapper: VueWrapper): readonly string[] {
  return wrapper.findAll("nav a").map((link) => link.attributes("href") ?? "");
}

beforeAll(() => {
  // Both filters are Loom `SegmentedControl`s, which size their selection
  // indicator on mount; jsdom ships no `ResizeObserver`.
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
  seam.fetchReconciliationRuns.mockResolvedValue(ok(RUNS));
  seam.fetchFindings.mockResolvedValue(ok(FINDINGS));
  document.body.innerHTML = "";
});

// One sweep for every `attachTo` wrapper, unconditional: a test that failed
// half way through is exactly the one that would have left its wrapper alive for
// the next test's `beforeEach` to re-arm.
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
  const wrapper = mount(ReconciliationPage, {
    attachTo: document.body,
    global: { plugins: [router, pinia] },
  });
  mounted.push(wrapper);
  await flushPromises();
  await flushPromises();
  return { wrapper, router };
}

/** The findings table, the second on the page. */
function findingsTable(wrapper: VueWrapper) {
  const table = wrapper.findAll("table")[1]!;
  return {
    headers: table.findAll("th[scope='col']").map((header) => header.text()),
    rows: table.findAll("tbody tr"),
  };
}

describe("ReconciliationPage", () => {
  it("keeps the console's architecture roster", async () => {
    // The roster is read off the RENDERED document, so this is a claim about
    // what the page actually produced rather than about what it imported. A
    // list screen that rendered no table, or a table nobody declared a layer
    // for, or a pager with no table to move, is a defect the reader would
    // experience and no type would catch.
    const { wrapper } = await mountAt("/reconciliation");
    await flushPromises();

    expect(describeViolations(inspect(wrapper.element.ownerDocument, "/reconciliation"))).toBe("");
  });

  describe("both lists are whole-system", () => {
    it("sends no account filter on either request", async () => {
      await mountAt("/reconciliation");

      // Neither operation takes an account, so a query key here would be a
      // scope the plane has no operation for. The assertion is over EVERY call
      // rather than the last one, because a screen that scoped only its second
      // read would pass a check that looked at one of them.
      for (const call of seam.fetchFindings.mock.calls) {
        expect(call[0], "findings must not be scoped").toEqual({ query: {} });
      }
      for (const call of seam.fetchReconciliationRuns.mock.calls) {
        expect(call[0], "runs must not be scoped").toEqual({ query: {} });
      }
      expect(seam.fetchFindings).toHaveBeenCalled();
      expect(seam.fetchReconciliationRuns).toHaveBeenCalled();
      // And no account id anywhere in either call, under any key — the shape of
      // the request is the claim, and this is the same claim spelled as a
      // substring.
      expect(JSON.stringify(seam.fetchFindings.mock.calls)).not.toMatch(/account/i);
      expect(JSON.stringify(seam.fetchReconciliationRuns.mock.calls)).not.toMatch(/account/i);
    });

    it("keeps an account filter the URL was handed out of both requests", async () => {
      // A reader who pastes a scoped URL — or a link from another console that
      // used to offer one — must get the whole-system answer, not a narrower
      // one dressed in the same table. Neither list declares the key, so it is
      // dropped before the request is built.
      await mountAt("/reconciliation?account_id=a0000000-0000-4000-8000-0000000000a1");
      expect(seam.fetchFindings.mock.lastCall?.[0]).toEqual({ query: {} });
      expect(seam.fetchReconciliationRuns.mock.lastCall?.[0]).toEqual({ query: {} });
    });
  });

  describe("the two filters are independent", () => {
    it("keeps a resolved critical finding findable by severity alone", async () => {
      const { wrapper } = await mountAt("/reconciliation?severity=critical");
      const { rows } = findingsTable(wrapper);

      // The claim: narrowing to `critical` says nothing about status. If the
      // screen treated severity as implying "open", a resolved finding would
      // vanish from a list the reader narrowed for exactly the right reason —
      // and a reconciliation screen that hides resolved history is a screen that
      // cannot be used to answer "has this been fixed".
      expect(seam.fetchFindings.mock.lastCall?.[0]).toEqual({ query: { severity: "critical" } });
      expect(rows).toHaveLength(2);
      expect(rows[0]!.text()).toContain("f-1");
      expect(rows[0]!.text()).toContain("Critical");
      expect(rows[0]!.text()).toContain("Resolved");
      // And the finding's evidence is still readable, because the row is.
      expect(wrapper.find('section[aria-label="Evidence for finding f-1"]').exists()).toBe(true);
    });

    it("does not add a status the reader did not ask for", async () => {
      // The request is the proof. `status: "open"` appearing here would be the
      // severity-implies-open claim made at the wire, where a reviewer reading
      // the request would see it and a user would not.
      const { wrapper } = await mountAt("/reconciliation?severity=critical");
      expect(JSON.stringify(seam.fetchFindings.mock.lastCall?.[0])).not.toContain("status");
      expect(JSON.stringify(wrapper.text())).not.toMatch(/\?status=/);
    });

    it("sends both when both are given, and either alone when only one is", async () => {
      await mountAt("/reconciliation?status=resolved&severity=critical");
      expect(seam.fetchFindings.mock.lastCall?.[0]).toEqual({
        query: { status: "resolved", severity: "critical" },
      });

      await mountAt("/reconciliation?status=open");
      expect(seam.fetchFindings.mock.lastCall?.[0]).toEqual({ query: { status: "open" } });

      await mountAt("/reconciliation?severity=warning");
      expect(seam.fetchFindings.mock.lastCall?.[0]).toEqual({ query: { severity: "warning" } });
    });
  });

  describe("each filter is checked against its own vocabulary", () => {
    it("reads a hand-edited status as no filter rather than sending it", async () => {
      const { wrapper } = await mountAt("/reconciliation?status=not-a-status&severity=critical");

      // The URL is user input. A value outside the contract's vocabulary is a
      // `400` the server would answer, and a reader who was handed a bad link
      // must not be shown one; the console drops the value and reads it as
      // "unfiltered" while the valid `severity` still goes through.
      expect(seam.fetchFindings.mock.lastCall?.[0]).toEqual({ query: { severity: "critical" } });
      expect(JSON.stringify(seam.fetchFindings.mock.lastCall?.[0])).not.toContain("not-a-status");

      // And the control reads as "no filter" too, which is the half that was
      // broken. The two computed refs read the route through `filter()`, which
      // applied no vocabulary check, so the bound value was `"not-a-status"` — a
      // value no segment carries — and NOT ONE segment was selected. The
      // request said unfiltered, the control said nothing, and clicking a
      // status to escape the dead state was the only way out of it.
      const statuses = wrapper
        .findAll('[role="radiogroup"]')
        .find((group) => group.attributes("aria-label") === "Filter findings by status")!;
      expect(
        statuses.findAll('[role="radio"]').map((radio) => radio.attributes("aria-checked")),
      ).toEqual(["true", "false", "false", "false"]);
      // The severity control, whose URL value IS in the vocabulary, does show
      // its segment — so the gap is the missing check, not a broken control.
      const severities = wrapper
        .findAll('[role="radiogroup"]')
        .find((group) => group.attributes("aria-label") === "Filter findings by severity")!;
      expect(
        severities.findAll('[role="radio"]').map((radio) => radio.attributes("aria-checked")),
      ).toEqual(["false", "false", "false", "true"]);
    });

    it("reads a hand-edited severity as no filter too", async () => {
      await mountAt("/reconciliation?severity=apocalyptic&status=open");
      expect(seam.fetchFindings.mock.lastCall?.[0]).toEqual({ query: { status: "open" } });
    });

    it("carries both declared filters through a page change and drops what it did not declare", async () => {
      const { wrapper } = await mountAt(
        "/reconciliation?status=open&severity=critical&account_id=a0000000-0000-4000-8000-0000000000a1",
      );

      // `has_more` is true, so there is a Next, and it must keep the two
      // declared filters — a page change that dropped them would render page two
      // of an unfiltered list under a control still showing the reader's
      // filters. It must DROP the undeclared one: a key nobody asked for cannot
      // ride along, and `account_id` is the one that would scope a
      // whole-system screen. The declared pair still goes through — the
      // undeclared key is dropped, which is the part that matters.
      expect(seam.fetchFindings.mock.lastCall?.[0]).toEqual({
        query: { status: "open", severity: "critical" },
      });
      const findingsNext = pagerHrefs(wrapper).filter((href) =>
        href.includes("findings_after=c-findings"),
      );
      expect(findingsNext).toEqual([
        "/reconciliation?status=open&severity=critical&findings_after=c-findings-2",
      ]);
      for (const href of pagerHrefs(wrapper)) {
        expect(href, href).not.toMatch(/account_id|account/i);
      }
    });

    it("keeps a valid filter pair on a page change with no undeclared noise", async () => {
      const { wrapper } = await mountAt("/reconciliation?status=resolved&severity=critical");
      expect(pagerHrefs(wrapper)).toEqual([
        "/reconciliation?status=resolved&severity=critical&findings_after=c-findings-2",
      ]);
    });
  });

  describe("the two lists on one route do not share a cursor", () => {
    it("sends each list only the cursor its own key holds", async () => {
      // The case the accounting screen fixed for its bucket list and its
      // ledger, and this screen did not have for either of its lists. Asserted
      // on the REQUESTS rather than on the hrefs, because a shared cursor key
      // produces links that look correct and only misbehave once the second
      // list reads the first list's position.
      const { router } = await mountAt(
        "/reconciliation?after=c-runs-2&findings_after=c-findings-2",
      );

      // Each list reads its own key, so each sends the cursor that key holds.
      expect(seam.fetchReconciliationRuns.mock.lastCall?.[0]).toEqual({
        query: { after: "c-runs-2" },
      });
      expect(seam.fetchFindings.mock.lastCall?.[0]).toEqual({
        query: { after: "c-findings-2" },
      });

      // And that is the ONLY way this passes. With both lists on the default
      // `after` key, the findings list would have read `after` — the runs
      // list's cursor — and sent it against `GET /findings`; the server refuses
      // a cursor that "names a different collection" with `400
      // invalid_request`, `resource.ts` classifies that as a lost cursor, and
      // the console replies with `restartWithoutCursor` — a REPLACE that drops
      // `after` and so resets the runs list the reader was not even reading.
      // The address bar is asserted because that restart is the damage: it
      // replaces the URL rather than pushing, so Back cannot undo it either.
      expect(router.currentRoute.value.query.after).toBe("c-runs-2");
      expect(router.currentRoute.value.query.findings_after).toBe("c-findings-2");

      // The runs list alone, with a findings cursor in the URL. It must send
      // nothing, and — the half a shared key fails on — the findings cursor
      // must survive on screen rather than being cleared by a restart.
      seam.fetchReconciliationRuns.mockClear();
      const { router: isolated } = await mountAt("/reconciliation?findings_after=c-findings-2");
      expect(seam.fetchReconciliationRuns.mock.lastCall?.[0]).toEqual({ query: {} });
      expect(isolated.currentRoute.value.query.findings_after).toBe("c-findings-2");
    });

    it("keeps both declared filters on a page change while naming its own cursor key", async () => {
      // The two halves of the findings list's shape, asserted together because
      // they are the same declaration: `shape.filters` says which keys a page
      // change carries, and `shape.cursor` says which key the cursor itself
      // rides in. Getting the first right and the second wrong still loses the
      // reader's place — on a different table, for a pager they never touched.
      const { wrapper } = await mountAt("/reconciliation?status=open&findings_after=c-findings-2");

      // The reader's own cursor is in the request...
      expect(seam.fetchFindings.mock.lastCall?.[0]).toEqual({
        query: { status: "open", after: "c-findings-2" },
      });
      // ...and "First" drops it while keeping the filter, because the first
      // page is a position the console can name and "no cursor" is the honest
      // way to write it.
      const hrefs = pagerHrefs(wrapper);
      expect(hrefs).toContain("/reconciliation?status=open");
      for (const href of hrefs) {
        expect(href, href).toContain("status=open");
      }
    });
  });

  describe("the evidence is the server's bytes", () => {
    it("renders observed as formatted JSON and invents no columns from it", async () => {
      const { wrapper } = await mountAt("/reconciliation?status=resolved");
      const evidence = wrapper.get('section[aria-label="Evidence for finding f-1"]');
      const pre = evidence.get("pre");

      // Formatted, because a reader is meant to read it — and exactly the JSON
      // the check wrote, because the shape is that check's and not the
      // contract's to fix.
      expect(pre.text()).toBe(JSON.stringify(RESOLVED_CRITICAL.observed, null, 2));
      expect(pre.text()).toContain('"settled_minor_units": 120000');
      expect(pre.text()).toContain("\n  ");

      // No invented columns: the findings table's headers are the CONTRACT's
      // fields and nothing from `observed` has been promoted into one. A screen
      // that summarised the evidence would have to put the figures somewhere,
      // and a table with a `settled_minor_units` header is that somewhere.
      const { headers, rows } = findingsTable(wrapper);
      expect(headers).toEqual([
        "Finding",
        "Check",
        "Subject",
        "Severity",
        "Status",
        "Detected",
        "Last seen",
      ]);
      for (const forbidden of ["settled_minor_units", "available_minor_units", "observed"]) {
        expect(headers).not.toContain(forbidden);
        for (const row of rows) {
          expect(row.text(), forbidden).not.toContain(forbidden);
        }
      }
    });

    it("gives each finding its own labelled section and names the check", async () => {
      const { wrapper } = await mountAt("/reconciliation");

      // Labelled by the finding's own id, so two evidence blocks in one screen
      // can be told apart by a screen reader's rotor as well as by a reader's
      // eye — and the section exists for every finding, hidden or not, because
      // "no figures were attached" and "this finding has no evidence section"
      // are different things for anyone navigating by landmark.
      expect(wrapper.findAll("section[aria-label^='Evidence for finding']")).toHaveLength(2);
      expect(wrapper.get('section[aria-label="Evidence for finding f-1"]').text()).toContain(
        RESOLVED_CRITICAL.detail ?? "",
      );
      // The second finding has no `observed`, so its block is empty and hidden
      // rather than rendering `{}` or a placeholder that looks like a figure.
      const bare = wrapper.get('section[aria-label="Evidence for finding f-2"]');
      expect(bare.get("pre").text()).toBe("");
      expect(bare.attributes("style")).toMatch(/display:\s*none/);
    });
  });

  describe("the run's window is half-open", () => {
    it("says which end is which, because two adjacent runs share a boundary", async () => {
      const { wrapper } = await mountAt("/reconciliation");
      const row = wrapper.findAll("table")[0]!.get("tbody tr");
      const window = row.get('[data-cell="r0c2"]').text();

      // A reader comparing run 1 to run 2 is comparing `to` of one with `from`
      // of the next. Printed as a bare pair, `2026-09-06` could be either the
      // last instant of one or the first of the other, and the screen would be
      // the only place that claim is made.
      expect(window).toContain("2026-09-05T00:00:00Z");
      expect(window).toContain("2026-09-06T00:00:00Z");
      expect(window).toMatch(/to exclusive/i);
      expect(window).toContain("→");
    });
  });

  describe("empty is an answer, not a failure", () => {
    it("claims nothing about the system until the system has answered", async () => {
      // The four-state resource, and the fifth state a two-state derivation
      // invents. `usePagedList` reads from `onMounted`, which runs AFTER the
      // first render, so `loading.value` is false exactly when the console has
      // least to say.
      //
      // Worth the assertion here more than on any other screen, because both of
      // this screen's empty sentences are SYSTEM-LEVEL claims — "No pass has
      // run yet.", "No finding matches those filters." A gateway whose reaper
      // has never run and a console that has not finished asking are different
      // facts, and only the second one is true during that window.
      seam.fetchReconciliationRuns.mockReturnValue(new Promise(() => {}));
      seam.fetchFindings.mockReturnValue(new Promise(() => {}));

      const { wrapper } = await mountAt("/reconciliation");
      for (const body of wrapper.findAll("tbody")) {
        expect(body.text()).toMatch(/Loading/);
      }
      expect(wrapper.text()).not.toContain("No pass has run yet.");
      expect(wrapper.text()).not.toContain("No finding matches those filters.");

      // The findings table's `note` — "That is the answer a healthy system
      // gives." — is a SHARPER claim than either sentence, and this change does
      // not reach it. `DataTable` guards `emptyMessage` on `state` but renders
      // `note` unconditionally, so the verdict survives next to "Loading…".
      // That is a real defect and it lives in `components/DataTable.vue`, which
      // this change does not own; the assertion below records the boundary
      // rather than pretending the screen is clean. An owner's fix is to gate
      // the note on the same `state` the message is gated on, at which point
      // this becomes a plain `not.toMatch`.
      expect(wrapper.text()).toMatch(/answer a healthy system gives/i);
    });

    it("renders a healthy system's silence as a sentence about the filters", async () => {
      seam.fetchFindings.mockResolvedValue(
        ok({ items: [], next_cursor: undefined, has_more: false }),
      );
      const { wrapper } = await mountAt("/reconciliation?status=resolved&severity=critical");

      // The sentence names the filters, because an empty list under two active
      // filters is the good answer and a reader needs to know which list is
      // empty — the one under the controls, not the runs above it.
      expect(wrapper.findAll('[role="alert"]')).toHaveLength(0);
      expect(wrapper.text()).toMatch(/No finding matches those filters/i);
      expect(wrapper.text()).toMatch(/answer a healthy system gives/i);
      // And the runs list is untouched: one empty answer is not two.
      expect(wrapper.findAll("table > caption")).toHaveLength(2);
      expect(wrapper.findAll("table")[0]!.findAll("tbody tr")).toHaveLength(1);
    });
  });

  // ADR 0012 §6, this screen's own slice. Driven off the table so a code the
  // contract gains is a code this test has never rendered.
  describe("the api-failure matrix", () => {
    it("renders the findings' failure over the findings alone", async () => {
      for (const code of REACHABLE) {
        seam.fetchReconciliationRuns.mockResolvedValue(ok(RUNS));
        seam.fetchFindings.mockResolvedValue({ ok: false, failure: apiFailure(code) });

        const { wrapper } = await mountAt("/reconciliation");

        // One failure, one dropped table, and the runs list standing. The runs
        // are a separate read; a findings outage is not a statement about the
        // pass, and the pass is the more reassuring of the two to render wrongly.
        expect(wrapper.findAll('[role="alert"]'), code).toHaveLength(1);
        expect(
          wrapper.findAll("table > caption").map((c) => c.text()),
          code,
        ).toEqual(["Reconciliation runs across the system"]);
        expect(wrapper.find('[role="alert"]').text(), code).toContain(
          CONSOLE_BEHAVIOUR[code].title,
        );
        // The evidence sections go with the rows they belong to: a failure view
        // above an evidence block for a list that could not be read would be a
        // reader looking at figures with no statement of where they came from.
        expect(wrapper.findAll("section[aria-label^='Evidence for finding']"), code).toHaveLength(
          0,
        );
        expect(wrapper.text(), code).not.toMatch(/No finding matches those filters/i);

        // The filter controls SURVIVE a failure: a retryable failure with no
        // controls is a dead end, and a `none` one leaves the reader unable to
        // narrow down what they were looking at.
        expect(wrapper.findAll('[role="radio"]'), code).toHaveLength(8);

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
        expect(wrapper.text().includes("req-recon-1"), code).toBe(
          CONSOLE_BEHAVIOUR[code].showRequestId,
        );
      }
    });

    it("renders the runs' failure over the runs alone, findings and all", async () => {
      for (const code of REACHABLE) {
        seam.fetchReconciliationRuns.mockResolvedValue({ ok: false, failure: apiFailure(code) });
        seam.fetchFindings.mockResolvedValue(ok(FINDINGS));

        const { wrapper } = await mountAt("/reconciliation");

        // A pass that could not be read is not a statement about the findings it
        // would have written, and the findings list is the more actionable of
        // the two. So the drop is the runs table and nothing else.
        expect(wrapper.findAll('[role="alert"]'), code).toHaveLength(1);
        expect(
          wrapper.findAll("table > caption").map((c) => c.text()),
          code,
        ).toEqual(["Findings across the system"]);
        expect(wrapper.findAll("section[aria-label^='Evidence for finding']"), code).toHaveLength(
          2,
        );
        expect(wrapper.find('[role="alert"]').text(), code).toContain(
          CONSOLE_BEHAVIOUR[code].title,
        );
        expect(wrapper.text(), code).not.toMatch(/No pass has run yet/i);

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
        expect(wrapper.text().includes("req-recon-1"), code).toBe(
          CONSOLE_BEHAVIOUR[code].showRequestId,
        );
      }
    });

    it("renders two failure views when both reads fail", async () => {
      seam.fetchReconciliationRuns.mockResolvedValue({
        ok: false,
        failure: apiFailure("internal"),
      });
      seam.fetchFindings.mockResolvedValue({ ok: false, failure: apiFailure("internal") });

      const { wrapper } = await mountAt("/reconciliation");

      // Two lists, two reads, two answers — and no table at all, because
      // "nothing to show" over a read that failed is the state the resource
      // header exists to make unreachable. The empty sentences are the
      // giveaway and are asserted absent for the same reason.
      expect(wrapper.findAll('[role="alert"]')).toHaveLength(2);
      expect(wrapper.findAll("table")).toHaveLength(0);
      expect(wrapper.text()).not.toMatch(/No pass has run yet/i);
      expect(wrapper.text()).not.toMatch(/No finding matches those filters/i);
    });
  });
});
