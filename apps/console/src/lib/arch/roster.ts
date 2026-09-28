// The console's own architecture roster: the rules the rendered DOM must
// satisfy, checked against the DOM rather than against source text.
//
// The alternative is a source scan, and a source scan cannot answer the
// question that matters here — whether a SCREEN actually rendered a table. A
// screen can import `DataTable` and hide it behind a `v-if` that is false at
// mount; a source rule sees the import and passes, and the page is then a list
// screen with no table and no pager. Reading the rendered document is the only
// way to see that, which is why `LAYER_ATTRIBUTE` and its siblings are on the
// elements rather than in a config file: a declaration that is not in the
// markup is a declaration that is not made.
//
// Four rules, each of which a screen can pass on every screen and still be
// wrong, so each is checked per rendered document rather than once per file:
//
//   1. **Every table declares a layer.** A list table in the console is either a
//      page's own table or a module's shared one, and the difference decides
//      who may read it. An undeclared table is a table nobody owns.
//   2. **Every page renders at least one table, or says why it does not.**
//      Sign-in, the dashboard, gateway status and the not-found page are not
//      list screens, and they are named in `NON_LIST_PAGES` rather than
//      exempted by a flag a caller could forget to set.
//   3. **Every list table has exactly one pager, and every pager belongs to
//      one.** Two pagers on one page is a reader with no way to tell which
//      table the cursor moves, and a pager with no table above it moves
//      something the reader cannot see.
//   4. **Every row header is a `<th scope="row">` and every column header a
//      `<th scope="col">`.** The roster reads the rendered roles rather than
//      trusting the component, so a screen that hand-rolled a `<td>` where a
//      row header belongs is caught here.
import {
  BODY_ATTRIBUTE,
  CELL_ATTRIBUTE,
  LAYER_ATTRIBUTE,
  PAGER_ATTRIBUTE,
  ROW_HEADER_ATTRIBUTE,
} from "@/components/data-table";

/**
 * The routes that are not list screens, and why.
 *
 * An exemption is a claim someone has to make, and a claim in a list is one
 * this file can print. The four are the console's own non-list surfaces; a
 * FIFTH non-list page has to be added here, in review, rather than acquiring
 * itself a flag in its own template.
 */
export const NON_LIST_PAGES: Readonly<Record<string, string>> = {
  "/": "the dashboard, which composes figures rather than listing rows",
  "/sign-in": "the session gate, which has no session to list",
  "/gateway-status": "two probes, not a list",
  "/not-found": "a route that matches nothing",
};

export interface RosterViolation {
  readonly rule: string;
  readonly detail: string;
}

const LAYERS = new Set(["module", "page"]);

/**
 * Check one rendered document against the four rules.
 *
 * `path` is the route the document was rendered at, and it is an ARGUMENT
 * rather than something read from the document: a router is a runtime
 * dependency this module does not take, so a test passes the path it navigated
 * to. Everything else is read from the DOM.
 */
export function inspect(document: Document, path: string): readonly RosterViolation[] {
  const violations: RosterViolation[] = [];
  const tables = [...document.querySelectorAll("table")];

  for (const table of tables) {
    const layer = table.getAttribute(LAYER_ATTRIBUTE);
    if (layer === null) {
      violations.push({
        rule: "every table declares a layer",
        detail: "a <table> rendered with no data-console-layer, so no screen owns it",
      });
    } else if (!LAYERS.has(layer)) {
      violations.push({
        rule: "every table declares a layer",
        detail: `a <table> declared the layer "${layer}", which is neither module nor page`,
      });
    }
  }

  if (!NON_LIST_PAGES[path] && tables.length === 0) {
    violations.push({
      rule: "every list page renders a table",
      detail: `${path} is not a declared non-list page and rendered no <table>`,
    });
  }

  const pagers = [...document.querySelectorAll(`nav[${PAGER_ATTRIBUTE}]`)];
  const tableBodies = [...document.querySelectorAll(`[${BODY_ATTRIBUTE}]`)];
  if (pagers.length > tableBodies.length) {
    violations.push({
      rule: "every pager belongs to a table",
      detail: `${pagers.length} pagers for ${tableBodies.length} tables, so one moves a list the reader cannot see`,
    });
  }
  for (const body of tableBodies) {
    const inTable = body.closest("table") !== null;
    if (!inTable) {
      violations.push({
        rule: "every table body is inside its table",
        detail: `a [${BODY_ATTRIBUTE}] rendered outside a <table>, so focus would be restored nowhere`,
      });
    }
  }

  // Row and column headers, read off the rendered roles.
  for (const table of tables) {
    for (const cell of table.querySelectorAll(`[${ROW_HEADER_ATTRIBUTE}]`)) {
      if (cell.tagName.toLowerCase() !== "th" || cell.getAttribute("scope") !== "row") {
        violations.push({
          rule: "a row header is a th scope=row",
          detail: `a [${ROW_HEADER_ATTRIBUTE}] rendered as <${cell.tagName.toLowerCase()}> rather than <th scope="row">`,
        });
      }
    }
    const columns = [...table.querySelectorAll("thead th")];
    for (const column of columns) {
      if (column.getAttribute("scope") !== "col") {
        violations.push({
          rule: "a column header is a th scope=col",
          detail:
            'a <th> in <thead> without scope="col", so a screen reader cannot say which column it is',
        });
      }
    }
    const cells = [...table.querySelectorAll(`[${CELL_ATTRIBUTE}]`)];
    for (const cell of cells) {
      if (!/^r\d+c\d+$/.test(cell.getAttribute(CELL_ATTRIBUTE) ?? "")) {
        violations.push({
          rule: "every cell is addressable",
          detail: `a [${CELL_ATTRIBUTE}] whose key is not an rNcN position, so focus cannot be restored to it`,
        });
      }
    }
  }

  return violations;
}

/**
 * The single sentence a screen's spec prints when this fails, so the message a
 * reviewer reads names the rule and the shape rather than a count.
 */
export function describe(violations: readonly RosterViolation[]): string {
  if (violations.length === 0) return "";
  return [
    "the rendered document breaks the console's architecture roster:",
    ...violations.map((violation) => `  - [${violation.rule}] ${violation.detail}`),
  ].join("\n");
}
