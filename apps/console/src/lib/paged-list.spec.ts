// The module a screen declares its list to, and the three ways that declaration
// can be wrong in a way the screen itself cannot see.
//
// `lib/paged-list.ts` is the only place in the console that turns a position in
// the address bar into a request, so the failures it can have are the ones that
// are INVISIBLE from a screen: a second read of a page the reader already has
// (a loop the screen renders identically, because the data it ends up with is
// the data it would have shown anyway), and a cursor belonging to a different
// list (a request the server refuses, which the console then answers by
// dropping the cursor — so the reader loses their place on a list they were not
// reading).
//
// Every test here drives the real `usePagedList` against a real router. The
// loop is a function of the watcher, the flush mode and the assignment in
// `useResource` interacting, so a unit test of any one of the three would
// pass while the combination spins; only mounting it measures the thing.
import { defineComponent, h, nextTick } from "vue";
import { createRouter, createWebHistory, type Router } from "vue-router";
import { mount } from "@vue/test-utils";
import { afterEach, beforeEach, describe, expect, expectTypeOf, it, vi } from "vitest";

import { usePagedList } from "@/lib/paged-list";
import type { ApiResult } from "@/lib/api";
import { CONSOLE_BEHAVIOUR, type ApiErrorCode } from "@/lib/failure-matrix";

/**
 * The contract's `Error.code` enum, pinned to the contract rather than derived
 * from the matrix — see the long form in `FailureView.spec.ts`, which this is
 * the same list as. The one-line reason it cannot be `Object.keys` of the table
 * under test: that derivation is a tautology and proves nothing about coverage
 * in either direction. The type ties it to the generated union so the gate
 * holds at the compiler, which is the gate a screen needs it at (ADR 0012 §6).
 *
 * Twelve members, and `conflict` is the newest: the payment surface's refusal
 * of a well-formed request on the server's own state. The list has to carry it
 * for the same reason the matrix does — this is what makes the coverage claim
 * below a claim rather than a restatement of whatever the table happens to
 * contain.
 */
const CONTRACT_CODES = [
  "not_found",
  "method_not_allowed",
  "invalid_request",
  "unauthenticated",
  "cursor_expired",
  "unsupported_version",
  "revision_gap",
  "snapshot_required",
  "conflict",
  "upstream_unavailable",
  "service_unavailable",
  "internal",
] as const satisfies readonly ApiErrorCode[];

expectTypeOf<(typeof CONTRACT_CODES)[number]>().toEqualTypeOf<ApiErrorCode>();

/** A page the contract's envelope would produce: two fields and a rows array. */
function pageOf<T>(items: readonly T[], hasMore = false) {
  return { items, next_cursor: "cursor-2", has_more: hasMore };
}

/** The page type these tests read, named once so `read` is typed to it. */
type TestPage = ReturnType<typeof pageOf<{ id: string }>>;

/**
 * The one page object a tripped ceiling keeps handing back. Hoisted so its
 * IDENTITY is the same on every call — see the ceiling's comment.
 */
const HELD: TestPage = pageOf([{ id: "row-held" }], false);

type Harness = {
  router: Router;
  calls: { query: Record<string, unknown> }[];
  read: (query: Record<string, unknown>) => Promise<ApiResult<TestPage>>;
  /** The list the screen was handed, so a test can reach the refs it renders from. */
  list: ReturnType<typeof usePagedList<TestPage, Record<string, unknown>>>;
  mounted: () => void;
  advance: () => Promise<void>;
};

/**
 * A real router on a real `/accounting`, one `usePagedList`, and a list of
 * every query that list was asked for. `respond` decides the answer so a test
 * can make the server refuse a cursor the way the contract says it will, or
 * hold a read open so a test can observe the module mid-flight.
 *
 * `respond` may return a PROMISE as well as a result: `read` is async and
 * returns whatever `respond` gives it without awaiting, so both spellings reach
 * the same place, and a test that needs to hold a read open has no other way
 * to say so.
 */
