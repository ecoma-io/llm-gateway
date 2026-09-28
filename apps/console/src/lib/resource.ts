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
import { computed, ref, shallowRef, type Ref, type ShallowRef } from "vue";

import type { ApiResult, Failure } from "@/lib/api";
import { behaviourFor } from "@/lib/failure-matrix";

/** What a screen shows right now. */
export interface ResourceState<T> {
  /** The last data that arrived, or `undefined` before the first success. */
  readonly data: ShallowRef<T | undefined>;
  /** The failure, or `undefined` while the last call succeeded. */
  readonly failure: Ref<Failure | undefined>;
  /** True while a call is in flight. */
  readonly loading: Ref<boolean>;
  /** True once a call has answered and no failure is standing. */
  readonly ready: Ref<boolean>;
  /** True when there is no data and no failure and nothing is in flight. */
  readonly empty: Ref<boolean>;
  /**
   * Whether the failure standing is one the console can answer itself by
   * dropping the cursor. Kept as a flag the screen reads rather than a code it
   * matches on, so the vocabulary of codes stays in the matrix.
   */
  readonly cursorLost: Ref<boolean>;
}

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
  const droppedByFailure = ref(false);

  async function run(): Promise<void> {
    loading.value = true;
    try {
      const result = await load();
      if (result.ok) {
        data.value = result.data;
        failure.value = undefined;
        droppedByFailure.value = false;
        return;
      }
      failure.value = result.failure;
      // A `none` recovery is a dead end — there is nothing the operator can do
      // but report it — so the data is dropped rather than left on screen
      // looking current. A `retry` one keeps it, which is the whole point of
      // offering a retry at all.
      const retains = keepsLastData(result.failure);
      droppedByFailure.value = !retains;
      if (!retains) data.value = undefined;
      if (isCursorLost(result.failure)) options.onCursorLost?.();
    } finally {
      loading.value = false;
      loaded.value = true;
    }
  }

  const state: ResourceState<T> = {
    data,
    failure,
    loading,
    ready: computed(() => loaded.value && failure.value === undefined),
    empty: computed(() => loaded.value && failure.value === undefined && data.value === undefined),
    cursorLost: computed(() => cursorLost.value),
  };

  const cursorLost = computed(() => failure.value !== undefined && isCursorLost(failure.value));

  return { ...state, run, cursorLost };
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
