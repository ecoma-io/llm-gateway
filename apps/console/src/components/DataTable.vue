<script setup lang="ts" generic="TRow extends object">
// The console's own table, because Loom's will not do this job (ADR 0012 §8.3).
// `Table` hands a bare `<table>` slot and `TableCell` renders `<td>` and never
// `<th>`, so a list built from it has no row header at all: the first column of
// every screen would be a cell with nothing naming it, and a screen-reader user
// moving down that column could not say which row they were in. This renders the
// three things that make a data table a table — a `<caption>` that names it, a
// `<thead>` whose headers are `<th scope="col">`, and a `<th scope="row">` for
// the first column of every row.
//
// It is also the only place in the console that renders a list, and every list
// here is paged, which is why the pager lives in the table rather than in each
// screen: the contract carries `items` / `next_cursor` / `has_more` and never a
// `total`, so there is no page count to render, no page number to name, and no
// position to compare one cursor against another. The pager is a `<nav>` of
// `RouterLink`s with `aria-current="page"` — real URLs, so Back undoes a page
// change, which Loom's `Pagination` (buttons, plus a `total` the contract does
// not have) cannot do.
//
// **The row type is a parameter, and a screen names it at the cell.** `rows` is
// `readonly TRow[]`, so the table reads a row's fields rather than stringifying
// an opaque value, and a cell slot says what it is handed:
// `<template #status="{ row }: { row: FundingBucket }">`. That annotation is
// the whole mechanism, and it is load-bearing in one specific way: Vue resolves a
// generic component's type parameters from a BOUND type argument or not at all,
// so `row` inside an unannotated slot is `any` and a screen's `row.status` —
// indexing a presentation map whose keys the contract fixes — silently accepts
// anything. The annotation is what turns that cell back into a checked
// expression.
//
// Two things it does not buy, both established by experiment rather than by
// argument. A bound argument (`<DataTable<Plan>`) is not available at all — the
// SFC parser reads the `<` as the start of a tag and reports "Invalid end tag" —
// and an inferred one is resolved by `extendsCheck` against the LAST component
// in the template, which is the pager's `RouterLink`, not against the slot's
// `row`. Both routes are therefore typed as `object` here, and a cell is what
// recovers the row's real shape. What remains checked without an annotation is
// the table's own `rows: readonly TRow[]`, and a screen never relies on that
// alone: `usePagedList<TPage>` in `lib/paged-list.ts` names the page type once
// and derives `rows` from it.
import { computed, nextTick, onBeforeUpdate, onUpdated, ref, useTemplateRef } from "vue";
import { RouterLink } from "vue-router";

import {
  BODY_ATTRIBUTE,
  CELL_ATTRIBUTE,
  PAGER_ATTRIBUTE,
  ROW_HEADER_ATTRIBUTE,
  captionId,
  type DataTableColumn,
  type FocusReturn,
  type GridRole,
  type PageLink,
  type TableLayer,
  layerAttribute,
} from "./data-table";

const props = withDefaults(
  defineProps<{
    /** The table's caption, rendered as a real `<caption>` rather than a heading above the table. */
    caption: string;
    /** The rows, in the server's order. The table never sorts them: a keyset page has no order to re-sort into. */
    rows: readonly TRow[];
    /** One entry per column, in reading order. The first is rendered as the row header of every row. */
    columns: readonly DataTableColumn[];
    /**
     * What is true of the data now. `empty` is a legitimate answer — an account
     * with no users is not an error — so it is a state the table renders rather
     * than a case every caller has to intercept.
     */
    state?: "ready" | "loading" | "empty";
    /** The sentence shown when there are no rows and nothing is in flight. */
    emptyMessage?: string;
    /**
     * The pager, as real links. Absent on a screen that renders one page, and
     * never a page count: see the file header.
     */
    pages?: readonly PageLink[];
    /** An accessible name for the pager's `<nav>`. Defaults to the caption. */
    pagesLabel?: string;
    /**
     * `table` (the default) is a plain data table. `grid` is for a list whose
     * first cell is a real control — an action, a copy button, a link — where
     * the reader moves between cells and the cells are focusable. Declaring
     * `grid` without making the cells focusable is a lie a screen reader will
     * act on, so it is opt-in and the keyboard wiring below is what it buys.
     */
    role?: GridRole;
    /** Which layer this table belongs to, read by the architecture roster. */
    layer?: TableLayer;
    /** Reported after every re-render that touched focus. */
    onFocusReturn?: (result: FocusReturn) => void;
  }>(),
  {
    state: "ready",
    emptyMessage: "There is nothing to show yet.",
    pages: undefined,
    pagesLabel: undefined,
    role: "table",
    layer: "module",
    onFocusReturn: undefined,
  },
);

