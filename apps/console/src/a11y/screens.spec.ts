/**
 * The console's automated accessibility gate: every screen, audited against
 * Loom's browserless rule set, in one file.
 *
 * ADR 0012 §7 promised this gate and it did not exist — the rule list was
 * declared on the roster and consumed by nothing, and `axe-core` was installed
 * and imported by nothing. This file is that promise, kept honestly: it runs
 * the 51 rules Loom measured as judgeable without a rendering engine, and it
 * names the 17 it does not run rather than letting a green run imply them.
 *
 * ── Why one file, and why it mounts through the router ───────────────────────
 *
 * The eight screens are audited in one place because the gate is a property of
 * the CONSOLE, not of a page: a screen that is added to `router/index.ts` and
 * not to this list would be an unaudited screen, and the roster in
 * `lib/arch/roster.ts` already accepts that a list of exemptions is a claim
 * somebody has to make. Here the claim runs the other way — `SCREENS` below is
 * an exhaustive table over the console's own route list, and a test asserts it
 * still is, so a ninth route cannot land without a ninth audit.
 *
 * Each screen is audited THROUGH THE SHELL, mounted as `App` over a memory
 * history rather than as a bare page component. That is not ceremony. The
 * shell is where the console's chrome lives — the skip link, the sidebar nav,
 * the header, the theme control, the sign-out button — and every one of those
 * is a focusable element with an accessible name, which is precisely the class
 * of thing `link-name`, `button-name` and `nested-interactive` judge. A gate
 * that mounted pages bare would pass while the navigation every operator uses
 * first was never looked at.
 *
 * ── The state each screen is audited in, and why that state ──────────────────
 *
 * Every screen is audited with its reads ANSWERED, not in its loading state,
 * and this is the single most important choice in the file. A screen's loading
 * branch renders a spinner and no rows; auditing that state audits almost
 * nothing, and a gate that passes on the empty skeleton while the populated
 * screen carries an unlabelled control is a gate that inspects nothing. The
 * fixtures below are therefore real contract-shaped rows — names, emails,
 * bucket ids, money — because the rules that fire on a populated list
 * (`th-has-data-cells` is browser-required, but `td-headers-attr`'s siblings,
 * `aria-required-children`, `list`, `listitem` and `duplicate-id-aria` are not)
 * only have something to say about rows that exist.
 *
 * Two screens cannot be fully audited in any single state, and both are named
 * in the report rather than quietly narrowed:
 *   - `/sign-in` is a form before input. It is audited as rendered at mount,
 *     with no values typed, which is the state every visitor meets first.
 *   - `/accounting` renders a SECOND table (one bucket's ledger) only after a
 *     bucket is chosen, so the gate drives that choice through the screen's
 *     own control and audits the two-table state too.
 */
import { createPinia, setActivePinia } from "pinia";
import { flushPromises, mount, type VueWrapper } from "@vue/test-utils";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { createMemoryHistory, type Router } from "vue-router";

/**
 * The seam, mocked at the boundary every screen's data arrives through — never
 * at a composable, because a composable stub would prove only that the screen
 * reads a stub. `importActual` keeps the rest of the module real, so the
 * session store the route guard reaches for is the real one.
 */
const seam = vi.hoisted(() => ({
  getSessionResult: vi.fn(),
  signOutOfSession: vi.fn(),
  signInWith: vi.fn(),
  getHealth: vi.fn(),
  getReadiness: vi.fn(),
  fetchAccountOverview: vi.fn(),
  fetchUsers: vi.fn(),
  fetchApiKeys: vi.fn(),
  createApiKey: vi.fn(),
  fetchPlans: vi.fn(),
  fetchSubscriptions: vi.fn(),
  fetchEntitlements: vi.fn(),
  fetchFundingBuckets: vi.fn(),
  fetchLedgerEntries: vi.fn(),
  fetchFindings: vi.fn(),
  fetchReconciliationRuns: vi.fn(),
}));

vi.mock("@/lib/api", async () => {
  const actual = await vi.importActual<typeof import("@/lib/api")>("@/lib/api");
  return { ...actual, ...seam };
});

