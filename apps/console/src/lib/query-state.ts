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

import type { PageEnvelope } from "@ecoma-io/llm-gateway-console-api-client";

import type { PageLink } from "@/components/data-table";

/**
 * The three fields every paged read returns, narrowed to the two the pager is
 * allowed to read.
 *
 * `PageEnvelope` is the CONTRACT's own type, imported rather than restated. A
 * hand-written mirror of it here would be a second place a contract change had
 * to be remembered: the cast in `lib/paged-list.ts` casts a generated page to
 * this, so a mirror that drifted from the envelope would not fail anywhere — it
 * would be satisfied by any object with two of the right fields. Taking the
 * generated type and narrowing it to the two fields the pager reads keeps the
 * drift at zero, and `has_more` stays the contract's own description of itself.
 */
export type PagedRead = Pick<PageEnvelope, "next_cursor" | "has_more">;
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
  /**
   * The address-bar key this list's cursor rides in.
   *
   * `after` by default, and named rather than assumed because ONE ROUTE CAN CARRY
   * MORE THAN ONE LIST. The accounting screen holds a bucket list and a ledger,
   * and both want a cursor; with a single route-global `after` the pager on the
   * bucket list writes its cursor, and the ledger — watching the same route —
   * reads it and sends it against its own operation. That is a cursor the server
   * issued for a different collection, which the contract answers with `400
   * invalid_request`, which the console answers by dropping the cursor, which
   * loses the reader's place on a list they were not even reading.
   *
   * Two lists on one route therefore have to disagree here, and a list that
   * does not is silently sharing a cursor with whichever list is beside it.
   */
  readonly cursor?: string;
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
  vocabulary: Readonly<Record<string, readonly string[]>> = {},
): readonly PageLink[] {
  if (!page) return [];

  const links: PageLink[] = [];
  const after = readParam(current, cursorKey(shape));
  if (after !== undefined) {
    links.push({ label: "First", to: routeTo(undefined, current, shape, vocabulary) });
  }
  if (page.has_more) {
    links.push({
      label: "Next",
      to: routeTo(page.next_cursor, current, shape, vocabulary),
      current: after === undefined,
    });
  }
  return links;
}

/** The address-bar key a list's cursor rides in; see `QueryShape.cursor`. */
function cursorKey(shape: QueryShape): string {
  return shape.cursor ?? "after";
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
  vocabulary: Readonly<Record<string, readonly string[]>> = {},
): string {
  const query: LocationQueryRaw = {};
  for (const key of shape.filters) {
    // Validated on the way OUT as well as on the way in. A pager link is the
    // one string a reader copies and shares, and a link carrying a value the
    // contract does not admit is a link that hands the next person a control
    // showing nothing selected.
    const value = filterValue(current, key, vocabulary);
    if (value !== undefined) query[key] = value;
  }
  const key = cursorKey(shape);
  if (cursor !== undefined) query[key] = cursor;
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
/**
 * The current value of one filter, or `undefined` when it is unset or is not a
 * value the contract admits.
 *
 * Validated, not merely read, and the same rule `listQuery` applies to the
 * request. Without it a hand-edited `?status=not-a-status` reached a segmented
 * control as a bound value that matched no segment: the request went out
 * unfiltered (the vocabulary check below caught it there) while the control
 * showed NOTHING selected. A filter the console cannot render is not a filter,
 * and a control in that state misreports the request it is about to make.
 */
export function filterValue(
  query: Readonly<LocationQuery>,
  key: string,
  vocabulary: Readonly<Record<string, readonly string[]>>,
): string | undefined {
  const value = readParam(query, key);
  if (value === undefined) return undefined;
  const allowed = vocabulary[key];
  // A key with no vocabulary is one a screen manages itself — the accounting
  // screen's `bucket` — so it is passed through. Only a key that DECLARED a
  // closed set can be found outside it.
  if (allowed === undefined) return value;
  return allowed.includes(value) ? value : undefined;
}

export function useQueryState(
  shape: QueryShape,
  vocabulary: Readonly<Record<string, readonly string[]>> = {},
) {
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

  /** The current value of one filter, validated against the vocabulary. */
  function filter(key: string): string | undefined {
    return filterValue(query.value, key, vocabulary);
  }

  /** The opaque cursor for this position, sent back verbatim, from this list's own key. */
  const after = computed(() => readParam(query.value, cursorKey(shape)));

  /**
   * Change one filter, keeping the others and dropping the cursor.
   *
   * The value is written as-is when it is a real one and REMOVED when it is the
   * empty string, so a filter control that means "no filter" produces the URL
   * a shared link would have rather than `?state=`.
   *
   * `carryOver` is what keeps a filter change from destroying the screen around
   * it: it holds every query key this list does NOT own, so changing the
   * ledger's kind on the accounting screen leaves the bucket the reader picked
   * alone. The keys a list does not own are still someone else's, and a filter
   * control that rebuilt the query from its own shape alone would silently
   * reset a neighbouring table.
   */
  function setFilter(key: string, value: string, carryOver: Readonly<LocationQueryRaw> = {}): void {
    const next: LocationQueryRaw = { ...carryOver };
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
   *
   * ONLY the cursor is dropped. `carryOver` carries every key this list does not
   * own, and an earlier version rebuilt the query from `shape.filters` alone: on
   * the accounting screen, where `bucket` selects which table is shown rather
   * than filtering one, a `cursor_expired` on the LEDGER navigated to a URL
   * with no `bucket` in it — the selected bucket gone, the ledger section
   * replaced by "choose a bucket", and a `replace` so Back could not undo it.
   * The failure was on one list and the damage was to the screen.
   */
  function restartWithoutCursor(carryOver: Readonly<LocationQueryRaw> = {}): void {
    const next: LocationQueryRaw = { ...carryOver };
    for (const name of shape.filters) {
      const existing = filter(name);
      if (existing !== undefined) next[name] = existing;
    }
    delete next[cursorKey(shape)];
    navigate(next, true);
  }

  /**
   * The query keys this list does NOT own, for a screen that hosts more than
   * one list on one route.
   *
   * A key is this list's if it is one of the filters the list declared or the
   * cursor it reads; everything else in the address bar belongs to somebody
   * else and is carried through a navigation untouched. The accounting screen
   * is the case that needs it: the bucket list declares no filters at all, so
   * without this every action on the ledger would have rebuilt the URL from
   * `kind` alone and dropped the reader's selected bucket.
   */
  function carryOver(): LocationQueryRaw {
    const mine = new Set([...shape.filters, cursorKey(shape)]);
    const others: LocationQueryRaw = {};
    for (const [key, value] of Object.entries(query.value)) {
      if (mine.has(key)) continue;
      // An array value is a repeated key; a list never declares one, and a
      // repeated parameter cannot be handed back as a single string without
      // changing the URL the reader is looking at.
      if (typeof value === "string") others[key] = value;
    }
    return others;
  }

  return { query, filter, after, setFilter, navigate, restartWithoutCursor, carryOver };
}