const ALIGN_CLASS = { left: "text-left", right: "text-right" } as const;
const layer = computed(() => layerAttribute(props.role === "grid" ? "page" : props.layer));

// Bound from the shared constants rather than spelled out in the template, so
// the roster in `lib/arch/` and this markup cannot drift apart: the rule reads
// the name the component renders because it is the same name.
const bodyAttributes = { [BODY_ATTRIBUTE]: "" };
const pagerAttributes = { [PAGER_ATTRIBUTE]: "" };
const rowHeaderAttributes = { [ROW_HEADER_ATTRIBUTE]: "" };

const tableCaptionId = captionId();
const isEmpty = computed(() => props.rows.length === 0);
const body = useTemplateRef<HTMLElement>("body");

function cellKey(rowIndex: number, columnIndex: number): string {
  return `r${rowIndex}c${columnIndex}`;
}

/**
 * The first COLUMN of every row is that row's header, and every other cell is
 * an ordinary `td`. It is the column, not the first row, that makes a header:
 * a `th` on row 0 alone would name the table rather than the rows, and a table
 * whose rows have no headers is the state Loom's `Table` leaves every list in.
 */
function isRowHeader(columnIndex: number): boolean {
  return columnIndex === 0;
}

/**
 * A cell with no slot of its own renders the row's own field under that column
 * key, stringified. A screen that wants a status badge, a money figure or a
 * relative date fills a slot; a screen that wants the raw field does nothing
 * and gets the value.
 */
function fieldValue(row: TRow, key: string): string {
  const value = (row as Record<string, unknown>)[key];
  return value === undefined || value === null ? "" : String(value);
}

/**
 * The cells of a `role="grid"` body participate in the roving tab stop: one
 * cell is tabbable at a time and the arrow keys move between them.
 *
 * A plain `role="table"` does not get this. Tabbing through forty rows of a
 * data table to reach the pager is a defect, not a feature, and the arrow-key
 * model is only correct when the grid also owns focus — which is why it is
 * opt-in and tied to the role that promises it.
 */
const isInteractive = computed(() => props.role === "grid");
const activeCell = ref<string | null>(null);

function cellSelector(rowIndex: number, columnIndex: number): string {
  return `[${CELL_ATTRIBUTE}="${cellKey(rowIndex, columnIndex)}"]`;
}

function isActiveCell(rowIndex: number, columnIndex: number): boolean {
  return activeCell.value === cellKey(rowIndex, columnIndex);
}

function moveTo(rowIndex: number, columnIndex: number): void {
  if (!isInteractive.value || isEmpty.value) return;
  const lastRow = props.rows.length - 1;
  const lastColumn = props.columns.length - 1;
  const row = Math.min(Math.max(rowIndex, 0), lastRow);
  const column = Math.min(Math.max(columnIndex, 0), lastColumn);
  activeCell.value = cellKey(row, column);
  // Synchronously, not after a tick: a grid has already given the cell its
  // tab index in the render that produced the key, so the node to focus is in
  // the document now. Deferring it would let the keydown that triggered the
  // move finish with focus still on the cell the reader just left.
  body.value?.querySelector<HTMLElement>(cellSelector(row, column))?.focus();
}