import App from "@/app/App.vue";
import { createConsoleRouter, ROUTES } from "@/router";
import { audit, describe as describeAudit, NOT_RUN, RUN_ONLY, undecidedIds } from "@/lib/a11y-gate";
import { BROWSERLESS_RULES } from "@ecoma-io/loom/a11y";
import type { ApiResult } from "@/lib/api";
import type {
  AccountOverview,
  ApiKeyPage,
  EntitlementPage,
  FindingPage,
  FundingBucket,
  FundingBucketPage,
  LedgerEntry,
  LedgerEntryPage,
  Plan,
  PlanPage,
  Principal,
  ReconciliationRun,
  ReconciliationRunPage,
  Subscription,
  SubscriptionPage,
  User,
  UserPage,
} from "@ecoma-io/llm-gateway-console-api-client";

const ok = <T>(data: T): ApiResult<T> => ({ ok: true, data });

const ACCOUNT_ID = "a0000000-0000-4000-8000-0000000000a1";

const PRINCIPAL: Principal = {
  class: "user",
  account_id: ACCOUNT_ID,
  user_id: "u0000000-0000-4000-8000-0000000000u1",
  email: "ops@example.test",
};

/**
 * Fixtures, typed as the contract's own shapes so a schema change breaks this
 * file at compile time rather than leaving a screen audited against a shape it
 * no longer receives. Every one carries a real name, address and id: the rules
 * this gate runs judge accessible NAMES, and a fixture of `"item-1"`/`"item-2"`
 * would make a table of indistinguishable rows look like a page.
 */
const OVERVIEW: AccountOverview = {
  account: {
    id: ACCOUNT_ID,
    name: "Northwind Robotics",
    state: "active",
    created_at: "2026-01-04T09:00:00Z",
    updated_at: "2026-09-01T09:00:00Z",
  },
  user_count: 3,
  active_api_key_count: 2,
  open_finding_count: 1,
  subscriptions: [],
  payg_balances: [],
};

const USERS: UserPage = {
  items: [
    {
      id: "u0000000-0000-4000-8000-0000000000u1",
      account_id: ACCOUNT_ID,
      email: "ops@example.test",
      state: "active",
      created_at: "2026-09-01T00:00:00Z",
      updated_at: "2026-09-02T00:00:00Z",
    },
    {
      id: "u0000000-0000-4000-8000-0000000000u2",
      account_id: ACCOUNT_ID,
      email: "auditor@example.test",
      state: "invited",
      created_at: "2026-09-03T00:00:00Z",
      updated_at: "2026-09-03T00:00:00Z",
    },
  ] satisfies User[],
  next_cursor: "c-users-3",
  has_more: false,
};

const KEYS: ApiKeyPage = {
  items: [
    {
      id: "d0000000-0000-4000-8000-0000000000d1",
      account_id: ACCOUNT_ID,
      display_name: "ci pipeline",
      prefix: "Gk_d0000000",
      state: "active",
      created_at: "2026-09-04T00:00:00Z",
    },
    {
      id: "d0000000-0000-4000-8000-0000000000d2",
      account_id: ACCOUNT_ID,
      display_name: "staging runner",
      prefix: "Gk_d0000000",
      state: "revoked",
      created_at: "2026-09-05T00:00:00Z",
    },
  ],
  next_cursor: "c-keys-3",
  has_more: false,
};

const PLANS: PlanPage = {
  items: [
    {
      id: "p0000000-0000-4000-8000-0000000000p1",
      name: "Starter",
      created_at: "2026-01-10T00:00:00Z",
    },
    {
      id: "p0000000-0000-4000-8000-0000000000p2",
      name: "Growth",
      created_at: "2026-02-10T00:00:00Z",
    },
  ] satisfies Plan[],
  next_cursor: "c-plans-3",
  has_more: false,
};

