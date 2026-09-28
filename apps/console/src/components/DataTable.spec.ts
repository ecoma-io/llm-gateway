// The table's three reasons for existing are asserted as facts about the
// rendered DOM, never about the component's options: a caption that names the
// table, headers that scope to their column, and a row header on the first
// column of every row. Loom's `Table` renders none of the last two, so a
// regression to it is a red test rather than a style review.
//
// Every mount is attached to a real `document.body` container. A focus test
// against a detached tree asserts something about this component's setup code
// and not one thing about what a keyboard user experiences: `document
// .activeElement` never leaves `body` in a detached tree, so the assertion
// below would pass for the wrong reason — or fail for one.
import { mount, type VueWrapper } from "@vue/test-utils";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { defineComponent, h, nextTick, ref } from "vue";
import { createMemoryHistory, createRouter, type Router } from "vue-router";

import DataTable from "./DataTable.vue";
import {
  CELL_ATTRIBUTE,
  ROW_HEADER_ATTRIBUTE,
  type DataTableColumn,
  type FocusReturn,
  type PageLink,
} from "./data-table";

const COLUMNS: readonly DataTableColumn[] = [
  { key: "name", label: "Name" },
  { key: "state", label: "State", align: "right" },
];

interface Row {
  name: string;
  state: string;
}

function rows(count: number): Row[] {
  return Array.from({ length: count }, (_unused, index) => ({
    name: `row-${index}`,
    state: `state-${index}`,
  }));
}

function makeRouter(): Router {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/buckets", component: { template: "<div />" } },
      { path: "/buckets/page/:after", component: { template: "<div />" } },
    ],
  });
}

/** A cell slot receives the row and its index; that is the whole contract a screen fills. */
type CellSlot = (slotProps: { row: unknown; index: number }) => unknown;

/** Mount into a live document, so focus and the accessibility tree are real. */
async function mountTable(
  props: Record<string, unknown> = {},
  slots: Record<string, CellSlot> = {},
) {
  const router = makeRouter();
  await router.push("/buckets");
  await router.isReady();

  const container = document.createElement("div");
  document.body.appendChild(container);

  const wrapper: VueWrapper = mount(DataTable, {
    attachTo: container,
    props: { caption: "Funding buckets", columns: COLUMNS, rows: rows(3), ...props },
    slots,
    global: { plugins: [router] },
  });
  await nextTick();
  return { wrapper, router, container };
}