function onKeydown(event: KeyboardEvent): void {
  const cell = (event.target as HTMLElement | null)?.closest(`[${CELL_ATTRIBUTE}]`);
  if (!cell) return;
  const key = cell.getAttribute(CELL_ATTRIBUTE) ?? "";
  const [rowText, columnText] = key.replace(/^r(\d+)c(\d+)$/, "$1 $2").split(" ");
  const row = Number(rowText);
  const column = Number(columnText);
  if (!Number.isInteger(row) || !Number.isInteger(column)) return;

  switch (event.key) {
    case "ArrowRight":
      moveTo(row, column + 1);
      break;
    case "ArrowLeft":
      moveTo(row, column - 1);
      break;
    case "ArrowDown":
      moveTo(row + 1, column);
      break;
    case "ArrowUp":
      moveTo(row - 1, column);
      break;
    case "Home":
      // Ctrl+Home is the first cell of the table, Home alone is the first
      // cell of this row — the distinction Loom's DataGrid draws, and the one
      // a spreadsheet user expects.
      moveTo(event.ctrlKey || event.metaKey ? 0 : row, 0);
      break;
    case "End":
      moveTo(
        event.ctrlKey || event.metaKey ? props.rows.length - 1 : row,
        props.columns.length - 1,
      );
      break;
    default:
      return;
  }
  event.preventDefault();
}

/**
 * What held focus before this update, read from the pre-update DOM.
 *
 * A cell is identified by its `data-cell` key rather than by its text or by
 * its index alone, so the element found after the update is the SAME cell and
 * not merely the nth one. `onBeforeUpdate` is the only hook that still sees
 * the old DOM, which is why the read lives here and not in `onUpdated`.
 */
let watched: { tag: string; cell: string | null } | null = null;

function rememberFocus(): void {
  const active = document.activeElement;
  if (!active || active === document.body || !body.value?.contains(active)) {
    watched = null;
    return;
  }
  watched = {
    tag: active.tagName.toLowerCase(),
    cell: active.closest(`[${CELL_ATTRIBUTE}]`)?.getAttribute(CELL_ATTRIBUTE) ?? null,
  };
}

/**
 * The same discipline Loom's `DataGrid` applies: after a re-render, if the
 * focused element is gone, focus its replacement.
 *
 * The search is deliberately narrow — inside this table's body, never the
 * document. Falling back to `document.body` is the failure being prevented, so
 * a lookup that comes up empty leaves focus where the browser put it and
 * REPORTS that it did, rather than claiming a restore it did not perform.
 */
function restoreFocus(): void {
  if (!watched) return;
  const { tag, cell } = watched;
  watched = null;

  // The cell itself may BE the focused element — the row header is a `th` and
  // the cell holding a link is a `td` — so the cell is checked before its
  // descendants. Looking only inside would miss the commonest case and fall
  // through to the "nearest thing in the table" branch, landing the reader on
  // the first cell of the first row instead of the one they were reading.
  const cellElement = cell
    ? body.value?.querySelector<HTMLElement>(`[${CELL_ATTRIBUTE}='${cell}']`)
    : null;
  const itself = cellElement?.tagName.toLowerCase() === tag ? cellElement : undefined;
  const within = cellElement?.querySelector<HTMLElement>(tag);
  const replacement = itself ?? within;
  if (replacement) {
    replacement.focus();
    report(cell ? `${tag} in ${cell}` : tag, true);
    return;
  }

  // The cell the reader was in no longer exists — a page change, a filter, a
  // shorter page. The nearest focusable thing inside the table is the honest
  // landing spot: the reader is still where they were, on the same list.
  const fallback = body.value?.querySelector<HTMLElement>(
    `[${CELL_ATTRIBUTE}] a, [${CELL_ATTRIBUTE}] button, [${CELL_ATTRIBUTE}]`,
  );
  if (fallback) {
    fallback.focus();
    report(`${tag} in ${cell ?? "table"} (cell gone)`, true);
    return;
  }

  report(`${tag} in ${cell ?? "table"}`, false);
}

function report(selector: string, restored: boolean): void {
  props.onFocusReturn?.({
    selector,
    restored,
    focused: document.activeElement?.tagName.toLowerCase() ?? "none",
  });
}

onBeforeUpdate(() => {
  rememberFocus();
  // A grid's tab stop is state, not a consequence of focus: the roving stop
  // has to follow the reader's cell across a re-render or the grid silently
  // jumps its tab order back to the first cell.
  const active = document.activeElement as HTMLElement | null;
  if (active && body.value?.contains(active) && active.hasAttribute(CELL_ATTRIBUTE)) {
    activeCell.value = active.getAttribute(CELL_ATTRIBUTE);
  }
});

