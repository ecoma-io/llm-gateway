// The one place a screen reads and writes the address bar (ADR 0012 §4).
//
// Three rules live here, and each exists because breaking it is a defect a user
// would experience rather than one only a reviewer would notice:
//
//   1. **The cursor is opaque.** A screen never parses a cursor, never compares
//      one, never orders them and never synthesises one from a page number. It
//      puts the server's `next_cursor` into the query and puts back exactly what
//      the query held. The only thing derived from it is `has_more`, which the
//      server also sent.
//   2. **No sensitive data in a URL.** The account comes from the session and is
//      never a query parameter; a bucket id is a PATH segment, because the
//      ledger operation takes one and a bucket the session's account does not own
//      is a `404`, not a disclosure; and a credential never appears in either.
//      Only the keys a screen declares in `QueryShape.filters` are carried
//      through, so a parameter nobody asked for cannot ride along.
//   3. **No page numbers, no totals.** The contract carries `has_more` and not a
//      count, so a pager here has a "First" and a "Next" and no "of N" — and no
//      "Previous" either, because a keyset cursor names a position the console
//      cannot step back from: it would have to re-derive a cursor the server
//      never issued. Back is what undoes a page change, and that is the browser
//      button doing a job the console cannot do for itself.
//
// The route query is the ONLY cursor state. There is no store, so reload, Back
// and a shared link all land on the same page by construction rather than by a
// synchronisation that could be forgotten.
import { computed } from "vue";
import { useRoute, useRouter, type LocationQuery, type LocationQueryRaw } from "vue-router";

import type { PageLink } from "@/components/data-table";

/**
 * The three fields every paged read returns, named once.
 *
 * The contract's `PageEnvelope` is not re-exported by the API client package —
 * every concrete page type is `PageEnvelope & { items: … }` and the envelope
 * itself is an implementation detail of those intersections. This is the same
 * three fields, structurally, so a generated page satisfies it without a cast,
 * and a future contract change to the envelope stops a screen from type-checking
 * rather than being absorbed by a loose structural type here.
 */
export interface PagedRead {
  readonly next_cursor: string;
  readonly has_more: boolean;
}

/**
 * The query keys a screen is allowed to carry, and which of them are cursors.
 *
 * A screen declares the FILTER keys it reads; this module adds the cursor. A
 * filter change drops the cursor, because a cursor carries a filter fingerprint
 * and sending it against a changed filter is precisely the `400
 * invalid_request` the contract documents.
 */
export interface QueryShape {
  /** The filter keys, in the order they appear in the address bar. Any value not here is dropped. */
  readonly filters: readonly string[];
}

/** Reads one string query parameter, or `undefined` when it is absent or blank. */
function readParam(query: LocationQuery, key: string): string | undefined {
  const value = query[key];
  if (typeof value !== "string") return undefined;
  return value === "" ? undefined : value;
}

/**
 * The pager for a paged read, as links — never as buttons, and never numbered.
 *
 * `Next` exists only when `has_more` is true, because that field and not the
 * page's length is the only honest end-of-collection signal. `First` always
 * exists while the reader is anywhere but the start: it is a real route with no
 * cursor, so it is the one link that is a position the console can name.
 */
export function pagerFor(
  page: PagedRead | undefined,
  current: Readonly<LocationQuery>,
  shape: QueryShape,
): readonly PageLink[] {
  if (!page) return [];

  const links: PageLink[] = [];
  const after = readParam(current, "after");
  if (after !== undefined) {
    links.push({ label: "First", to: routeTo(undefined, current, shape) });
  }
  if (page.has_more) {
    links.push({
      label: "Next",
      to: routeTo(page.next_cursor, current, shape),
      current: after === undefined,
    });
  }
  return links;
}

/**
 * The route for one position, carrying the declared filters and one cursor.
 *
 * `cursor === undefined` means the FIRST page, and the key is dropped rather
 * than set to an empty string: an empty `?after=` is a cursor the server has to
 * reason about, and "the first page" is the honest way to say there is no
 * cursor.
 */
export function routeTo(
  cursor: string | undefined,
  current: Readonly<LocationQuery>,
  shape: QueryShape,
): string {
  const query: LocationQueryRaw = {};
  for (const key of shape.filters) {
    const value = readParam(current, key);
    if (value !== undefined) query[key] = value;
  }
  if (cursor !== undefined) query.after = cursor;
  const search = new URLSearchParams(query as Record<string, string>).toString();
  return search === "" ? "" : `?${search}`;
}

/**
 * The query a screen hands to its list operation, built from the address bar.
 *
 * Every filter is validated against a closed vocabulary before it is sent, so
 * a hand-edited `?state=not-a-state` becomes "no filter" rather than a `400` —
 * the URL is user input and the contract's 400 is a real response a visitor
 * should never be shown for a link they were handed.
 */
export function listQuery<TFilters>(
  current: Readonly<LocationQuery>,
  vocabulary: Readonly<Record<string, readonly string[]>>,
): TFilters {
  const query: Record<string, string> = {};
  for (const [key, allowed] of Object.entries(vocabulary)) {
    const value = readParam(current, key);
    if (value !== undefined && allowed.includes(value)) query[key] = value;
  }
  return query as TFilters;
}

/**
 * The filter reads and writes for one screen, plus the cursor pass-through.
 *
 * `setFilter` is a navigation, not an assignment: a filter change is a history
 * entry so Back undoes it, and it DROPS the cursor because the cursor the
 * reader is holding was issued against the filter they just left.
 */
export function useQueryState(shape: QueryShape) {
  const route = useRoute();
  const router = useRouter();

  const query = computed(() => route.query);

  /**
   * Move to a query. A filter change is a history entry so Back undoes it; the
   * restart that discards an unusable cursor is a REPLACE, because a Back that
   * returned to a position the server refuses is a loop the reader cannot
   * escape.
   */
  function navigate(next: Readonly<LocationQueryRaw>, replace = false): void {
    const target = { path: route.path, query: { ...next } };
    const navigation = replace ? router.replace(target) : router.push(target);
    // A duplicated navigation rejects: clicking a filter that is already set is
    // a legitimate act, and letting that rejection escape would surface as an
    // unhandled error for doing nothing.
    void navigation.catch(() => undefined);
  }

  /** The current value of one filter, or `undefined` when it is unset. */
  function filter(key: string): string | undefined {
    return readParam(query.value, key);
  }

  /** The opaque cursor for this position, sent back verbatim. */
  const after = computed(() => readParam(query.value, "after"));

  /**
   * Change one filter, keeping the others and dropping the cursor.
   *
   * The value is written as-is when it is a real one and REMOVED when it is the
   * empty string, so a filter control that means "no filter" produces the URL
   * a shared link would have rather than `?state=`.
   */
  function setFilter(key: string, value: string): void {
    const next: Record<string, string> = {};
    for (const name of shape.filters) {
      const existing = filter(name);
      if (existing !== undefined) next[name] = existing;
    }
    if (value === "") delete next[key];
    else next[key] = value;
    navigate(next);
  }

  /**
   * Clear a cursor whose filter fingerprint no longer matches the request.
   *
   * This is the `400 invalid_request` the contract documents, and the console's
   * answer is to drop the cursor and reload the first page — the position the
   * cursor named no longer exists under any filter the reader still has.
   */
  function restartWithoutCursor(): void {
    const next: Record<string, string> = {};
    for (const name of shape.filters) {
      const existing = filter(name);
      if (existing !== undefined) next[name] = existing;
    }
    navigate(next, true);
  }

  return { query, filter, after, setFilter, navigate, restartWithoutCursor };
}