const SUBSCRIPTIONS: SubscriptionPage = {
  items: [
    {
      id: "s0000000-0000-4000-8000-0000000000s1",
      account_id: ACCOUNT_ID,
      plan_version_id: "p0000000-0000-4000-8000-0000000000p2",
      state: "active",
      current_period_start: "2026-09-01T00:00:00Z",
      current_period_end: "2026-10-01T00:00:00Z",
      created_at: "2026-03-01T00:00:00Z",
    },
  ] satisfies Subscription[],
  next_cursor: "c-subscriptions-2",
  has_more: false,
};

const ENTITLEMENTS: EntitlementPage = {
  items: [
    {
      id: "e0000000-0000-4000-8000-0000000000e1",
      subscription_id: "s0000000-0000-4000-8000-0000000000s1",
      cycle: 7,
      scope: "default",
      dimension: "cost",
      granted_minor_units: 5_000_00,
      state: "active",
      period_start: "2026-09-01T00:00:00Z",
      period_end: "2026-10-01T00:00:00Z",
    },
    // A grant too large for the money formatter to render exactly, so the
    // screen's unrenderable branch — the one that puts an `aria-label` on a
    // roleless `<span>` — is actually reached. A fixture of well-behaved
    // amounts would leave the most interesting markup on this screen unaudited
    // while still reporting a green run, which is the failure this whole file
    // exists to make impossible.
    {
      id: "e0000000-0000-4000-8000-0000000000e2",
      subscription_id: "s0000000-0000-4000-8000-0000000000s1",
      cycle: 8,
      scope: "default",
      dimension: "cost",
      // Beyond `Number.MAX_SAFE_INTEGER` minor units: the domain holds 64-bit
      // money, so this is a real value and not a contrived one. Written through
      // `BigInt` and then narrowed, because the literal itself does not survive
      // a round trip through a double and ESLint's `no-loss-of-precision`
      // rightly refuses it.
      granted_minor_units: Number(BigInt("9007199254740993")),
      state: "active",
      period_start: "2026-10-01T00:00:00Z",
      period_end: "2026-11-01T00:00:00Z",
    },
  ],
  next_cursor: "c-entitlements-3",
  has_more: false,
};

const BUCKET: FundingBucket = {
  id: "b0000000-0000-4000-8000-0000000000b1",
  kind: "account",
  account_id: ACCOUNT_ID,
  status: "active",
  balances: {
    settled: { minor_units: 1_250_00 },
    held: { minor_units: 300_00 },
    available: { minor_units: 950_00 },
  },
  version: 7,
  created_at: "2026-01-15T00:00:00Z",
};

const BUCKETS: FundingBucketPage = {
  items: [BUCKET],
  next_cursor: "c-buckets-2",
  has_more: false,
};

const LEDGER: LedgerEntryPage = {
  items: [
    {
      id: "l0000000-0000-4000-8000-0000000000l1",
      funding_bucket_id: BUCKET.id,
      kind: "topup",
      sequence: 1,
      settled_delta: { minor_units: 1_000_00 },
      held_delta: { minor_units: 0 },
      created_at: "2026-09-06T00:00:00Z",
    },
    {
      id: "l0000000-0000-4000-8000-0000000000l2",
      funding_bucket_id: BUCKET.id,
      kind: "consume",
      sequence: 2,
      settled_delta: { minor_units: -250_00 },
      held_delta: { minor_units: 0 },
      price: {
        revision_id: "r0000000-0000-4000-8000-0000000000r1",
        input_unit_price: { minor_units: 3 },
        output_unit_price: { minor_units: 15 },
      },
      created_at: "2026-09-07T00:00:00Z",
    },
  ] satisfies LedgerEntry[],
  next_cursor: "c-ledger-3",
  has_more: false,
};

const RUNS: ReconciliationRunPage = {
  items: [
    {
      id: 41,
      scope: "all",
      status: "completed",
      window_from: "2026-09-08T00:00:00Z",
      window_to: "2026-09-09T00:00:00Z",
      started_at: "2026-09-09T00:00:00Z",
      finished_at: "2026-09-09T00:00:04Z",
      buckets_scanned: 12,
      findings_opened: 1,
      findings_unchanged: 3,
    },
  ] satisfies ReconciliationRun[],
  next_cursor: "c-runs-2",
  has_more: false,
};

