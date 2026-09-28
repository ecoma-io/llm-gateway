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
// **Eight rules**, each of which a screen can pass on every screen and still be
// wrong, so each is checked per rendered document rather than once per file.
// The count is the point, and it is a fact a test holds rather than a number in
// a comment: `ROSTER_RULES` below is the enumeration, `inspect` emits nothing
// outside it, and a test asserts those two are the same set. A header that says
// four while the code names eight is a header nobody can check — and it is the
// header a reviewer reads to decide whether a rule is MISSING, so a rule it
// does not name is a rule nobody knows exists.
//
//   1. **Every table declares a layer.** A list table in the console is either a
//      page's own table or a module's shared one, and the difference decides
//      who may read it. An undeclared table is a table nobody owns.
//   2. **Every page renders at least one table, or says why it does not.**
//      Sign-in, the dashboard, gateway status and the not-found page are not
//      list screens, and they are named in `NON_LIST_PAGES` rather than
//      exempted by a flag a caller could forget to set.
//   3. **Every list table has at most one pager.** Two pagers on one page is a
//      reader with no way to tell which table the cursor moves.
//   4. **Every pager belongs to a table.** A pager with no table above it moves
//      something the reader cannot see.
//   5. **Every table body is inside its table.** Focus restoration is scoped to
//      the body, so a body that is not inside a `<table>` is a list whose focus
//      return is guaranteed to find nothing.
//   6. **Every row header is a `<th scope="row">`** — the cell that names which
//      row a screen-reader user is in, which Loom's own table never renders.
//   7. **Every column header is a `<th scope="col">`**, read off the rendered
//      roles rather than off the component, so a screen that hand-rolled a
//      header is caught here.
//   8. **Every cell is addressable** by an `rNcN` position, because that key is
//      how `DataTable` finds the SAME cell again after a re-render; a cell
//      without a parseable position is a cell that loses the reader's place.
//
// Rules 3 and 4 are two rules about one pairing — a pager BELONGS to a table —
// and they are kept apart because they fail in opposite directions: two
// pagers under one table is a reader with two cursors and no way to tell them
// apart, and a pager under no table is a cursor for a list nobody can see. A
// rule stated as "exactly one pager per table" would name the first and be
// silent on the second, which is the whole reason rule 4 exists as its own
// line and not as a clause.
//
// Rule 3 is the one worth reading the implementation of. It is stated as "at
// most one" rather than "exactly one", and that is not a rewording — it is the
// defect it exists to catch. Two tables on one route is the NORMAL shape here
// (identity, commerce, accounting and reconciliation all hold two), and a
// cursor-less table is normal too: `pagerFor` returns an empty array whenever
// `has_more` is false, so a single-page list renders no pager at all, and a
// naive "every table needs a pager" rule would fail every screen in the console
// for being correct. So the pairing a reader needs is not one-pager-per-table;
// it is that no table is paged TWICE. The old check was a bare count
// (`pagers > tableBodies`) and it is not restated here for three separate
// reasons, each of which is a case the count cannot see: a count of `<tbody>`
// elements is not a count of `<table>` elements, so the two quantities compared
// are not the two the sentence names; a count can only fail in one direction,
// so two tables and two pagers — the console's own shape on four screens — can
// never be distinguished from a table paged twice; and a count cannot NAME the
// table at fault, which is the part a reviewer actually reads. The check is
// per-table now, for exactly that reason.
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

/**
 * Every rule this roster governs, in the order `inspect` checks them.
 *
 * Exported so the count is a fact a test can hold rather than a number in a
 * comment: `ROSTER_RULES` is what the header enumerates, and a rule added to
 * `inspect` without being added here fails the roster's own roster test — which
 * is the pattern ADR 0012 §7 borrows from `internal/arch`'s
 * `TestTheRuleRosterIsTheDeclaredOne`. A header that drifts from its
 * implementation is not a documentation nit: it is the document a reviewer
 * reads to decide whether a rule is missing, and a rule the header does not
 * name is a rule nobody knows exists.
 */
export const ROSTER_RULES = [
  "every table declares a layer",
  "every list page renders a table",
  "every list table has at most one pager",
  "every pager belongs to a table",
  "every table body is inside its table",
  "a row header is a th scope=row",
  "a column header is a th scope=col",
  "every cell is addressable",
] as const;

export interface RosterViolation {
  readonly rule: string;
  readonly detail: string;
}

const LAYERS = new Set(["module", "page"]);

/**
 * One `DataTable`'s rendered extent, as the roster can see it.
 *
 * `DataTable` wraps its `<table>` and its pager's `<nav>` in one `<div>`, so
 * that div is the CLOSEST COMMON ANCESTOR of the two, and a closest common
 * ancestor is the only boundary that can say "these belong together" without a
 * rule about how many wrappers a screen is allowed to add. Two tables on one
 * route each have their own wrapper, so their two pagers land in two different
 * groups — which is the answer rule 3 needs, and the answer a count cannot
 * give.
 */
interface TableGroup {
  readonly table: Element;
  readonly pagers: readonly Element[];
}