async function harness(
  query: Record<string, string>,
  respond?: (query: Record<string, unknown>) => ApiResult<TestPage> | Promise<ApiResult<TestPage>>,
  ceiling = 50,
): Promise<Harness> {
  const router = createRouter({
    history: createWebHistory(),
    routes: [{ path: "/accounting", component: { template: "<div />" } }],
  });
  await router.push({ path: "/accounting", query });
  await router.isReady();

  const calls: { query: Record<string, unknown> }[] = [];
  const read = async (q: Record<string, unknown>): Promise<ApiResult<TestPage>> => {
    calls.push({ query: q });
    // A ceiling, so the loop this file is a regression test for ends in an
    // ASSERTION rather than in an exhausted heap. Left unbounded, the failing
    // run does not fail — the vitest worker dies of SIGABRT some seconds
    // later, which is a red build either way but says nothing about which test
    // broke and cannot be read as a verdict on this file.
    //
    // Past the ceiling the answer is a STANDING one — the same object every
    // time, not a fresh page. The watcher keys on `data`'s identity, so a
    // refusal that left the old page in place would keep re-arming it and the
    // loop would outlive its own guard; a fresh page each time would spin
    // forever. One constant object does neither: the watcher's source stops
    // changing, the run returns, and the test's assertion sees the count.
    if (calls.length > ceiling) return { ok: true, data: HELD };
    // Awaited, because `respond` may be async — the read's whole value is the
    // promise it returns, and `useResource` is already awaiting this one, so
    // awaiting here is the same await rather than a second tick.
    return respond ? await respond(q) : { ok: true, data: pageOf([{ id: "row-1" }], true) };
  };

  // The list the probe's `setup` captured, so a test can reach the refs the
  // screen was handed. Declared before the mount and narrowed on the way out,
  // because the assignment happens inside `setup` and the compiler cannot see
  // that it always runs.
  let list!: Harness["list"];

  const Probe = defineComponent({
    setup() {
      // Typed as `TestPage` rather than as `ReturnType<typeof pageOf>`: the row
      // element type is then `{ id: string }` rather than `unknown`, which is
      // what makes the returned list assignable to `Harness["list"]`. The two
      // name the same shape — the annotation only names the element.
      list = usePagedList<TestPage, Record<string, unknown>>({
        read,
        shape: { filters: ["kind"], cursor: "after" },
        vocabulary: { kind: ["consume", "grant"] },
      });
      return () => h("div", String(list.rows.value.length));
    },
  });
  const wrapper = mount(Probe, { global: { plugins: [router] } });

  return {
    router,
    calls,
    read,
    list,
    mounted: () => wrapper.unmount(),
    advance: async () => {
      // Let every already-queued navigation COMMIT before reading the address
      // bar. `router.replace` is a promise, and a cursor restart is one: a
      // test that only ticks has read the URL before the navigation the
      // module asked for has landed, and would then assert that nothing
      // happened — which is exactly the shape of the bug this file exists for.
      await nextTick();
      await new Promise((resolve) => setTimeout(resolve, 0));
      await nextTick();
    },
  };
}

describe("one mount, one read", () => {
  it("asks the server once, however long the test waits", async () => {
    // The regression. A watcher that observes the read's own RESULT re-fires
    // when the result arrives, because a successful run assigns a freshly
    // parsed object to `data`, so the reload triggers the reload. Measured at
    // 61 reads in 200 ms before the fix, and the loop had no natural end.
    //
    // The harness's ceiling is what makes this a VERDICT rather than a
    // casualty: with the buggy watcher the run does not fail, it dies — the
    // vitest worker exhausts its heap and exits on SIGABRT seconds later,
    // which is a red build that says nothing about which assertion broke.
    // A low ceiling converts the same defect into one honest failure.
    const h1 = await harness({}, undefined, 8);
    for (let i = 0; i < 20; i += 1) await new Promise((r) => setTimeout(r, 10));

    expect(h1.calls).toHaveLength(1);
    h1.mounted();
  });

  it("asks again when the position changes, which is the watcher's whole job", async () => {
    // The other side of the same fix: dropping `data` from the source must not
    // have cost the module the ability to reload, or "stop looping" would have
    // been a cheaper defect to ship than "stop reading".
    const h1 = await harness({});
    await h1.advance();
    expect(h1.calls).toHaveLength(1);

    await h1.router.push({ path: "/accounting", query: { kind: "consume" } });
    await h1.advance();

    expect(h1.calls).toHaveLength(2);
    expect(h1.calls[1]?.query).toMatchObject({ kind: "consume" });
    h1.mounted();
  });

  it("sends the cursor back verbatim and never manufactures one", async () => {
    const h1 = await harness({ after: "cursor-2" });
    await h1.advance();

    expect(h1.calls).toHaveLength(1);
    // Byte-for-byte. A cursor the console parsed, compared or rebuilt is a
    // cursor the server never issued.
    expect(h1.calls[0]?.query).toMatchObject({ after: "cursor-2" });
    h1.mounted();
  });

  it("reads a value outside the filter vocabulary as no filter, not as a 400", async () => {
    // The URL is user input. A link with `?kind=not-a-kind` must render the
    // unfiltered list rather than hand the visitor the contract's 400.
    const h1 = await harness({ kind: "not-a-kind" });
    await h1.advance();

    expect(h1.calls[0]?.query).not.toHaveProperty("kind");
    h1.mounted();
  });
});

