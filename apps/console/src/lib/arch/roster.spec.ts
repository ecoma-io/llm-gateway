// The roster, checked against a real rendered document rather than a hand-built
// one.
//
// The claims here are all of the shape "a screen can pass this rule on every
// screen and still be wrong", which is why each test asserts on markup that
// actually violates the rule and expects the roster to name it. A roster that
// is only ever shown conforming markup is indistinguishable from a roster that
// inspects nothing: both return zero violations, and only one of them means it.
//
// The exception is the LAST block, and it earns its name honestly rather than
// by being renamed. Rendering every screen in the console from this file would
// mean re-declaring all ten API seams, the router, Pinia and the fixtures five
// times over — five copies of the mount each screen's own spec already owns,
// each one drifting from the screen it claims to check. The screens ARE checked
// against their real rendered markup, in the page specs that mount them
// ("keeps the console's architecture roster" in each of CatalogPage,
// IdentityPage, CommercePage, AccountingPage and ReconciliationPage). What this
// file contributes is the property those five cannot state: the roster is
// PATH-AGNOSTIC, so the same conforming document passes at every list route and
// the exemption list is consulted per path. That is the assertion, and the
// title says it.
import { describe, expect, it } from "vitest";

import { NON_LIST_PAGES, ROSTER_RULES, describe as describeViolations, inspect } from "./roster";

/** A conforming list page: one table, declared, with its headers and its pager. */
function CONFORMING(): string {
  return `
    <main>
      <table data-console-layer="page">
        <caption>Users</caption>
        <thead>
          <tr><th scope="col">Name</th><th scope="col">State</th></tr>
        </thead>
        <tbody data-table-body>
          <tr>
            <th scope="row" data-row-header data-cell="r0c0">ada</th>
            <td data-cell="r0c1">active</td>
          </tr>
        </tbody>
      </table>
      <nav data-table-pager aria-label="Users"><a href="/identity">Next</a></nav>
    </main>`;
}

/**
 * A document, rendered. The path is NOT taken here and passed along: it goes
 * to `inspect`, which is the only thing that reads it, and a helper that
 * accepted one would have let four tests hand a path to a function that drops
 * it — which is what happened, and the types are what said so.
 */
function render(html: string): Document {
  document.body.innerHTML = html;
  return document;
}