const FINDINGS: FindingPage = {
  items: [
    {
      id: "f0000000-0000-4000-8000-0000000000f1",
      check_kind: "settled_exceeds_grant",
      subject_kind: "funding_bucket",
      subject_id: BUCKET.id,
      severity: "warning",
      status: "open",
      detail: "The bucket settled more than its grant allowed in this window.",
      detected_at: "2026-09-09T00:00:02Z",
      last_seen_at: "2026-09-09T00:00:02Z",
    },
  ],
  next_cursor: "c-findings-2",
  has_more: false,
};

/** Every read the console can make, answered. Anything unmocked resolves `undefined`, which is a failure a screen renders. */
function answerEveryRead(): void {
  seam.getSessionResult.mockResolvedValue(ok(PRINCIPAL));
  seam.getHealth.mockResolvedValue({ data: { status: "ok" } });
  seam.getReadiness.mockResolvedValue({ data: { status: "ok" } });
  seam.fetchAccountOverview.mockResolvedValue(ok(OVERVIEW));
  seam.fetchUsers.mockResolvedValue(ok(USERS));
  seam.fetchApiKeys.mockResolvedValue(ok(KEYS));
  seam.fetchPlans.mockResolvedValue(ok(PLANS));
  seam.fetchSubscriptions.mockResolvedValue(ok(SUBSCRIPTIONS));
  seam.fetchEntitlements.mockResolvedValue(ok(ENTITLEMENTS));
  seam.fetchFundingBuckets.mockResolvedValue(ok(BUCKETS));
  seam.fetchLedgerEntries.mockResolvedValue(ok(LEDGER));
  seam.fetchFindings.mockResolvedValue(ok(FINDINGS));
  seam.fetchReconciliationRuns.mockResolvedValue(ok(RUNS));
}

const mounted: VueWrapper[] = [];

/**
 * Run one whole audit: mount, audit, and tear the DOM down again.
 *
 * The teardown is the non-obvious part, and without it this gate reports
 * defects that exist in no browser. `useId()` is what makes every rendered id
 * unique inside ONE application — `DataTable`'s caption ids are literally
 * `data-table-v-0`/`data-table-v-1` — but Vitest's per-file `appContext` is
 * created ONCE, so every test's counter restarts at `v-0`. An `attachTo`
 * wrapper left in the document from the previous test therefore holds ids the
 * next test reissues, and `duplicate-id-aria` fires on ids that were never
 * duplicated in a browser. Clearing the body between audits is what keeps that
 * distinction honest, and it means the ids in each result are this screen's own.
 */
async function auditScreen(
  path: string,
  query = "",
): Promise<{ readonly judged: number; readonly message: string; readonly undecided: string[] }> {
  const wrapper = await mountAt(path, query);
  const result = await audit(wrapper.element);
  const wrapperToRemove = mounted.pop();
  wrapperToRemove?.unmount();
  document.body.innerHTML = "";
  return {
    // `passes` and `violations` are the groups a rule appears in when it RAN;
    // a rule that matched nothing is `inapplicable` and is in neither.
    judged: result.passes.length + result.violations.length,
    message: describeAudit(result, path),
    undecided: undecidedIds(result),
  };
}

beforeAll(() => {
  // Loom's `SegmentedControl` sizes its selection indicator on mount and jsdom
  // ships no `ResizeObserver`. Stubbed rather than polyfilled, for the reason
  // every page spec in this package gives: the indicator's geometry is not what
  // this gate is about, and an observer that fabricated rectangles would be a
  // second thing lying to the audit. Note the honest consequence, which is that
  // nothing about the indicator's rendered size is judged here — it belongs to
  // the browser-required tier, where `target-size` and `color-contrast` live.
  class NoopResizeObserver {
    observe(): void {}
    unobserve(): void {}
    disconnect(): void {}
  }
  vi.stubGlobal("ResizeObserver", NoopResizeObserver);
});

