// The four states every screen's data is in, and the decisions between them.
//
// The claims worth testing here are the ones a screen's own spec cannot see. A
// page's test mounts it with one answer and checks what it rendered; this one
// drives the transitions, and every transition is a decision the file's header
// argues for — the generation guard, the kept data on a retryable failure, the
// dropped data on a dead end, and the cursor-lost hook. Each is a claim about
// what happens when two things happen at once or in a particular order, which
// is precisely what a single-mount test renders as nothing.
//
// The matrix is read rather than restated: the recovery column that decides
// "keep the data or drop it" is `CONSOLE_BEHAVIOUR`, and the loops below assert
// against the codes the console can produce rather than a list copied here. A
// code added to the contract with a new recovery moves those assertions
// instead of leaving them asserting a row that no longer exists.
import { defineComponent, h, nextTick } from "vue";
import { mount } from "@vue/test-utils";
import { describe, expect, it, vi } from "vitest";

import { useResource } from "./resource";
import type { ApiResult, Failure } from "@/lib/api";
import { CONSOLE_BEHAVIOUR, type ApiErrorCode } from "@/lib/failure-matrix";

/** A failure of the given contract code, built the way `lib/api` builds one. */
function apiFailure(code: ApiErrorCode): Extract<Failure, { kind: "api" }> {
  return {
    kind: "api",
    unauthenticated: code === "unauthenticated",
    envelope: { error: { code, message: "the server's own sentence" }, request_id: "req-test" },
  };
}

/** A transport failure: something that never became a contract body at all. */
function transportFailure(): Extract<Failure, { kind: "transport" }> {
  return { kind: "transport", error: new TypeError("fetch failed") };
}

function ok<T>(data: T): ApiResult<T> {
  return { ok: true, data };
}

function refused<T>(code: ApiErrorCode): ApiResult<T> {
  return { ok: false, failure: apiFailure(code) };
}

/** The codes whose matrix row keeps the data, and the codes whose row drops it. */
const KEEPS = Object.entries(CONSOLE_BEHAVIOUR)
  .filter(([, behaviour]) => behaviour.recovery !== "none")
  .map(([code]) => code as ApiErrorCode);
const DROPS = Object.entries(CONSOLE_BEHAVIOUR)
  .filter(([, behaviour]) => behaviour.recovery === "none")
  .map(([code]) => code as ApiErrorCode);

/**
 * A loader that answers the first call with `first` and every later one with
 * `rest` — the shape a "the screen loaded, then the read failed" test needs,
 * written once so each assertion below reads as the state it is about.
 */
function answering(first: ApiResult<string>, rest: ApiResult<string>) {
  let seen = false;
  return async (): Promise<ApiResult<string>> => {
    if (seen) return rest;
    seen = true;
    return first;
  };
}

/**
 * A loader that parks on a promise per call, so a test decides the ORDER two
 * reads settle in. This is the only honest way to test the generation guard: a
 * loader that resolves immediately settles in the order it was called, which is
 * the order the guard exists to make irrelevant.
 */
function parking() {
  const calls: { value: string; settle: () => void }[] = [];
  const load = () =>
    new Promise<ApiResult<string>>((resolve) => {
      const call = {
        value: "",
        settle: () => resolve(ok(call.value)),
      };
      calls.push(call);
    });
  return { load, calls };
}

/**
 * Mount a component that owns one resource, so the composable runs inside a
 * real effect scope. `run` is a promise, so a test that did not await would be
 * asserting on a state no reader ever saw.
 */
function harness<T>(load: () => Promise<ApiResult<T>>, onCursorLost?: () => void) {
  const resource = useResource<T>(load, onCursorLost ? { onCursorLost } : {});
  const wrapper = mount(
    defineComponent({
      setup: () => () => h("div", String(resource.loaded.value)),
    }),
  );
  return { resource, wrapper };
}

