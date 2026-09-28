// The column shape `<DataTable>` renders, and the attributes the console's
// architecture rules read. Both live here, apart from the component, for one
// reason: a boundary rule that reads a string the component also declares is a
// boundary that drifts the first time someone edits the component and forgets
// the rule. The selectors below are the ONLY place these attribute names are
// spelled, and the component takes its markup from this module's constants — so
// a rule that stops matching a table fails, loudly, rather than quietly
// governing nothing.
import { useId } from "vue";

/** Where a column's cells sit. Right is for figures compared down a column; it is an alignment and never the only channel. */
export type CellAlign = "left" | "right";

/** The grid roles a list screen can declare. The default is a plain table, which is what every list in the console is. */
export type GridRole = "table" | "grid";

export interface DataTableColumn {
  /** The key the column reads off a row, and the slot name a caller fills. */
  readonly key: string;
  /** The column header's text. Rendered as a `<th scope="col">`. */
  readonly label: string;
  readonly align?: CellAlign;
  /** Extra classes for this column's cells, merged after the alignment. */
  readonly class?: string;
}

/**
 * One page position, as the contract models it.
 *
 * `to` is a real route with a real href, so a page change is a history entry
 * and Back undoes it. The cursor inside the route is the server's own
 * `next_cursor`, sent back verbatim: it is OPAQUE, and nothing in the console
 * decodes it, compares it, orders it, or synthesises one from a page number. A
 * "page 3" link would have to mean a keyset position, and only the server can
 * turn a page number into a position.
 */
export interface PageLink {
  /** What the link says. A position, never a number. */
  readonly label: string;
  /** The route to navigate to. */
  readonly to: string;
  /** True for the position the reader is on. It becomes `aria-current="page"`. */
  readonly current?: boolean;
}

/**
 * What a re-render did to focus, reported upward so a screen — or a test —
 * can read it.
 *
 * This is the return-focus discipline Loom's `DataGrid` implements internally,
 * made observable: a re-render that unmounts the focused element does not move
 * focus anywhere, the browser drops it to the document, and a screen-reader
 * user is returned to the top of a page they had navigated down. Focus has to
 * land on the element's replacement, or on the nearest thing inside the table —
 * never on `document.body`.
 */
export interface FocusReturn {
  /** The selector the restoration looked for. */
  readonly selector: string;
  /** Whether something was found and focused. */
  readonly restored: boolean;
  /** Where focus ended up, as a tag name. `body` is the failure this exists to prevent. */
  readonly focused: string;
}

/**
 * The attributes every list table in the console carries.
 *
 * They exist for the architecture roster in `lib/arch/`, which reads the
 * declared layers off the DOM rather than off source text: a screen module
 * that renders its own `<table>` by hand cannot claim a layer it never
 * declared, and a declared layer with no table is a rule governing nothing.
 */
export const LAYER_ATTRIBUTE = "data-console-layer";

/** The layer a table belongs to. A screen module names its own. */
export type TableLayer = "module" | "page";

export function layerAttribute(layer: TableLayer): Record<string, string> {
  return { [LAYER_ATTRIBUTE]: layer };
}

/**
 * The row header, the cell a screen-reader user reads to know which row they
 * are in, and the anchor focus returns to after a re-render. `null` on a cell
 * that is not a header; the key itself is `${rowIndex}c${columnIndex}` and is
 * stable across a re-render of the same page.
 */
export const ROW_HEADER_ATTRIBUTE = "data-row-header";

/** Marks every cell so focus can find the same one again after the DOM is replaced. */
export const CELL_ATTRIBUTE = "data-cell";

/** The table element itself, the region focus is restored into. */
export const BODY_ATTRIBUTE = "data-table-body";

/** The pager's `<nav>`, so the roster can tell a pager from any other nav. */
export const PAGER_ATTRIBUTE = "data-table-pager";

/**
 * The accessible id of a table's caption, stable for the life of the
 * component instance. A caption that changed id on every re-render would
 * break the label it is meant to provide, and the label is the only thing that
 * names the table to a screen-reader user.
 */
export function captionId(): string {
  return `data-table-${useId()}`;
}