describe("the roster's own roster", () => {
  it("names every rule it emits, and emits no rule it does not name", () => {
    // The header enumerates the rules, and a header that has drifted from the
    // implementation is not a documentation nit: it is the document a reviewer
    // reads to decide whether a rule is MISSING. A rule the header does not
    // name is a rule nobody knows exists, and a rule the code emits that the
    // header does not name is a rule nobody can reason about before seeing it
    // fail. The count is asserted as a number too, so "eight" in the header is
    // a claim with a test behind it rather than a word.
    expect(ROSTER_RULES).toHaveLength(8);

    // Every rule in the roster, found by breaking it: each entry gets a
    // document that violates exactly that one, so a rule that stops governing
    // anything fails here rather than silently returning nothing.
    const byRule = {
      "every table declares a layer": `<table><tbody data-table-body></tbody></table>`,
      "every list page renders a table": `<main><p>Nothing here.</p></main>`,
      "every list table has at most one pager": CONFORMING().replace(
        "</main>",
        `<nav data-table-pager><a href="/x">Next</a></nav></main>`,
      ),
      "every pager belongs to a table": `<main><nav data-table-pager></nav></main>`,
      "every table body is inside its table": `<main><div data-table-body></div></main>`,
      "a row header is a th scope=row": `<table data-console-layer="page"><tbody data-table-body><tr><td data-row-header data-cell="r0c0">a</td></tr></tbody></table>`,
      "a column header is a th scope=col": `<table data-console-layer="page"><thead><tr><th>Name</th></tr></thead><tbody data-table-body><tr><th scope="row" data-cell="r0c0">a</th></tr></tbody></table>`,
      "every cell is addressable": `<table data-console-layer="page"><tbody data-table-body><tr><td data-cell="somewhere">a</td></tr></tbody></table>`,
    };
    expect(Object.keys(byRule).sort()).toEqual([...ROSTER_RULES].sort());

    for (const rule of ROSTER_RULES) {
      const found = inspect(render(byRule[rule]), "/identity");
      expect(
        found.map((violation) => violation.rule),
        rule,
      ).toContain(rule);
    }
  });

  it("emits nothing that is not on the roster, whatever the document does", () => {
    // The other direction, and the one that needs a document that BREAKS
    // something: every rule the code can emit has to be reachable from a rule
    // that fires, so a rule added to `inspect` and forgotten in
    // `ROSTER_RULES` is a rule a reviewer cannot look up. Checked over the
    // union of every violating document in the file plus the console's own
    // two-list shape, because a rule that only fires on a rare document is
    // exactly the one that gets left off the list.
    const documents: readonly [string, string][] = [
      ["an undeclared table", `<table><tbody data-table-body></tbody></table>`],
      [
        "a foreign layer",
        `<table data-console-layer="global"><tbody data-table-body></tbody></table>`,
      ],
      ["no table at all", `<main><p>Nothing here.</p></main>`],
      [
        "two pagers on one list",
        CONFORMING().replace("</main>", `<nav data-table-pager></nav></main>`),
      ],
      ["a pager with no table", `<main><nav data-table-pager></nav></main>`],
      [
        "a body outside a table",
        `<main><table data-console-layer="page"></table><div data-table-body></div></main>`,
      ],
      [
        "a td row header",
        `<table data-console-layer="page"><tbody data-table-body><tr><td data-row-header data-cell="r0c0">a</td></tr></tbody></table>`,
      ],
      [
        "an unscoped column header",
        `<table data-console-layer="page"><thead><tr><th>Name</th></tr></thead><tbody data-table-body><tr><th scope="row" data-cell="r0c0">a</th></tr></tbody></table>`,
      ],
      [
        "an unaddressable cell",
        `<table data-console-layer="page"><tbody data-table-body><tr><td data-cell="somewhere">a</td></tr></tbody></table>`,
      ],
      [
        "two lists, one route",
        `<main><div><table data-console-layer="page"><tbody data-table-body><tr><th scope="row" data-cell="r0c0">a</th></tr></tbody></table><nav data-table-pager></nav></div><div><table data-console-layer="page"><tbody data-table-body><tr><th scope="row" data-cell="r0c0">b</th></tr></tbody></table><nav data-table-pager></nav></div></main>`,
      ],
      [
        "every rule broken at once",
        `<main><table><tbody data-table-body><tr><td data-row-header data-cell="x">a</td></tr></tbody></table><nav data-table-pager></nav><div data-table-body></div><table data-console-layer="page"><thead><tr><th>N</th></tr></thead></table></main>`,
      ],
    ];

    const declared = new Set<string>(ROSTER_RULES);
    for (const [what, html] of documents) {
      for (const violation of inspect(render(html), "/identity")) {
        expect(declared.has(violation.rule), `${what} -> ${violation.rule}`).toBe(true);
      }
    }
  });
});

describe("a conforming page", () => {
  it("breaks nothing", () => {
    expect(inspect(render(CONFORMING()), "/identity")).toEqual([]);
  });

  it("breaks nothing at every list route, and asks no path of the markup", () => {
    // Path-agnosticism, which is the property the five page specs each check
    // one instance of: the same document passes wherever a list screen is
    // rendered, so a screen is not exempt from a rule by where it was mounted.
    for (const path of ["/identity", "/catalog", "/commerce", "/accounting", "/reconciliation"]) {
      expect(
        inspect(render(CONFORMING()), path).map((v) => v.rule),
        path,
      ).toEqual([]);
    }
  });
});

describe("a table nobody owns", () => {
  it("names a table with no declared layer", () => {
    const doc = render(
      `<table><tbody data-table-body><tr><td data-cell="r0c0">x</td></tr></tbody></table>`,
    );
    const violations = inspect(doc, "/identity");

    expect(violations).toContainEqual(
      expect.objectContaining({ rule: "every table declares a layer" }),
    );
  });

  it("names a layer the contract of layers does not have", () => {
    const doc = render(
      `<table data-console-layer="global"><tbody data-table-body></tbody></table>`,
    );
    expect(inspect(doc, "/identity")).toContainEqual(
      expect.objectContaining({
        rule: "every table declares a layer",
        detail: expect.stringContaining("global"),
      }),
    );
  });
});