describe("a resource's four states", () => {
  it("has not loaded, and holds nothing, before anything has answered", () => {
    const { resource } = harness<string>(() => Promise.resolve(ok("first")));

    // Two separate facts — "nothing has arrived" and "a read is in flight" —
    // and a screen that collapsed them into one boolean would say "loading"
    // over a screen that is simply empty, forever.
    expect(resource.data.value).toBeUndefined();
    expect(resource.loaded.value).toBe(false);
    expect(resource.failure.value).toBeUndefined();
    expect(resource.loading.value).toBe(false);
    // Nothing has been asked, so nothing has been refused. `settled` is what
    // keeps those two apart.
    expect(resource.settled.value).toBe(false);
  });

  it("is loading, and not settled, while the read is in flight", async () => {
    const { resource } = harness<string>(async () => {
      await Promise.resolve();
      return ok("late");
    });

    const inFlight = resource.run();
    expect(resource.loading.value).toBe(true);
    expect(resource.loaded.value).toBe(false);
    expect(resource.settled.value).toBe(false);

    await inFlight;
    expect(resource.loading.value).toBe(false);
    expect(resource.loaded.value).toBe(true);
    expect(resource.settled.value).toBe(false);
  });

  it("is loaded with data, and not settled, once the read answers", async () => {
    const { resource } = harness<string>(() => Promise.resolve(ok("payload")));
    await resource.run();

    // `data` and `settled` are the two a screen branches on: one is "here is
    // the list", the other is "here is why there is not one", and a screen that
    // only had the first would have nothing to render for a failure.
    expect(resource.data.value).toBe("payload");
    expect(resource.settled.value).toBe(false);
  });

  it("holds an empty collection as an answer, never as a failure", async () => {
    const { resource } = harness<readonly string[]>(() => Promise.resolve(ok([])));
    await resource.run();

    // An account with no users is an answer, not an error, and a screen that
    // reached for the empty branch on `failure` alone would be one branch too
    // few for the state the header names.
    expect(resource.data.value).toEqual([]);
    expect(resource.failure.value).toBeUndefined();
    expect(resource.settled.value).toBe(false);
  });

  it("is settled, and holds no data, on a failure with no recovery", async () => {
    const { resource } = harness<string>(() => Promise.resolve(refused("internal")));
    await resource.run();

    expect(resource.settled.value).toBe(true);
    // The screen renders a failure view, not a code, so the assertion is on the
    // pair the matrix joins rather than on the code alone.
    const standing = resource.failure.value;
    expect(standing?.kind).toBe("api");
    expect(standing?.kind === "api" ? standing.envelope.error.code : undefined).toBe("internal");
    expect(resource.data.value).toBeUndefined();
  });

  it("clears the failure when a later read answers", async () => {
    const { resource } = harness<string>(answering(refused("internal"), ok("recovered")));

    await resource.run();
    expect(resource.settled.value).toBe(true);

    await resource.run();
    // The failure stands only until the read that replaces it, or a screen
    // would keep a "something went wrong on our side" banner over data it has
    // since fetched successfully.
    expect(resource.failure.value).toBeUndefined();
    expect(resource.data.value).toBe("recovered");
    expect(resource.settled.value).toBe(false);
  });
});

describe("what a failure does to the data already on screen", () => {
  it("keeps the last good data for every code the matrix calls retryable", async () => {
    expect(KEEPS).toContain("upstream_unavailable");
    expect(KEEPS).toContain("cursor_expired");

    for (const code of KEEPS) {
      const { resource } = harness<string>(answering(ok("on screen"), refused(code)));

      await resource.run();
      expect(resource.data.value, code).toBe("on screen");

      await resource.run();
      // Still there. An upstream that is briefly unreachable has not made the
      // rendered rows wrong, and blanking a dashboard to say "try again" is how
      // an operator learns to ignore the banner.
      expect(resource.data.value, code).toBe("on screen");
      expect(resource.failure.value, code).toBeDefined();
    }
  });

  it("drops the data for a dead end, so nothing is left looking current", async () => {
    expect(DROPS).toContain("internal");

    for (const code of DROPS) {
      const { resource } = harness<string>(answering(ok("stale"), refused(code)));

      await resource.run();
      await resource.run();

      // A 500 offers no retry, so the operator's next move is to quote a
      // request id — and a screen showing old figures beside that banner is
      // making a claim the server has already contradicted.
      expect(resource.data.value, code).toBeUndefined();
      expect(resource.failure.value, code).toBeDefined();
    }
  });

  it("keeps the data on a transport failure, which is a retry with no code", async () => {
    const { resource } = harness<string>(
      answering(ok("loaded"), { ok: false, failure: transportFailure() }),
    );

    await resource.run();
    await resource.run();

    // The transport arm names no code, so `behaviourFor` cannot be consulted
    // for it — the composable decides, and what it decides is that a body which
    // never arrived has not falsified what did.
    expect(resource.data.value).toBe("loaded");
    expect(resource.failure.value?.kind).toBe("transport");
  });
});