afterEach(() => {
  while (mounted.length > 0) mounted.pop()!.unmount();
  document.body.innerHTML = "";
});

/**
 * Mount the whole console at a route, signed in, with every read answered.
 *
 * A router and a pinia per test, for the reason `IdentityPage.spec.ts` gives:
 * both carry the previous test's state, and a gate whose third assertion is
 * really about the second test's session is worse than no gate.
 */
async function mountAt(path: string, query = ""): Promise<VueWrapper> {
  const pinia = createPinia();
  setActivePinia(pinia);
  const router: Router = createConsoleRouter(createMemoryHistory());
  await router.push(`${path}${query}`);
  await router.isReady();
  const wrapper = mount(App, {
    attachTo: document.body,
    global: { plugins: [router, pinia] },
  });
  mounted.push(wrapper);
  // Two rounds, as every page spec in this package does: the composable reads
  // on mount, and a pager's own watcher reads again when the first answer
  // lands. Asserting between them audits a state no reader ever saw.
  await flushPromises();
  await flushPromises();
  return wrapper;
}

beforeEach(() => {
  for (const mock of Object.values(seam)) mock.mockReset();
  answerEveryRead();
});

/**
 * The eight screens the console has, as a table.
 *
 * Exhaustive over the route table by design: the assertion below is what stops
 * a ninth route from landing unaudited. `not-found` is excluded and SAYS SO —
 * it is the catch-all, and auditing the catch-all tells you about the route
 * that matched nothing rather than about a screen, which is the one case where
 * "all screens" would be a claim rather than a fact.
 */
const SCREENS: ReadonlyArray<{ readonly path: string; readonly label: string }> = [
  { path: "/", label: "the dashboard" },
  { path: "/sign-in", label: "sign-in" },
  { path: "/identity", label: "identity" },
  { path: "/catalog", label: "catalog" },
  { path: "/commerce", label: "commerce" },
  { path: "/accounting", label: "accounting" },
  { path: "/reconciliation", label: "reconciliation" },
  { path: "/gateway-status", label: "gateway status" },
];