/** The chain of ancestors from `element` up to the document, nearest first. */
function ancestors(element: Element): readonly Element[] {
  const chain: Element[] = [];
  for (let node = element.parentElement; node !== null; node = node.parentElement) {
    chain.push(node);
  }
  return chain;
}

/**
 * The nearest element that contains BOTH, or `null` when there is none.
 *
 * `null` is the answer, not a failure to find one: a `<nav>` sharing no
 * container with any table is an unowned pager, and rule 4 is the rule that
 * reports it. Inventing a common ancestor for it — the `<body>`, say — would
 * attribute it to every table on the page and produce a rule 3 failure
 * describing a defect that is really a rule 4 one.
 */
function commonScope(left: Element, right: Element): Element | null {
  const rightChain = new Set(ancestors(right));
  for (const ancestor of ancestors(left)) if (rightChain.has(ancestor)) return ancestor;
  return null;
}

/**
 * Pair each table with the pagers that share a container with IT and with
 * nothing nearer.
 *
 * A pager belongs to a table when that table is the one it shares the CLOSEST
 * container with — the container nearest the pager — and to no other. So a
 * pager inside a `DataTable`'s own wrapper belongs to that table and to nothing
 * else, which is what makes the console's own two-lists-one-route shape (two
 * tables, two pagers, each pair in its own wrapper) resolve the way a reader
 * resolves it. `DataTable` renders each table and its pager under one `<div>`
 * precisely so the answer is in the DOM.
 *
 * A pager that is EQUALLY near two tables is attributed to NEITHER, and rule 4
 * reports it. That is not a technicality and it is the case a heuristic cannot
 * fake: a pager dropped at the page's own level, below both tables' wrappers,
 * is in the markup of a page with two lists and nothing in that markup says
 * which one it moves. Attributing it to the first table would be the console
 * asserting a binding the document never made, and attributing it to both
 * would invent the two-pagers-one-table defect rule 3 exists to catch. Neither
 * is what happened, so the roster says what it can honestly say: a pager here
 * belongs to no table a reader could name.
 *
 * The nearest scope is chosen by DEPTH, and comparing by anything else is the
 * defect this replaced. Taking the first table that shares any container at
 * all attributes both of a page's pagers to the first table, which fails a
 * screen that did nothing wrong.
 */
function groups(document: Document): readonly TableGroup[] {
  const tables = [...document.querySelectorAll("table")];
  const pagers = [...document.querySelectorAll(`nav[${PAGER_ATTRIBUTE}]`)];
  const paired = new Map<Element, Element[]>();

  for (const pager of pagers) {
    const scopeChain = ancestors(pager);
    // Every table at the NEAREST shared depth, so a tie is visible as a tie
    // rather than resolved by document order.
    let depth = Number.POSITIVE_INFINITY;
    for (const table of tables) {
      const scope = commonScope(table, pager);
      if (scope === null) continue;
      depth = Math.min(depth, scopeChain.indexOf(scope));
    }
    const nearest = tables.filter((table) => {
      const scope = commonScope(table, pager);
      return scope !== null && scopeChain.indexOf(scope) === depth;
    });
    // One table at that depth, or the pager is shared and belongs to none.
    if (nearest.length !== 1) continue;
    const owned = paired.get(nearest[0]!) ?? [];
    owned.push(pager);
    paired.set(nearest[0]!, owned);
  }

  return tables.map((table) => ({ table, pagers: paired.get(table) ?? [] }));
}

/**
 * Check one rendered document against the eight rules.
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

  // Rule 3: at most one pager per table, checked PER TABLE so the message can
  // name the table. A count comparison could not: two tables and two pagers is
  // the console's own shape on four screens, and a count of pagers against a
  // count of bodies can only notice a surplus, never a table that was paged
  // twice by two pagers it does not own.
  const claimed = new Set<Element>();
  for (const group of groups(document)) {
    for (const pager of group.pagers) claimed.add(pager);
    if (group.pagers.length > 1) {
      violations.push({
        rule: "every list table has at most one pager",
        detail: `a <table> rendered ${group.pagers.length} pagers, so a reader pressing Next cannot tell which table's cursor moves`,
      });
    }
  }
  for (const body of document.querySelectorAll(`[${BODY_ATTRIBUTE}]`)) {
    if (body.closest("table") === null) {
      violations.push({
        rule: "every table body is inside its table",
        detail: `a [${BODY_ATTRIBUTE}] rendered outside a <table>, so focus would be restored nowhere`,
      });
    }
  }

  // Rule 4: a pager no table claims. Read against the TABLES, not against the
  // bodies, because the two are different quantities and a screen that renders
  // a body outside a table already has that reported.
  for (const pager of document.querySelectorAll(`nav[${PAGER_ATTRIBUTE}]`)) {
    if (!claimed.has(pager)) {
      violations.push({
        rule: "every pager belongs to a table",
        detail: `a [${PAGER_ATTRIBUTE}] rendered with no table beside it, so it moves a list the reader cannot see`,
      });
    }
  }

  // Row headers, column headers and cells, read off the rendered roles.
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
