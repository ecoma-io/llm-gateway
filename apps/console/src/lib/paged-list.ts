// The one way a screen reads a paged list, and the only module in the console
// that knows paging and the URL are the same thing (ADR 0012 §4).
//
// A screen's whole paging story is three declarations — the operation, the
// filters the route may carry, and the columns — and everything else about
// getting from a query to a table is here. That is the point. Nine lists across
// six screens, each with a cursor the console must not interpret, a filter that
// invalidates that cursor, a stale answer that must not overwrite a fresh one,
// and a `400` the console can answer itself: the failure mode for that is nine
// near-identical `watch` blocks that drift apart on the ninth.
//
// Three rules this module holds, each of which a screen would otherwise have to
// remember on its own:
//
//   1. **The query is the state, and it is the ONLY state.** There is no page
//      counter, no cursor cache and no store. A reload, a Back, a shared link
//      and a filter change all land on the same request by construction, because
//      there is nothing else for them to land on.
//   2. **The cursor is sent back verbatim and never derived.** `has_more` is the
//      server's answer and `next_cursor` is the server's string; the module does
//      not decode either, compare them, or manufacture a position. It cannot,
//      which is why the pager has a First and a Next and no numbers.
//   3. **A stale answer never lands.** The route can change twice before the
//      first read returns — a fast filter click then a fast Next — and a module
//      without a generation guard renders whichever answers last, which is not
//      the one the reader asked for.
import { computed, onMounted, watch } from "vue";

import { useResource } from "@/lib/resource";
import {
  listQuery,
  pagerFor,
  type PagedRead,
  type QueryShape,
  useQueryState,
} from "@/lib/query-state";
import type { ApiResult } from "@/lib/api";

/**
 * How one screen declares a paged list.
 *
 * `read` is built from the operation's own generated type, so a screen cannot
 * hand this module a query the operation does not accept — which is the drift
 * that arrives through the front door as a `400` the type said was fine.
 * `TFilters` is inferred from it and is what the filter vocabulary is keyed
 * over, so the three declarations cannot fall out of step with one another.
 *
 * `query` is built from the route by `listQuery`, so it is already narrowed to
 * the contract's vocabulary; `read` receives it and the opaque cursor and is
 * handed to `lib/api`, which is the only place a request is built.
 */
export interface ListRead<TPage extends PagedRead, TQuery> {
  /** The screen's own call into the seam, taking the operation's query shape. */
  readonly read: (query: TQuery) => Promise<ApiResult<TPage>>;
  /** The filter keys this route may carry, in address-bar order. The cursor is added by this module. */
  readonly shape: QueryShape;
  /**
   * The closed vocabulary each filter key may hold, read from the contract.
   * A value outside it reads as "no filter" rather than becoming a `400` on a
   * link the operator was handed — the URL is user input.
   */
  readonly vocabulary: Readonly<Record<string, readonly string[]>>;
  /**
   * Whether this list's read may go out at all.
   *
   * A read with a PATH parameter can only be made once that parameter exists.
   * The accounting screen's ledger takes a bucket, and before the reader has
   * chosen one there is no bucket to put in the path — so the list must not ask,
   * or the console sends `{"funding_bucket_id": ""}` and the server answers for
   * a bucket the reader never named. A `v-if` around the table is NOT the same
   * thing: it hides the result and still fires the request, which is how a
   * screen ends up rendering "choose a bucket" over a read that failed for a
   * bucket that does not exist.
   *
   * A function rather than a boolean so the gate is re-read on every reload
   * decision, not captured once at setup. Defaults to always-on, so a list with
   * no precondition says nothing about having one.
   */
  readonly enabled?: () => boolean;
}

/**
 * The `TPage` of a list, with `items` given the screen's own row type.
 *
 * The generated pages are `PageEnvelope & { items: T[] }`, so the row type is
 * already in `TPage` and the only thing to add here is the `PagedRead` bound
 * `useResource` needs — which is why this is a conditional rather than a
 * parameter the caller could get wrong.
 */
/**
 * What a screen actually holds, and where its rows are typed.
 *
 * A generated page is `PageEnvelope & { items: T[] }`; the rows are an element
 * of that array. Naming the page type once here means a screen's `rows` is
 * `readonly FundingBucket[]` rather than `readonly unknown[]` — which is why
 * the cell templates can write `row.balances.settled` and index a presentation
 * map with `row.status` with no cast at all. `<DataTable>` still declares its
 * rows as `unknown` (its own header explains why that is not fixable from the
 * component side), and the bound generic narrows nothing there — but the SCREEN
 * reads `rows.value`, and that is where the type lives.
 */