onUpdated(() => {
  // The DOM is already written by the time this runs, so the replacement
  // exists; the tick is kept because a caller whose cells resolve their own
  // content asynchronously will not have it yet, and an async cell is the
  // normal case for a money figure or a status badge.
  nextTick(restoreFocus);
});

function tabbable(rowIndex: number, columnIndex: number): boolean {
  if (!isInteractive.value) return false;
  if (activeCell.value === null || activeCell.value === undefined) {
    return rowIndex === 0 && columnIndex === 0;
  }
  return isActiveCell(rowIndex, columnIndex);
}
</script>

<template>
  <div class="flex flex-col gap-3">
    <table
      v-bind="layer"
      :aria-labelledby="tableCaptionId"
      :role="role"
      class="w-full border-collapse text-left text-sm [&_td]:border-b [&_td]:border-border [&_td]:px-3 [&_td]:py-2.5 [&_td]:align-top [&_td]:text-foreground [&_th]:border-b [&_th]:border-border-strong [&_th]:px-3 [&_th]:py-2.5 [&_th]:text-xs [&_th]:font-semibold [&_th]:text-muted-foreground [&_tbody_tr:last-child_td]:border-b-0"
    >
      <caption :id="tableCaptionId" class="pb-2 text-left text-sm text-muted-foreground">
        {{
          caption
        }}
      </caption>
      <thead>
        <tr>
          <th
            v-for="column in columns"
            :key="column.key"
            scope="col"
            :class="[ALIGN_CLASS[column.align ?? 'left'], column.class]"
          >
            {{ column.label }}
          </th>
        </tr>
      </thead>
      <tbody
        ref="body"
        v-bind="bodyAttributes"
        :aria-label="role === 'grid' ? caption : undefined"
        @keydown="onKeydown"
      >
        <tr v-if="isEmpty">
          <td :colspan="columns.length" class="px-3 py-6 text-muted-foreground">
            {{ state === "loading" ? "Loading…" : emptyMessage }}
          </td>
        </tr>
        <tr v-for="(row, rowIndex) in isEmpty ? [] : rows" :key="rowIndex">
          <component
            :is="isRowHeader(columnIndex) ? 'th' : 'td'"
            v-for="(column, columnIndex) in columns"
            :key="column.key"
            :data-cell="cellKey(rowIndex, columnIndex)"
            v-bind="isRowHeader(columnIndex) ? rowHeaderAttributes : undefined"
            :scope="isRowHeader(columnIndex) ? 'row' : undefined"
            :tabindex="tabbable(rowIndex, columnIndex) ? 0 : -1"
            :class="[ALIGN_CLASS[column.align ?? 'left'], column.class]"
          >
            <!--
              The cell's value: the caller's slot when it fills one, and the
              row's own `column.key` field when it does not. The fallback is
              why the columns a screen declares carry real field names, and it
              is an `v-if` rather than a `<slot>` fallback body because Vue
              compiles a slot's default content away the moment a component
              exposes any named slot at all — a fallback that renders on no
              path is worse than none, because it looks correct in review.
            -->
            <slot v-if="$slots[column.key]" :name="column.key" :row="row" :index="rowIndex" />
            <template v-else>{{ fieldValue(row, column.key) }}</template>
          </component>
        </tr>
      </tbody>
    </table>

    <nav
      v-if="pages && pages.length > 0"
      v-bind="pagerAttributes"
      :aria-label="pagesLabel ?? caption"
      class="flex flex-wrap items-center gap-2"
    >
      <RouterLink
        v-for="page in pages"
        :key="page.to"
        :to="page.to"
        :aria-current="page.current ? 'page' : undefined"
        :class="
          page.current
            ? 'rounded-md bg-muted px-3 py-1.5 text-sm font-medium text-foreground'
            : 'rounded-md px-3 py-1.5 text-sm text-foreground underline-offset-4 hover:underline focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-ring'
        "
      >
        {{ page.label }}
      </RouterLink>
    </nav>
  </div>
</template>
