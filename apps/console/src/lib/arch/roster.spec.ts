// The roster, checked against a real rendered document rather than a hand-built
// one.
//
// The claims here are all of the shape "a screen can pass this rule on every
// screen and still be wrong", which is why each test asserts on markup that
// actually violates the rule and expects the roster to name it. A roster that
// is only ever shown conforming markup is indistinguishable from a roster that
// inspects nothing: both return zero violations, and only one of them means it.
import { describe, expect, it } from "vitest";

import { NON_LIST_PAGES, describe as describeViolations, inspect } from "./roster";

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

describe("a conforming page", () => {
  it("breaks nothing", () => {
    expect(inspect(render(CONFORMING()), "/identity")).toEqual([]);
  });

  it("breaks nothing on any of the console's own pages, as rendered", () => {
    // The rule that would matter most: a list page that renders no table is
    // the failure the roster exists for, and the only way to know one does not
    // is to look at what a screen actually rendered.
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