describe("two lists on one route", () => {
  /**
   * The accounting screen's shape, mounted for real: a bucket list that
   * declares no filters, and a ledger that declares one, both watching the same
   * route at the same time. This is the arrangement in which a single
   * route-global cursor key is a defect rather than a simplification.
   */
  async function twoLists(query: Record<string, string>) {
    const router = createRouter({
      history: createWebHistory(),
      routes: [{ path: "/accounting", component: { template: "<div />" } }],
    });
    await router.push({ path: "/accounting", query });
    await router.isReady();

    const bucketCalls: { query: Record<string, unknown> }[] = [];
    const ledgerCalls: { query: Record<string, unknown> }[] = [];
    const guard = (list: { query: Record<string, unknown> }[]): ApiResult<TestPage> => {
      if (list.length > 50) return { ok: true, data: HELD };
      return { ok: true, data: pageOf([{ id: "row" }], false) };
    };

    const Probe = defineComponent({
      setup() {
        const buckets = usePagedList<TestPage, Record<string, unknown>>({
          read: async (q) => {
            bucketCalls.push({ query: q });
            return guard(bucketCalls);
          },
          shape: { filters: [], cursor: "after" },
          vocabulary: {},
        });
        const ledger = usePagedList<TestPage, Record<string, unknown>>({
          read: async (q) => {
            ledgerCalls.push({ query: q });
            return guard(ledgerCalls);
          },
          shape: { filters: ["kind"], cursor: "ledger_after" },
          vocabulary: { kind: ["consume"] },
        });
        return () => h("div", `${buckets.rows.value.length}:${ledger.rows.value.length}`);
      },
    });
    const wrapper = mount(Probe, { global: { plugins: [router] } });
    return { router, bucketCalls, ledgerCalls, unmount: () => wrapper.unmount() };
  }

  it("gives each list its own cursor, so neither sends the other's", async () => {
    // The defect. With one route-global `after`, the bucket list's Next writes a
    // position and the LEDGER — watching the same route — reads it and sends it
    // against its own operation. That is a cursor the server issued for a
    // different collection, which the contract answers with `400
    // invalid_request`, which the console answers by dropping the cursor, which
    // loses the reader's place on a list they were not reading.
    //
    // Both keys are present in the URL, which is the hard case: a test that
    // only ever loads `after` cannot tell a shared key from a named one, which
    // is exactly how this passed all ten tests before the key was declared.
    const h1 = await twoLists({ after: "bucket-cursor", ledger_after: "ledger-cursor" });
    await new Promise((r) => setTimeout(r, 0));
    await new Promise((r) => setTimeout(r, 0));

    expect(h1.bucketCalls[0]?.query).toMatchObject({ after: "bucket-cursor" });
    expect(h1.ledgerCalls[0]?.query).toMatchObject({ after: "ledger-cursor" });
    // Neither may send a key that is not its own, and neither may be handed
    // the other's position under any name.
    expect(h1.bucketCalls[0]?.query).not.toHaveProperty("ledger_after");
    expect(h1.ledgerCalls[0]?.query).toHaveProperty("after", "ledger-cursor");
    h1.unmount();
  });

  it("keeps a position change on one list off the other", async () => {
    // The same defect seen from the pager rather than from the read: moving the
    // bucket list must not move the ledger, even though both live on this URL.
    const h1 = await twoLists({ after: "bucket-1", ledger_after: "ledger-1" });
    await new Promise((r) => setTimeout(r, 0));

    const before = h1.ledgerCalls.length;
    await h1.router.push({ path: "/accounting", query: { after: "bucket-2" } });
    await new Promise((r) => setTimeout(r, 0));
    await new Promise((r) => setTimeout(r, 0));

    // Both re-read — the ledger is still on `ledger_after`, which did not move,
    // so a correctly-shaped implementation may or may not re-issue it. What it
    // must never do is send the bucket list's new cursor.
    for (const call of h1.ledgerCalls.slice(before)) {
      expect(call.query).not.toHaveProperty("after", "bucket-2");
    }
    h1.unmount();
  });
});

