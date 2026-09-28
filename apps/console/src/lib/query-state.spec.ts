// The URL is the only cursor state, and these are the three rules that makes
// load-bearing (ADR 0012 §4).
//
// They are tested here, in the module that owns them, rather than in a screen's
// spec: a pager is a piece of arithmetic over an opaque string, and a screen's
// test proves that screen renders — not that the arithmetic is right. The
// failures that matter are the ones a test asserting rendered text cannot see:
// a cursor that gets truncated, a filter that survives a cursor change, a page
// number that creeps in because a developer wanted a "page 2" link.
import { describe, expect, it } from "vitest";
import { createMemoryHistory, createRouter, type Router } from "vue-router";

import { pagerFor, routeTo, listQuery, type PagedRead, type QueryShape } from "./query-state";

const USERS: QueryShape = { filters: ["state"] };

function routerAt(path: string): Router {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [{ path: "/:p(.*)*", component: { template: "<div />" } }],
  });
  void router.push(path);
  return router;
}

function pageOf(next_cursor: string, has_more: boolean): PagedRead {
  return { next_cursor, has_more };
}

describe("pagerFor", () => {
  it("offers Next only when the server says there is more", () => {
    const more = pagerFor(pageOf("c-2", true), {}, USERS);
    expect(more.map((link) => link.label)).toEqual(["Next"]);

    // `has_more` and not the page's length is the only honest end-of-collection
    // signal, because a short page is also what a concurrent write produces.
    const end = pagerFor(pageOf("c-2", false), {}, USERS);
    expect(end).toEqual([]);
  });

  it("sends the server's own next_cursor back, byte for byte", () => {
    // A cursor is OPAQUE. A base64url cursor contains characters that a naive
    // "encode it a bit" implementation would eat, and the server is the only
    // thing that can place one.
    const opaque = "Y3Vyc29yOjEyMw==?a=b&c";
    const [next] = pagerFor(pageOf(opaque, true), {}, USERS);

    expect(next?.to).toBe(`?after=${encodeURIComponent(opaque)}`);
    // And the console never decodes it: round-tripping the value it puts in the
    // URL must give back the string the server sent.
    const from = new URLSearchParams(next?.to.slice(1)).get("after");
    expect(from).toBe(opaque);
  });

  it("marks the first page current and never numbers any position", () => {
    const [next] = pagerFor(pageOf("c-2", true), {}, USERS);
    expect(next?.current).toBe(true);
    expect(next?.label).not.toMatch(/\d/);

    const second = pagerFor(pageOf("c-3", true), { after: "c-2" }, USERS);
    expect(second.map((link) => link.label)).toEqual(["First", "Next"]);
    // "First" is the position the console CAN name, because it is the absence of
    // a cursor rather than a cursor it would have to invent.
    expect(second[0].to).toBe("");
    expect(second[0].current).toBeUndefined();
    // Neither link on a later page is "current": the position the reader is on
    // is the one they arrived at, and a keyset cursor cannot be named. Marking
    // "First" current here would tell a screen reader the reader is on page one
    // when they have already moved on.
    expect(second[1].current).toBe(false);
  });

  it("carries the declared filters through a page change and drops the rest", () => {
    const [next] = pagerFor(pageOf("c-2", true), { state: "active", utm_source: "news" }, USERS);
    expect(next?.to).toBe("?state=active&after=c-2");

    // A parameter the screen never declared cannot ride along into a URL the
    // operator shares, which is where rule 2's "no sensitive data in a URL"
    // becomes mechanical rather than a habit.
    expect(next?.to).not.toContain("utm_source");
  });

  it("renders no pager at all before a page has arrived", () => {
    expect(pagerFor(undefined, {}, USERS)).toEqual([]);
  });
});

describe("routeTo", () => {
  it("omits the cursor key entirely for the first page", () => {
    // `?after=` is a cursor the server has to reason about; "the first page" is
    // the honest way to say there is no cursor, and it is what a shared link
    // from the first page looks like.
    expect(routeTo(undefined, { after: "c-2", state: "active" }, USERS)).toBe("?state=active");
    expect(routeTo(undefined, {}, USERS)).toBe("");
  });
});

describe("listQuery", () => {
  it("sends only a filter whose value is in the contract's vocabulary", () => {
    // The URL is user input. A hand-edited `?state=not-a-state` must read as "no
    // filter" rather than becoming a 400 on a link the operator was handed.
    const vocabulary = { state: ["invited", "active", "removed"] as const };
    expect(listQuery({ state: "active" }, vocabulary)).toEqual({ state: "active" });
    expect(listQuery({ state: "not-a-state" }, vocabulary)).toEqual({});
    expect(listQuery({ state: "" }, vocabulary)).toEqual({});
    // An array-valued parameter is not a single value and is dropped rather than
    // silently taking its first entry.
    expect(listQuery({ state: ["active", "removed"] }, vocabulary)).toEqual({});
  });

  it("carries the cursor through to the operation untouched", async () => {
    const router = routerAt("/identity?after=c-9");
    await router.isReady();
    expect(router.currentRoute.value.query.after).toBe("c-9");
  });
});