describe("a slow answer never overwrites a fast one", () => {
  it("renders the answer the reader asked for last, not the one that settled last", async () => {
    // The bug this is here for is reachable by a double click on Next: two
    // reads go out, and without a generation the second to ANSWER wins — so the
    // reader sees page three's rows under page two's cursor, which looks right
    // and is not. The newer read is settled FIRST on purpose, because that is
    // the order the guard has to survive.
    const { load, calls } = parking();
    const { resource } = harness<string>(load);

    const first = resource.run();
    const firstCall = calls[0]!;
    firstCall.value = "page one";
    const second = resource.run();
    const secondCall = calls[1]!;
    secondCall.value = "page two";

    // The reader asked for page two, and page two answers first.
    secondCall.settle();
    await second;
    await nextTick();
    expect(resource.data.value).toBe("page two");

    // Page one lands late. It must be ignored entirely.
    firstCall.settle();
    await first;
    await nextTick();
    expect(resource.data.value).toBe("page two");
  });

  it("leaves the state of the newer read alone when the older one lands", async () => {
    const { load, calls } = parking();
    const { resource } = harness<string>(load);

    const first = resource.run();
    const second = resource.run();
    calls[1]!.value = "second";
    calls[1]!.settle();
    await second;
    expect(resource.loading.value).toBe(false);
    expect(resource.loaded.value).toBe(true);

    // The stale call finishing must not mark the screen idle, must not unset
    // `loaded`, and must not put its own rows on screen. Each of those three is
    // a separate way for a superseded read to lie.
    calls[0]!.value = "first";
    calls[0]!.settle();
    await first;
    expect(resource.loading.value).toBe(false);
    expect(resource.loaded.value).toBe(true);
    expect(resource.data.value).toBe("second");
  });

  it("gives up the whole screen when a superseded read is the last one in flight", async () => {
    // The reason the guard is `return`-and-not-a-flag, and the one state the
    // header promises cannot be reached. A screen's watcher fires once for a
    // route change; if a read is somehow still outstanding when the component
    // unmounts, the late answer is the last thing to touch the resource. It
    // must not set `loaded` on a component nobody is looking at, and it must
    // not clear `loading` for a newer read that is genuinely in flight.
    const { load, calls } = parking();
    const { resource, wrapper } = harness<string>(load);

    const first = resource.run();
    const second = resource.run();
    calls[1]!.value = "second";
    calls[1]!.settle();
    await second;

    // The screen goes away with the first read still outstanding.
    wrapper.unmount();
    calls[0]!.value = "first";
    calls[0]!.settle();
    await first;

    // Nothing moved: the stale call took the early return, so `data` is still
    // the newer answer and `loading` still reflects the newer call.
    expect(resource.data.value).toBe("second");
    expect(resource.loaded.value).toBe(true);
  });
});

describe("a cursor the server will not place", () => {
  it("asks the screen to restart at the first page for exactly the two cursor codes", async () => {
    expect(CODES).toContain("cursor_expired");
    expect(CODES).toContain("invalid_request");

    for (const code of ["cursor_expired", "invalid_request"] as const) {
      const onCursorLost = vi.fn();
      const { resource } = harness<string>(() => Promise.resolve(refused(code)), onCursorLost);
      await resource.run();

      // The contract documents both as a cursor that cannot be placed, and the
      // console's answer is the same for both: drop it and start again. A flag
      // rather than a code keeps that vocabulary in the one table that owns it.
      expect(onCursorLost, code).toHaveBeenCalledTimes(1);
    }
  });

  it("does not ask for a restart on any other code", async () => {
    const others = CODES.filter((code) => code !== "cursor_expired" && code !== "invalid_request");

    for (const code of others) {
      const onCursorLost = vi.fn();
      const { resource } = harness<string>(() => Promise.resolve(refused(code)), onCursorLost);
      await resource.run();

      // A 500 is not a stale position. Restarting at the first page because a
      // server error arrived would throw away the reader's place for nothing.
      expect(onCursorLost, code).not.toHaveBeenCalled();
    }
  });

  it("treats a transport failure as a failure that is not a lost cursor", async () => {
    const onCursorLost = vi.fn();
    const { resource } = harness<string>(
      () => Promise.resolve({ ok: false, failure: transportFailure() }),
      onCursorLost,
    );
    await resource.run();

    expect(onCursorLost).not.toHaveBeenCalled();
  });
});

describe("the contract between a screen and a resource", () => {
  it("hands out a read-only loading flag, and the write does not take", async () => {
    // `loading` is a readonly view, so a screen cannot mark itself idle while a
    // read is outstanding — the defect the generation guard guards against, with
    // the type closing the other door. Vue reports the write as a warning rather
    // than an exception, so the assertion that matters is that the write DID NOT
    // TAKE: a composable that merely complained would still have been lied to.
    const { resource } = harness<string>(() => Promise.resolve(ok("x")));
    const inFlight = resource.run();
    const warn = vi.spyOn(console, "warn").mockImplementation(() => undefined);

    (resource.loading as unknown as { value: boolean }).value = false;
    expect(resource.loading.value).toBe(true);
    expect(warn).toHaveBeenCalled();

    warn.mockRestore();
    await inFlight;
    expect(resource.loading.value).toBe(false);
  });
});

/** Every code the contract declares, in the order the matrix declares them. */
const CODES = Object.keys(CONSOLE_BEHAVIOUR) as readonly ApiErrorCode[];