describe("a list page with no list", () => {
  it("names a non-declared page that rendered no table", () => {
    const doc = render("<main><p>Nothing here.</p></main>");

    expect(inspect(doc, "/identity")).toContainEqual(
      expect.objectContaining({ rule: "every list page renders a table" }),
    );
  });

  it("does not name the four pages that are not list screens", () => {
    // The exemption is a list someone has to argue for, and a screen that
    // acquires itself one by rendering nothing would be exempting itself.
    for (const [path, why] of Object.entries(NON_LIST_PAGES)) {
      const doc = render("<main><p>No table here.</p></main>");
      expect(inspect(doc, path), `${path} — ${why}`).toEqual([]);
    }
  });
});

describe("two pagers one reader cannot tell apart", () => {
  // The case the header's own stated harm names — "two pagers on one page is a
  // reader with no way to tell which table the cursor moves" — and the case a
  // count of pagers against a count of bodies CANNOT see, because 2 > 2 is
  // false. Each of the three below renders two pagers for two tables and is
  // the shape the count already passed: what changes is WHERE the second pager
  // sits, and only the pairing can see it.
  it("names two pagers rendered under one table", () => {
    const doc = render(`
      <main>
        <table data-console-layer="page">
          <tbody data-table-body><tr><th scope="row" data-cell="r0c0">ada</th></tr></tbody>
        </table>
        <nav data-table-pager><a href="/identity?after=c2">Next</a></nav>
        <nav data-table-pager><a href="/identity?after=c3">Older</a></nav>
      </main>`);

    expect(inspect(doc, "/identity")).toContainEqual(
      expect.objectContaining({
        rule: "every list table has at most one pager",
        detail: expect.stringContaining("2 pagers"),
      }),
    );
  });

  it("names a pager that one list has been given beside its own", () => {
    // The shape a screen acquires by pasting a second `<DataTable>`'s pager
    // into the FIRST table's wrapper: the first list with two pagers inside
    // its own extent, and the second table beside it with none. The old count
    // saw two pagers and two bodies and passed it, because 2 > 2 is false.
    const doc = render(`
      <main>
        <div>
          <table data-console-layer="page">
            <tbody data-table-body><tr><th scope="row" data-cell="r0c0">ada</th></tr></tbody>
          </table>
          <nav data-table-pager><a href="/identity?after=c-users-2">Next</a></nav>
          <nav data-table-pager><a href="/identity?after=c-keys-2">Next</a></nav>
        </div>
        <div>
          <table data-console-layer="page">
            <tbody data-table-body><tr><th scope="row" data-cell="r0c0">ci</th></tr></tbody>
          </table>
        </div>
      </main>`);

    expect(inspect(doc, "/identity")).toContainEqual(
      expect.objectContaining({
        rule: "every list table has at most one pager",
        detail: expect.stringContaining("2 pagers"),
      }),
    );
  });

  it("names a pager a page rendered where nothing in the markup ties it to a table", () => {
    // Two tables and one pager at the page's own level, below neither table's
    // wrapper. Nothing says which list it moves, and the roster does not
    // guess: rule 4 fires on the unowned pager and rule 3 does NOT fire on
    // either table, because attaching the pager to the nearer of the two would
    // be the console asserting a binding the document never made. The two
    // halves of rule 3 and rule 4 are kept apart for exactly this case.
    const doc = render(`
      <main>
        <div>
          <table data-console-layer="page">
            <tbody data-table-body><tr><th scope="row" data-cell="r0c0">ada</th></tr></tbody>
          </table>
        </div>
        <nav data-table-pager><a href="/identity?after=c2">Next</a></nav>
        <div>
          <table data-console-layer="page">
            <tbody data-table-body><tr><th scope="row" data-cell="r0c0">ci</th></tr></tbody>
          </table>
        </div>
      </main>`);

    const rules = inspect(doc, "/identity").map((violation) => violation.rule);
    expect(rules).toContain("every pager belongs to a table");
    expect(rules).not.toContain("every list table has at most one pager");
  });

  it("does not name the console's own shape: two tables, two pagers, one each", () => {
    // The case a naive "every table needs a pager" rule would fail the whole
    // console for being correct, and the case this rule has to keep passing.
    const doc = render(`
      <main>
        <div>
          <table data-console-layer="page">
            <tbody data-table-body><tr><th scope="row" data-cell="r0c0">ada</th></tr></tbody>
          </table>
          <nav data-table-pager><a href="/identity?after=c-users-2">Next</a></nav>
        </div>
        <div>
          <table data-console-layer="page">
            <tbody data-table-body><tr><th scope="row" data-cell="r0c0">ci</th></tr></tbody>
          </table>
          <nav data-table-pager><a href="/identity?after=c-keys-2">Next</a></nav>
        </div>
      </main>`);

    expect(inspect(doc, "/identity")).toEqual([]);
  });

  it("does not name a table with no pager, because a one-page list has none", () => {
    // `pagerFor` returns an empty array whenever `has_more` is false, so a
    // single-page list renders no `<nav>` at all. "Every table has a pager"
    // would fail every screen in this console on its normal case.
    const doc = render(`
      <main>
        <table data-console-layer="page">
          <tbody data-table-body><tr><th scope="row" data-cell="r0c0">ada</th></tr></tbody>
        </table>
      </main>`);

    expect(inspect(doc, "/identity")).toEqual([]);
  });
});