describe("the console's automated accessibility gate", () => {
  it("runs every rule Loom measured as judgeable without a browser, and none of the ones it did not", () => {
    // The gate's own configuration, asserted rather than assumed. A Loom release
    // that re-partitions the tier is a change to WHAT this gate promises, and
    // it should arrive as a failing test a reviewer reads — not as a silently
    // different rule count.
    expect(RUN_ONLY.type).toBe("rule");
    expect([...RUN_ONLY.values].sort()).toEqual([...BROWSERLESS_RULES].sort());
    expect(RUN_ONLY.values).toHaveLength(51);

    // And the negative claim, which is the one that would be dangerous: a rule
    // that needs a rendering engine must never appear in a jsdom run.
    // `color-contrast` is named because it is the rule an operator is most
    // likely to assume is covered, and in jsdom it returns `cantTell` on every
    // node — a green run of this gate says nothing whatsoever about contrast.
    for (const rule of NOT_RUN) {
      expect(RUN_ONLY.values).not.toContain(rule);
    }
    expect(NOT_RUN).toHaveLength(17);
    expect(RUN_ONLY.values).not.toContain("color-contrast");
    expect(RUN_ONLY.values).not.toContain("target-size");
  });

  it("audits every screen on the console's route table, and none has been added without one", () => {
    const declared = ROUTES.filter(
      (route) => route.name !== "not-found" && typeof route.path === "string",
    ).map((route) => route.path);
    expect([...SCREENS.map((screen) => screen.path)].sort()).toEqual([...declared].sort());
  });

  it.each(SCREENS)("$label has no axe violation", async ({ path, label }) => {
    const result = await auditScreen(path);
    // The screen's own name in the message, not its path: the message is what a
    // reviewer reads in CI, and "identity breaks the automated accessibility
    // gate" names the surface while "/identity" is a URL to go and look up.
    expect(result.message.replace(path, label)).toBe("");
  });

  it("audits a real number of rules on every screen", async () => {
    // A gate that reports "0 violations" because nothing matched is
    // indistinguishable from a gate that reports it because everything passed.
    // The floor below is what separates them: a screen that renders less than
    // this is not being audited. The number is far under the 51 configured,
    // because most rules are inapplicable to a given page — a screen with no
    // definition list makes `definition-list` inapplicable, not passed.
    const judged: Record<string, number> = {};
    for (const { path, label } of SCREENS) {
      const result = await auditScreen(path);
      judged[label] = result.judged;
    }
    // Stated as a floor, not a target: if a future Loom or axe release stops a
    // rule from matching, this goes red and a reviewer decides whether the page
    // really lost the element or the rule really stopped applying. A gate whose
    // scope can silently shrink to nothing is not a gate.
    for (const [label, count] of Object.entries(judged)) {
      expect(count, `rules that judged ${label}`).toBeGreaterThan(5);
    }
  });

  it("leaves no rule undecided, except the one this console has argued about on purpose", async () => {
    // `incomplete` is axe saying "I could not decide", which is NOT a pass, and
    // a gate that reported only `violations.length` would hide it. Every
    // undecided rule on every screen must therefore be named here.
    //
    // There is exactly one, and it is a design decision rather than an oversight:
    // `aria-prohibited-attr` on `MoneyAmount`'s `<span>`. axe reports
    // `aria-label` on a roleless generic element as incomplete because whether
    // the label is honoured depends on the browser's mapping of that element's
    // role, which jsdom cannot decide and the rule therefore asks a human about.
    // `MoneyAmount` puts the label there deliberately — an amount too large to
    // render exactly must not be read out as digits a reader would take at face
    // value — and the judgement was made in the component's own docblock, not
    // waved through here. The alternative, an `aria-live` or a `role="img"`, was
    // rejected for the reason that component already records.
    //
    // The rest of the list MUST be empty, and it is a list rather than a count so
    // that a new undecided rule on a new screen arrives as a named entry a
    // reviewer has to look at.
    const undecided: Record<string, string[]> = {};
    for (const { path, label } of SCREENS) {
      const result = await auditScreen(path);
      // Only the screens that HAVE an undecided rule appear, in the shape
      // `NON_LIST_PAGES` in `lib/arch/roster.ts` already established: an
      // exemption is a claim somebody has to make, and a claim in a list is one
      // this file can print. A screen appearing here is a claim to review, not a
      // silent default.
      if (result.undecided.length > 0) undecided[label] = result.undecided;
    }
    expect(undecided).toEqual({ commerce: ["aria-prohibited-attr"] });
  });

  it("audits the accounting screen in its two-table state, after a bucket is chosen", async () => {
    // The ledger table is behind a `v-if` on a selected bucket, so a gate that
    // only ever mounted `/accounting` would audit one table and report the
    // screen as covered. The selection is made through the screen's OWN control
    // — a `router.push` here would set the filter without ever exercising the
    // path a reader takes, and the markup the reader's click produces is part of
    // what is being audited.
    const wrapper = await mountAt("/accounting");
    // The bucket cell renders the id as a `<button aria-pressed>` — the screen's
    // own affordance, matched on the id rather than on a word in a label, so
    // this stays the control a reader presses rather than a guess at its text.
    const chooseBucket = wrapper.findAll("button").find((button) => button.text() === BUCKET.id);
    expect(chooseBucket, "the bucket row offers a control to open its ledger").toBeDefined();

    // If the click did not open the ledger, the audit below would be auditing
    // the same single-table state again and reporting success. Assert the state
    // CHANGED first, so a screen that stopped opening its ledger fails here
    // rather than quietly reducing this gate's coverage.
    expect(wrapper.findAll("table")).toHaveLength(1);
    await chooseBucket!.trigger("click");
    await flushPromises();
    await flushPromises();
    expect(wrapper.findAll("table")).toHaveLength(2);
    expect(seam.fetchLedgerEntries).toHaveBeenCalled();

    const result = await audit(wrapper.element);
    expect(describeAudit(result, "accounting with a bucket selected")).toBe("");
  });
});
