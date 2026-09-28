// The one shape every screen's data is in, so that no screen invents a fourth.
//
// A screen has exactly four states and they are not a boolean pair: **loading**,
// **empty**, **ready with data**, and **failed**. A screen that renders its own
// combination of `loading` / `error` / `data` refs can reach a state the failure
// matrix has no row for — a blank table under a "try again" banner, or a
// "nothing to show" sentence over a screen that actually failed to load, and an
// empty list is a legitimate answer while a failure is not.
//
// The last good data is KEPT on a failure, deliberately. The matrix says a
// retryable code leaves the last good data on screen: an upstream that is
// briefly unreachable has not made the data already rendered wrong, and
// blanking a dashboard to say "try again" is how an operator learns to ignore
// the banner. `FailureView` decides the sentence; this composable decides that
// a `retry` keeps the data and a `none` does not — and a `none` failure is a
// dead end, so its data is dropped rather than left on screen looking current.
//
// A `cursor_expired` or `invalid_request` on a paged read is the one failure
// with a console-side answer beyond a retry: the cursor carried a filter
// fingerprint the request no longer makes, so the screen restarts at the first
// page through `onCursorLost`. The failure matrix's own sentence for those two
// names exactly that fix, and honouring it is why those rows say `retry` and
// not `none`.
import { computed, readonly, ref, shallowRef } from "vue";

import type { ApiResult, Failure } from "@/lib/api";
import { behaviourFor } from "@/lib/failure-matrix";

/**
 * One resource, its four states, and the three operations that move between
 * them. `loader` is the screen's own call to `lib/api`; this module never
 * fetches, so a test can drive any screen by handing it a function.
 */
export function useResource<T>(
  load: () => Promise<ApiResult<T>>,
  options: { readonly onCursorLost?: () => void } = {},
) {
  const data = shallowRef<T | undefined>(undefined);
  const failure = ref<Failure | undefined>(undefined);
  const loading = ref(false);
  const loaded = ref(false);

  /**
   * The generation the in-flight call belongs to, which is how a slow answer is
   * stopped from overwriting a fast one.
   *
   * The bug this closes is real and reachable in a console, not a theoretical
   * race: the Refresh button is live while a load is in flight, and a double
   * click on Next starts two reads. Whichever ANSWERS LAST then wins, so
   * double-clicking Next renders page three on top of page two — the user sees
   * the second page with the first page's rows, which is worse than seeing
   * nothing because it looks correct.
   *
   * `run` takes a generation number and a call writes only if its own is still
   * the current one, so the counter has to ADVANCE too — a double click is a
   * second generation, not a second call sharing the first's number. Every
   * call claims one, and every call is therefore both unstoppable once issued
   * and stoppable by any later one.
   *
   * There is deliberately no way to ask for a read that no later read can
   * supersede. An option that did would need a generation value no `++` could
   * ever produce, and the first version of this module had one: a `latest`
   * branch that took `Number.MAX_SAFE_INTEGER` without incrementing. It was
   * unreachable in the direction it promised — the counter was a small integer
   * and could never equal the sentinel, so a `latest` call discarded its own
   * answer and left `loading` stuck on forever. The test that tried to pin the
   * behaviour it was described as having is what proved it, and nothing ever
   * called it, so it is gone rather than repaired: a knob with no caller is
   * not an extension point, it is a second code path that only exists to be
   * wrong.
   */
  let generation = 0;

  async function run(): Promise<void> {
    const mine = ++generation;
    loading.value = true;
    try {
      const result = await load();
      // A stale call returns before ANY state is touched, `loaded` included. It
      // is the newer call that decides whether anything has loaded, and a
      // superseded one claiming otherwise would tell a screen that has nothing
      // to render that it is ready — which is the blank-table-under-a-spinner
      // state the header says this module exists to make impossible.
      if (mine !== generation) return;
      if (result.ok) {
        data.value = result.data;
        failure.value = undefined;
        return;
      }
      failure.value = result.failure;
      // A `none` recovery is a dead end — there is nothing the operator can do
      // but report it — so the data is dropped rather than left on screen
      // looking current. A `retry` one keeps it, which is the whole point of
      // offering a retry at all.
      if (!keepsLastData(result.failure)) data.value = undefined;
      if (isCursorLost(result.failure)) options.onCursorLost?.();
    } finally {
      // A stale call clears nothing: the newer one still in flight owns
      // `loading`, and letting a superseded call set it false is how a screen
      // ends up claiming it is idle while a read is outstanding.
      if (mine === generation) loading.value = false;
      // In the `finally` and not after it, so it is reached on the SUCCESS path
      // too: the `return` above leaves the block, and a `loaded` flag that is
      // skipped whenever a read succeeds is not a flag at all.
      if (mine === generation) loaded.value = true;
    }
  }

  return {
    data,
    failure,
    loading: readonly(loading),
    /**
     * Whether a read has been answered at all, whatever the answer was.
     *
     * `loaded` and `data` are different facts and a screen needs both: a
     * retryable failure keeps the data and still has not given the reader a
     * current answer, so a list that renders the kept rows next to a banner is
     * branching on `data` and a screen that says "nothing to show" is
     * branching on this.
     */
    loaded: readonly(loaded),
    /**
     * Whether a read is settled and has something to show: data, or a failure
     * the console renders. A screen that branches on this is branching between
     * "here is the list" and "here is why there is not one", and cannot land in
     * the third state the file header names — a blank table under a spinner,
     * or a "nothing to show" sentence over a read that failed.
     */
    settled: computed(() => loaded.value && failure.value !== undefined),
    run,
  };
}

/**
 * The recovery column, read straight from the one table that declares it rather
 * than re-derived here. `CONSOLE_BEHAVIOUR` is keyed exhaustively over the
 * generated union, so this lookup is total and a code added to the contract
 * without a row stops the module compiling — which is ADR 0012 §6's gate
 * holding at the point where a screen needs it.
 */
function keepsLastData(failure: Failure): boolean {
  if (failure.kind === "transport") return true;
  return behaviourFor(failure.envelope.error.code).recovery !== "none";
}

/**
 * The two codes the contract documents as a cursor that cannot be placed.
 * `cursor_expired` is a position past the retained history; `invalid_request`
 * on a paged read is a position whose filter fingerprint the request no longer
 * makes. Both are answered the same way by the console: drop the cursor and
 * start again at the first page.
 */
function isCursorLost(failure: Failure): boolean {
  if (failure.kind !== "api") return false;
  const code = failure.envelope.error.code;
  return code === "cursor_expired" || code === "invalid_request";
}