describe("a cursor the server will not place", () => {
  const refused: Readonly<Record<string, ApiErrorCode>> = {
    cursor_expired: "cursor_expired",
    invalid_request: "invalid_request",
  };

  for (const [label, code] of Object.entries(refused)) {
    it(`drops only its own cursor on ${label}`, async () => {
      // The failure is on ONE list and the reader's place on it is the only
      // thing that should move. An earlier version rebuilt the query from the
      // list's own declared filters, so on the accounting screen — where
      // `bucket` selects which table is shown rather than filtering one — a
      // cursor_expired on the LEDGER navigated to a URL with no `bucket` in it.
      // The selected bucket was gone, the ledger section replaced itself with
      // "choose a bucket", and `replace` meant Back could not undo it.
      const h1 = await harness(
        { bucket: "bucket-1", kind: "consume", after: "stale", ledger_after: "stale-ledger" },
        () => ({
          ok: false,
          failure: {
            kind: "api",
            unauthenticated: false,
            envelope: { error: { code, message: "gone" }, request_id: "req-1" },
          },
        }),
      );
      await h1.advance();
      await h1.advance();

      const landed = new URLSearchParams(h1.router.currentRoute.value.fullPath.split("?")[1] ?? "");
      // The key this list did NOT own survives, or the screen around it is gone.
      expect(landed.get("ledger_after"), label).toBe("stale-ledger");
      // Its own declared filter survives.
      expect(landed.get("kind"), label).toBe("consume");
      // Only its own cursor goes.
      expect(landed.get("after"), label).toBeNull();
      h1.mounted();
    });
  }

  it("leaves a URL no server answer touches exactly as it found it", async () => {
    // The negative space around the rule above: nothing was refused, so
    // nothing should have been navigated.
    const h1 = await harness({ after: "cursor-2" });
    await h1.advance();
    const before = h1.router.currentRoute.value.fullPath;

    await h1.advance();
    await h1.advance();

    expect(h1.router.currentRoute.value.fullPath).toBe(before);
    h1.mounted();
  });
});

describe("what the console reads from the matrix", () => {
  it("reads the recovery column rather than restating it", () => {
    // Every code the contract declares has a row, keyed exhaustively over the
    // generated union. The set comparison is what makes the claim checkable:
    // deriving the code list off the table and then checking each derived code
    // has a row is a tautology that holds for a table of eleven rows and for an
    // empty one alike. The list is pinned to the contract instead, in the type
    // system — `satisfies` proves the list names only contract codes, and
    // `toEqualTypeOf` proves it names all of them — so a code the contract
    // gained and a code the table invented are each a different compile error.
    const declared = Object.keys(CONSOLE_BEHAVIOUR).sort();
    expect(declared).toEqual([...CONTRACT_CODES].sort());
    for (const code of declared) {
      expect(CONSOLE_BEHAVIOUR[code as ApiErrorCode].recovery, code).toBeTruthy();
    }
  });
});

describe("the module does not reach past its declaration", () => {
  let warn: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    warn = vi.spyOn(console, "warn").mockImplementation(() => undefined);
  });
  afterEach(() => warn.mockRestore());

  it("hands the screen a readonly loading flag, and the write does not take", async () => {
    // Vue reports a write to a readonly ref as a warning rather than an
    // exception, so asserting only that unmounting is quiet proves nothing: it
    // is a property of the teardown, not of the flag, and a module that handed
    // out a plain writable `loading` would satisfy it exactly as well. So the
    // write is ATTEMPTED, and both halves of the claim are checked — the flag
    // is still true afterwards, and Vue complained about it.
    //
    // The read is HELD rather than let to settle, because `loading` is true only
    // between a read starting and its answer landing. A write attempted
    // against an idle `loading` is a write against `false`, and a module whose
    // `loading` was stuck on would pass it.
    let release!: () => void;
    const held = new Promise<void>((resolve) => {
      release = resolve;
    });
    const h1 = await harness({}, async (): Promise<ApiResult<TestPage>> => {
      await held;
      return { ok: true, data: pageOf([{ id: "row-1" }], true) };
    });

    // Still in flight, so the flag under test is the one that says "busy".
    expect(h1.list.loading.value).toBe(true);
    warn.mockClear();
    (h1.list.loading as unknown as { value: boolean }).value = false;
    expect(h1.list.loading.value).toBe(true);
    expect(warn).toHaveBeenCalled();

    // And the read that set it is what clears it — a flag a screen could pin at
    // `true` is a spinner that never stops, so the write really is inert rather
    // than merely overwritten a moment later.
    release();
    await h1.advance();
    expect(h1.list.loading.value).toBe(false);
    h1.mounted();
  });
});