describe("DataTable", () => {
  beforeEach(() => {
    document.body.innerHTML = "";
  });

  afterEach(() => {
    document.body.innerHTML = "";
  });

  it("renders a real caption that names the table", async () => {
    const { wrapper } = await mountTable({ caption: "Funding buckets" });
    const caption = wrapper.find("table > caption");

    expect(caption.exists()).toBe(true);
    expect(caption.text()).toBe("Funding buckets");
    // The caption is the table's accessible NAME, not a heading sitting above
    // it: a screen-reader user announcing this table must hear the sentence
    // that says what it is.
    expect(wrapper.find("table").attributes("aria-labelledby")).toBe(caption.attributes("id"));
  });

  it("renders a thead whose headers scope to their column", async () => {
    const { wrapper } = await mountTable();

    const headers = wrapper.findAll("table > thead > tr > th");
    expect(headers).toHaveLength(2);
    for (const header of headers) {
      expect(header.attributes("scope")).toBe("col");
    }
    expect(headers.map((header) => header.text())).toEqual(["Name", "State"]);
  });

  it("renders the first column of every row as a th with scope row", async () => {
    const { wrapper } = await mountTable({ rows: rows(3) });

    const rowHeaders = wrapper.findAll(`tbody tr > [${ROW_HEADER_ATTRIBUTE}]`);
    expect(rowHeaders).toHaveLength(3);
    for (const header of rowHeaders) {
      expect(header.element.tagName).toBe("TH");
      expect(header.attributes("scope")).toBe("row");
    }
    expect(rowHeaders.map((header) => header.text())).toEqual(["row-0", "row-1", "row-2"]);

    // Everything that is not the first column is an ordinary data cell. A table
    // that made every column a header would be a different lie, and one that
    // made none is Loom's.
    expect(wrapper.findAll("tbody tr > td")).toHaveLength(3);
  });

  it("fills a cell from a named slot and falls back to the row's own value", async () => {
    const { wrapper } = await mountTable(
      { rows: [{ name: "bucket-a", state: "active" }] },
      {
        // The row arrives as `unknown` — the table is untyped about its rows, and
        // a screen module that knows its row type casts here, which is the
        // boundary the cast is honest about.
        state: ({ row }: { row: unknown }) => h("span", { class: "badge" }, (row as Row).state),
      },
    );

    expect(wrapper.find(`tbody tr [${ROW_HEADER_ATTRIBUTE}]`).text()).toBe("bucket-a");
    expect(wrapper.find("tbody tr td .badge").text()).toBe("active");
  });

  it("paginates as a nav of real links and never as buttons", async () => {
    const pages: PageLink[] = [
      { label: "First", to: "/buckets" },
      { label: "Next", to: "/buckets/page/c0-9tOQ" },
    ];
    const { wrapper } = await mountTable({ pages, pagesLabel: "Funding bucket pages" });

    expect(wrapper.find("nav").attributes("aria-label")).toBe("Funding bucket pages");
    // Loom's Pagination would render buttons here, which is why it is not
    // used: Back must undo a page change and a button is not a history entry.
    expect(wrapper.findAll("nav a")).toHaveLength(2);
    expect(wrapper.findAll("nav button")).toHaveLength(0);
    expect(wrapper.find("nav a").attributes("href")).toBe("/buckets");
    // The cursor travels in the URL verbatim, as the opaque string it is.
    expect(wrapper.findAll("nav a")[1].attributes("href")).toBe("/buckets/page/c0-9tOQ");
  });

  it("marks exactly the current position and renders no page count", async () => {
    const pages: PageLink[] = [
      { label: "First", to: "/buckets", current: true },
      { label: "Next", to: "/buckets/page/c0-9tOQ" },
    ];
    const { wrapper } = await mountTable({ pages });

    const current = wrapper.findAll('nav a[aria-current="page"]');
    expect(current).toHaveLength(1);
    expect(current[0].text()).toBe("First");

    // No "of N", no page number, no jump-to-page: the contract carries
    // `has_more` and not a total, and a keyset cursor names a position that no
    // number can be derived from.
    const pager = wrapper.find("nav").text();
    expect(pager).not.toMatch(/\d+\s*of\s*\d+/);
    expect(pager).not.toMatch(/page\s*\d+/i);
    expect(pager).toBe("FirstNext");
  });

  it("renders an empty page as a sentence rather than a bare table", async () => {
    const { wrapper } = await mountTable({
      rows: [],
      state: "empty",
      emptyMessage: "No buckets yet.",
    });

    expect(wrapper.find("tbody tr").text()).toBe("No buckets yet.");
    expect(wrapper.findAll(`tbody tr [${ROW_HEADER_ATTRIBUTE}]`)).toHaveLength(0);
    // The headers are still there: an empty list is a legitimate answer, and
    // the table's shape does not depend on data having arrived.
    expect(wrapper.findAll("thead th")).toHaveLength(2);
  });

  it("says it is loading rather than claiming a list is empty", async () => {
    const { wrapper } = await mountTable({ rows: [], state: "loading" });

    expect(wrapper.find("tbody tr").text()).toBe("Loading…");
  });

  describe("return focus after a re-render", () => {
    // Loom's `DataGrid` does this internally: a re-render that unmounts the
    // focused element leaves focus on `document.body`, and a screen-reader user
    // is at the start of the document. The table remembers which cell held
    // focus before the update and puts focus on that cell's replacement.
    function harness() {
      const returns: FocusReturn[] = [];
      const rowsRef = ref(rows(3));
      const Host = defineComponent({
        setup() {
          return () =>
            h(DataTable, {
              caption: "Funding buckets",
              columns: COLUMNS,
              rows: rowsRef.value,
              onFocusReturn: (result: FocusReturn) => returns.push(result),
            });
        },
      });
      return { Host, rowsRef, returns };
    }

    async function mountHarness() {
      const router = makeRouter();
      await router.push("/buckets");
      await router.isReady();
      const container = document.createElement("div");
      document.body.appendChild(container);
      const h2 = harness();
      const wrapper = mount(h2.Host, { attachTo: container, global: { plugins: [router] } });
      await nextTick();
      return { ...h2, wrapper };
    }

    it("keeps focus inside the table when a re-render replaces the focused element", async () => {
      const { rowsRef, returns, wrapper } = await mountHarness();

      const target = wrapper.get(`tbody tr [${ROW_HEADER_ATTRIBUTE}]`).element as HTMLElement;
      target.setAttribute("tabindex", "-1");
      target.focus();
      expect(document.activeElement).toBe(target);

      // Re-render with a different page: the exact element is replaced.
      rowsRef.value = [{ name: "other", state: "x" }];
      await nextTick();
      await nextTick();

      const active = document.activeElement as HTMLElement;
      expect(active.tagName.toLowerCase()).not.toBe("body");
      expect(wrapper.get("tbody").element.contains(active)).toBe(true);
      expect(returns.at(-1)?.restored).toBe(true);
    });

    it("focuses the matching cell by key, not merely the nth one", async () => {
      const { rowsRef, wrapper } = await mountHarness();

      // Focus the SECOND row's header, then re-render with more rows above it,
      // so an index-based restore would land on the wrong row entirely.
      const target = wrapper.get(`tbody tr:nth-child(2) [${ROW_HEADER_ATTRIBUTE}]`)
        .element as HTMLElement;
      target.setAttribute("tabindex", "-1");
      target.focus();
      const key = target.getAttribute(CELL_ATTRIBUTE);

      rowsRef.value = rows(5);
      await nextTick();
      await nextTick();

      const active = document.activeElement as HTMLElement;
      expect(active.getAttribute(CELL_ATTRIBUTE)).toBe(key);
      expect(active.textContent).toBe("row-1");
    });

    it("reports honestly when there is nothing to focus", async () => {
      const { rowsRef, returns, wrapper } = await mountHarness();

      const target = wrapper.get(`tbody tr [${ROW_HEADER_ATTRIBUTE}]`).element as HTMLElement;
      target.setAttribute("tabindex", "-1");
      target.focus();

      rowsRef.value = [];
      await nextTick();
      await nextTick();

      // The claim that matters is the negative one: the component does NOT
      // report a restore it did not perform, and focus is never left on a
      // detached node pretending to be fine.
      expect(returns.at(-1)?.restored).toBe(false);
      expect(document.activeElement?.tagName.toLowerCase()).toBe("body");
    });

    it("does not steal focus when nothing in the table held it", async () => {
      const { rowsRef, returns } = await mountHarness();

      const outside = document.createElement("button");
      document.body.appendChild(outside);
      outside.focus();
      expect(document.activeElement).toBe(outside);

      rowsRef.value = rows(2);
      await nextTick();
      await nextTick();

      expect(document.activeElement).toBe(outside);
      expect(returns).toHaveLength(0);
    });
  });

  describe("grid role", () => {
    it("keeps a plain table out of the tab order", async () => {
      const { wrapper } = await mountTable({ rows: rows(2) });

      // Tabbing through forty rows to reach the pager is a defect, not a
      // feature, so a data table's cells are not tab stops.
      for (const cell of wrapper.findAll(`tbody [${CELL_ATTRIBUTE}]`)) {
        expect(cell.attributes("tabindex")).toBe("-1");
      }
      expect(wrapper.find("table").attributes("role")).toBe("table");
    });

    it("puts exactly one cell in the tab order when it is a grid", async () => {
      const { wrapper } = await mountTable({ rows: rows(3), role: "grid" });

      expect(wrapper.find("table").attributes("role")).toBe("grid");
      const stops = wrapper.findAll(`tbody [${CELL_ATTRIBUTE}][tabindex="0"]`);
      expect(stops).toHaveLength(1);
      expect(stops[0].attributes(CELL_ATTRIBUTE)).toBe("r0c0");
    });

    it("moves the roving tab stop with the arrow keys", async () => {
      const { wrapper } = await mountTable({ rows: rows(3), role: "grid" });

      // Dispatched from the CELL, as a real keypress is: the handler reads the
      // target to learn which cell the reader was in, so an event fired at the
      // tbody would tell it nothing and prove nothing.
      const press = async (key: string, ctrlKey = false) => {
        const from = document.activeElement as HTMLElement;
        from.dispatchEvent(
          new KeyboardEvent("keydown", { key, ctrlKey, bubbles: true, cancelable: true }),
        );
        await nextTick();
      };

      (wrapper.get(`tbody [${CELL_ATTRIBUTE}="r0c0"]`).element as HTMLElement).focus();
      await press("ArrowRight");
      expect(document.activeElement?.getAttribute(CELL_ATTRIBUTE)).toBe("r0c1");

      await press("ArrowDown");
      expect(document.activeElement?.getAttribute(CELL_ATTRIBUTE)).toBe("r1c1");

      // End at the LAST column is a no-op, not a wrap. The reader is already
      // on `r1c1` — the last column of a three-column row — so this is the one
      // End press that can fail by going somewhere else, and the earlier
      // position it would have to have started from is stated above so a reader
      // can see the state the assertion is about rather than infer it.
      await press("End");
      expect(document.activeElement?.getAttribute(CELL_ATTRIBUTE)).toBe("r1c1");

      // Home from that last column reaches the first, so the End that follows
      // is pressed from a DIFFERENT cell than the one above and asserts a
      // transition rather than a fixed point.
      await press("Home");
      expect(document.activeElement?.getAttribute(CELL_ATTRIBUTE)).toBe("r1c0");

      // End alone is the last column of THIS row; Ctrl+End is the last cell of
      // the table, which is the distinction a spreadsheet user expects. From
      // `r1c0` the plain End is a real move to `r1c1`, and Ctrl+End then leaves
      // the row entirely for `r2c1` — so the two are told apart by WHERE they
      // land, not by one being a repeat of the other.
      await press("End");
      expect(document.activeElement?.getAttribute(CELL_ATTRIBUTE)).toBe("r1c1");

      await press("End", true);
      expect(document.activeElement?.getAttribute(CELL_ATTRIBUTE)).toBe("r2c1");
    });

    it("stops at the edges instead of wrapping", async () => {
      const { wrapper } = await mountTable({ rows: rows(2), role: "grid" });

      (wrapper.get(`tbody [${CELL_ATTRIBUTE}="r0c0"]`).element as HTMLElement).focus();
      for (const key of ["ArrowUp", "ArrowLeft"]) {
        const from = document.activeElement as HTMLElement;
        from.dispatchEvent(new KeyboardEvent("keydown", { key, bubbles: true, cancelable: true }));
        await nextTick();
        expect(document.activeElement?.getAttribute(CELL_ATTRIBUTE)).toBe("r0c0");
      }
    });

    it("leaves an unhandled key to the browser", async () => {
      const { wrapper } = await mountTable({ rows: rows(2), role: "grid" });

      const cell = wrapper.get(`tbody [${CELL_ATTRIBUTE}="r0c0"]`).element as HTMLElement;
      cell.focus();
      // Dispatched on the cell rather than through `trigger`, so the event
      // reaches the handler with a real `target`: a handler that reads the
      // target cannot be exercised by an event that has none.
      const event = new KeyboardEvent("keydown", { key: "a", bubbles: true, cancelable: true });
      cell.dispatchEvent(event);
      await nextTick();

      expect(event.defaultPrevented).toBe(false);
      expect(document.activeElement?.getAttribute(CELL_ATTRIBUTE)).toBe("r0c0");
    });
  });
});