type ListPage<TPage, TRow> = TPage & PagedRead & { readonly items: readonly TRow[] };

/**
 * One screen's paged list: the data, the failure, the pager and the filter
 * control's two ends.
 *
 * The returned object is the entire contract between a list screen and its
 * operation, and a screen cannot reach past it — there is no second way to
 * build a pager, and the module below is the only caller of `pagerFor`.
 */
export function usePagedList<TPage extends PagedRead, TQuery>(
  declaration: ListRead<TPage, TQuery>,
) {
  type TRow = TPage extends { readonly items: readonly (infer TItem)[] } ? TItem : never;
  const { query, filter, after, setFilter, restartWithoutCursor, carryOver } = useQueryState(
    declaration.shape,
    declaration.vocabulary,
  );

  const resource = useResource<TPage>(
    () => {
      const filters = listQuery<Record<string, string>>(query.value, declaration.vocabulary);
      return declaration.read({
        ...(filters as TQuery),
        ...(after.value === undefined ? {} : ({ after: after.value } as unknown as TQuery)),
      });
    },
    // The console's own answer to a cursor the server will not place: drop it
    // and read the first page. `restartWithoutCursor` is a REPLACE rather than a
    // push, so a Back that returned to a position the server refuses is a loop
    // the reader cannot escape — and it is handed the keys this list does not
    // own, so dropping one list's cursor cannot take a neighbouring list's
    // selection with it. Wiring it here is what makes that true on every list
    // instead of on the one that remembered to.
    { onCursorLost: () => restartWithoutCursor(carryOver()) },
  );

  /**
   * The page, narrowed to the two fields the pager is allowed to read.
   *
   * The narrowing is what stops a screen from treating a paged response as the
   * whole collection: `pagerFor` cannot see `items`, and a screen that wants
   * rows reads `items` off the page itself while the pager is built from these
   * two fields alone.
   */
  const page = computed<PagedRead | undefined>(() => resource.data.value);

  const rows = computed<readonly TRow[]>(() => resource.data.value?.items ?? []);

  const pages = computed(() =>
    pagerFor(page.value, query.value, declaration.shape, declaration.vocabulary),
  );

  /**
   * The one place a read is issued from, so the gate cannot be forgotten on one
   * of the three callers. `resource.run` is not exposed raw; a screen gets
   * `run`, which asks this first.
   */
  function reload(): void {
    if (declaration.enabled?.() === false) return;
    void resource.run();
  }

  /**
   * Reload whenever the POSITION changes — the filters and the cursor, which is
   * the whole of `query`.
   *
   * The read's own `data` is deliberately NOT in the watch source, and that is
   * not an oversight: a successful run ASSIGNS a freshly parsed object to
   * `data`, so observing it makes every load re-trigger the load that produced
   * it. That is a self-sustaining loop rather than a double fetch — measured at
   * 61 reads in 200 ms from one `mount()`, still climbing, terminating only
   * when the test's own circuit breaker threw. The reload this watcher wants
   * has exactly one cause, and the cause is the position, which is what is
   * observed.
   *
   * `flush: "sync"` is deliberate: the watcher must not fire while a previous
   * component's teardown is still running, or a route change that both ends one
   * list and starts another leaves the old one reading on.
   */
  watch(
    () => query.value,
    () => {
      reload();
    },
    { flush: "sync" },
  );

  // The gate is watched as well as consulted. A screen whose precondition is
  // satisfied by a click — the reader choosing a bucket — has nothing to
  // navigate, so nothing in `query` changes and the position watcher above
  // would never fire. Without this the first read of the ledger happens only
  // when the reader changes a filter.
  watch(
    () => declaration.enabled?.() ?? true,
    (allowed) => {
      if (allowed) reload();
    },
    { flush: "sync" },
  );

  onMounted(reload);

  return {
    data: computed<ListPage<TPage, TRow> | undefined>(
      () => resource.data.value as ListPage<TPage, TRow> | undefined,
    ),
    failure: resource.failure,
    loading: resource.loading,
    loaded: resource.loaded,
    rows,
    pages,
    filter,
    setFilter,
    /**
     * Reload, through the same gate the watchers use. A Retry button calling
     * this cannot fire a read the screen has already decided is not allowed to
     * make, which a raw `resource.run` would.
     */
    run: reload,
  };
}