describe("a pager that moves nothing", () => {
  it("names a pager with no table to move", () => {
    const doc = render(`<main><nav data-table-pager><a href="/x">Next</a></nav></main>`);

    expect(inspect(doc, "/identity")).toContainEqual(
      expect.objectContaining({ rule: "every pager belongs to a table" }),
    );
  });

  it("names a table body rendered outside any table", () => {
    // Focus restoration is scoped to this element, so a body outside a table
    // is a table whose focus return is guaranteed to find nothing.
    const doc = render(
      `<main><table data-console-layer="page"></table><div data-table-body></div></main>`,
    );

    expect(inspect(doc, "/identity")).toContainEqual(
      expect.objectContaining({ rule: "every table body is inside its table" }),
    );
  });
});

describe("headers the reader cannot hear", () => {
  it("names a row header that is not a row header", () => {
    // The state Loom's Table leaves every list in: a first cell with nothing
    // naming which row a screen-reader user is in.
    const doc = render(`
      <table data-console-layer="page">
        <tbody data-table-body>
          <tr><td data-row-header data-cell="r0c0">ada</td></tr>
        </tbody>
      </table>`);

    expect(inspect(doc, "/identity")).toContainEqual(
      expect.objectContaining({ rule: "a row header is a th scope=row" }),
    );
  });

  it("names a column header with no scope", () => {
    const doc = render(`
      <table data-console-layer="page">
        <thead><tr><th>Name</th></tr></thead>
        <tbody data-table-body><tr><th scope="row" data-cell="r0c0">ada</th></tr></tbody>
      </table>`);

    expect(inspect(doc, "/identity")).toContainEqual(
      expect.objectContaining({ rule: "a column header is a th scope=col" }),
    );
  });

  it("names a cell focus could never be restored to", () => {
    // The key is how `DataTable` finds the SAME cell after a re-render. A cell
    // without a parseable position is a cell that loses the reader's place.
    const doc = render(`
      <table data-console-layer="page">
        <tbody data-table-body><tr><td data-cell="somewhere">ada</td></tr></tbody>
      </table>`);

    expect(inspect(doc, "/identity")).toContainEqual(
      expect.objectContaining({ rule: "every cell is addressable" }),
    );
  });
});

describe("the message a reviewer reads", () => {
  it("names the rule and the shape, not a count", () => {
    const doc = render("<main><p>Nothing here.</p></main>");
    const message = describeViolations(inspect(doc, "/identity"));

    expect(message).toContain("every list page renders a table");
    expect(message).toContain("/identity");
    expect(message).not.toMatch(/^\s*\d+\s*violations?$/m);
  });

  it("says nothing when nothing broke", () => {
    expect(describeViolations([])).toBe("");
  });
});
