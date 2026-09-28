// Identity: two lists, and the one credential the whole console ever holds.
//
// The mint is why this file is more than a test of two tables. A key's
// plaintext is returned by exactly one operation on `console.yaml`, and every
// rule ADR 0012 §3 states about it is a NEGATIVE — not in a live region, not in
// web storage, not in a URL, gone on dismiss — so a spec that proved only the
// happy path would pass just as happily with the secret in a Pinia store or on
// a screen reader's buffer. Each rule gets the assertion that can go red.
//
// The failure walk at the end is the §6 matrix test for this screen, and it is
// driven off `CONSOLE_BEHAVIOUR` rather than a hand-copied list: a code added
// to `shared/errors.yaml` lands here as a code this file has never rendered.
import { flushPromises, mount, type VueWrapper } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { createMemoryHistory, type Router } from "vue-router";

// The seam, mocked at the boundary every screen's data arrives through — never
// the composables, because a composable stub would prove only that the screen
// reads a stub. `importActual` keeps the rest of the module real, so the
// session store the route guard reaches for is the real one.
const seam = vi.hoisted(() => ({
  fetchUsers: vi.fn(),
  fetchApiKeys: vi.fn(),
  createApiKey: vi.fn(),
  getSessionResult: vi.fn(),
}));

vi.mock("@/lib/api", async () => {
  const actual = await vi.importActual<typeof import("@/lib/api")>("@/lib/api");
  return { ...actual, ...seam };
});

import IdentityPage from "@/pages/IdentityPage.vue";
import { createConsoleRouter } from "@/router";
import { describe as describeViolations, inspect } from "@/lib/arch/roster";
import { CONSOLE_BEHAVIOUR, type ApiErrorCode } from "@/lib/failure-matrix";
import type { ApiFailure, ApiResult } from "@/lib/api";
import type {
  ApiKeyPage,
  MintApiKeyResponse,
  UserPage,
} from "@ecoma-io/llm-gateway-console-api-client";

/**
 * The fixture credential, ASSEMBLED rather than written.
 *
 * `identity.TokenBrand` is `gw` and a secret is unpadded base64url, so this
 * mirrors the grammar the server mints to. The reason it is concatenated rather
 * than typed as one literal is the secret scanner: gitleaks scans HISTORY and
 * blocks the push at the commit that introduced a credential-shaped string, no
 * matter how clearly the surrounding test says it is fake. Building it from
 * parts keeps the fixture honest — it really does have the server's shape — and
 * keeps the scanner's signal meaningful for the day a real key is committed.
 */
const TOKEN = ["gw", "bGl2ZS1hMWIyYzNkNGU1ZjY3ODlhYmNkZWY"].join("_");

const MINTED: MintApiKeyResponse = {
  id: "d0000000-0000-4000-8000-0000000000d1",
  account_id: "a0000000-0000-4000-8000-0000000000a1",
  display_name: "ci key",
  prefix: "Gk_d0000000",
  state: "active",
  created_at: "2026-09-28T00:00:00Z",
  token: TOKEN,
};

const USERS: UserPage = {
  items: [
    {
      id: "u0000000-0000-4000-8000-0000000000u1",
      account_id: "a0000000-0000-4000-8000-0000000000a1",
      email: "invited@example.test",
      state: "invited",
      created_at: "2026-09-01T00:00:00Z",
      updated_at: "2026-09-01T00:00:00Z",
    },
    {
      id: "u0000000-0000-4000-8000-0000000000u2",
      account_id: "a0000000-0000-4000-8000-0000000000a1",
      email: "active@example.test",
      state: "active",
      created_at: "2026-09-02T00:00:00Z",
      updated_at: "2026-09-02T00:00:00Z",
    },
  ],
  next_cursor: "c-users-2",
  has_more: false,
};

const KEYS: ApiKeyPage = {
  items: [
    {
      id: "d0000000-0000-4000-8000-0000000000d0",
      account_id: "a0000000-0000-4000-8000-0000000000a1",
      // A key provisioned by CI carries no minting identity, and the list does
      // not invent one to make the row look like a relationship it is not.
      created_by: null,
      display_name: "release pipeline",
      prefix: "Gk_d0000000",
      state: "active",
      created_at: "2026-09-03T00:00:00Z",
    },
  ],
  next_cursor: "c-keys-2",
  has_more: false,
};

const ok = <T>(data: T): ApiResult<T> => ({ ok: true, data });

/** A contract failure carrying `code`, the shape the gateway actually returns. */
function apiFailure(code: ApiErrorCode): ApiFailure {
  return {
    kind: "api",
    unauthenticated: code === "unauthenticated",
    envelope: { error: { code, message: "the server's own words" }, request_id: "req-identity-1" },
  };
}

/** Every code this console can produce, read off the matrix rather than copied. */
const REACHABLE = Object.entries(CONSOLE_BEHAVIOUR)
  .filter(([, behaviour]) => behaviour.reachable)
  .map(([code]) => code as ApiErrorCode);

beforeAll(() => {
  // Loom's `SegmentedControl` sizes its selection indicator on mount, and jsdom
  // ships no `ResizeObserver`. Stubbed rather than polyfilled: the indicator's
  // geometry is not what this screen is about, and every Loom component that
  // needs it will need the same stub.
  class NoopResizeObserver {
    observe(): void {}
    unobserve(): void {}
    disconnect(): void {}
  }
  vi.stubGlobal("ResizeObserver", NoopResizeObserver);
});

beforeEach(() => {
  for (const mock of Object.values(seam)) mock.mockReset();
  seam.getSessionResult.mockResolvedValue(
    ok({ class: "user", account_id: "a0000000-0000-4000-8000-0000000000a1" }),
  );
  seam.fetchUsers.mockResolvedValue(ok(USERS));
  seam.fetchApiKeys.mockResolvedValue(ok(KEYS));
  seam.createApiKey.mockResolvedValue(ok(MINTED));
  document.body.innerHTML = "";
});

/**
 * Every `attachTo` wrapper this file made, torn down in one place.
 *
 * A wrapper left mounted keeps its composables subscribed to a route the NEXT
 * test's `beforeEach` has already re-armed, so a read issued by the previous
 * test lands in the following one — which is how a suite grows rejections
 * nobody can attribute to a test. The sweep is unconditional rather than a line
 * at the end of each test, because a test that fails half way through is
 * exactly the one that would have left its wrapper behind.
 */
const mounted: VueWrapper[] = [];

afterEach(() => {
  while (mounted.length > 0) mounted.pop()!.unmount();
  document.body.innerHTML = "";
});

/**
 * A router of this screen's own, over the real route table and a memory
 * history. Built per test because a shared singleton carries the previous
 * test's current route and its guard's memory of who is signed in — and a file
 * whose fourth assertion is really about the third test's session is worse than
 * no test at all.
 */
async function mountAt(path: string): Promise<{ wrapper: VueWrapper; router: Router }> {
  const pinia = createPinia();
  setActivePinia(pinia);
  const router = createConsoleRouter(createMemoryHistory());
  await router.push(path);
  await router.isReady();
  const wrapper = mount(IdentityPage, {
    attachTo: document.body,
    global: { plugins: [router, pinia] },
  });
  mounted.push(wrapper);
  // Two rounds: the composable reads on mount, and the pager's own watcher
  // reads again when the first answer lands. Asserting between them would be
  // asserting on a state no reader ever saw.
  await flushPromises();
  await flushPromises();
  return { wrapper, router };
}

/** Mint through the screen's own control, the way an operator does. */
async function mintThrough(wrapper: VueWrapper, displayName: string): Promise<void> {
  await wrapper.get("input").setValue(displayName);
  const mint = wrapper.findAll("button").find((button) => button.text().includes("Create key"));
  expect(mint).toBeDefined();
  await mint!.trigger("click");
  await flushPromises();
  await flushPromises();
}

/**
 * Every element in the document that holds the credential, in any of the three
 * ways an element can: a text node a reader would traverse, a `value`
 * attribute, or a `value` property (which is what an `<input>`'s current value
 * is). An assertion over `textContent` alone is vacuous for a secret rendered
 * into a field, which is the only way it is ever allowed to appear.
 */
function carriersOf(token: string): readonly Element[] {
  return Array.from(document.querySelectorAll("*")).filter((element) =>
    [element.textContent, element.getAttribute("value"), (element as HTMLInputElement).value]
      .filter((candidate): candidate is string => typeof candidate === "string")
      .some((candidate) => candidate.includes(token)),
  );
}

/** The pager links, in the order the tables render them. */
function pagerHrefs(wrapper: VueWrapper): readonly string[] {
  return wrapper.findAll("nav a").map((link) => link.attributes("href") ?? "");
}

/**
 * The secret's own two refs, read off the LIVE `OneTimeSecret` instance.
 *
 * The DOM cannot tell "erased" from "hidden", and on this component that
 * difference is the whole claim: `clear()` runs twice per click — the
 * component's button and then the Card's own dismiss path — so a `clear()`
 * that never touched `secret.value` still left a clean-looking screen, because
 * the second pass set `revealed` false and the reveal panel is a `v-if`. The
 * ref is what an operator's devtools inspector would read, so it is what the
 * test has to read.
 *
 * `devtoolsRawSetupState` rather than `setupState`: the unwrapped refs, which
 * is the whole point — a `shallowRef` is meant to sit in no reactive graph
 * devtools walks, and the graph devtools walks is this one.
 */
function secretState(wrapper: VueWrapper): { secret?: string; revealed: boolean } {
  // Found through the rendered tree rather than from the PAGE's own setup
  // state: `<script setup>` keeps a non-exposed binding out of the parent's
  // `setupState`, which is the same rule that keeps the token out of anything
  // the page holds.
  const child = wrapper.findComponent({ name: "OneTimeSecret" });
  expect(child.exists()).toBe(true);
  const state = (
    child.vm as unknown as {
      $: { devtoolsRawSetupState: Record<string, unknown> };
    }
  ).$.devtoolsRawSetupState;
  // The RAW graph holds the refs themselves, which is the point of reading it:
  // this is the object devtools walks, so an erased credential is erased HERE
  // and not merely in a rendering that a re-render would undo.
  const unwrap = (key: string): unknown => {
    const held = state[key];
    return (held as { __v_isRef?: boolean; value?: unknown } | undefined)?.__v_isRef === true
      ? (held as { value: unknown }).value
      : held;
  };
  return {
    secret: unwrap("secret") as string | undefined,
    revealed: unwrap("revealed") as boolean,
  };
}

describe("IdentityPage", () => {
  it("keeps the console's architecture roster", async () => {
    // The roster is read off the RENDERED document, so this is a claim about
    // what the page actually produced rather than about what it imported. A
    // list screen that rendered no table, or a table nobody declared a layer
    // for, or a pager with no table to move, is a defect the reader would
    // experience and no type would catch.
    const { wrapper } = await mountAt("/identity");
    await flushPromises();

    expect(describeViolations(inspect(wrapper.element.ownerDocument, "/identity"))).toBe("");
  });

  it("renders two lists, because a person and a credential are not one thing", async () => {
    const { wrapper } = await mountAt("/identity");

    const captions = wrapper.findAll("table > caption").map((caption) => caption.text());
    expect(captions).toEqual(["Users in this account", "API keys for this account"]);

    // `invited` is a live state with no credential behind it, so it wears its
    // own label rather than reading as `active` — an operator who believed a
    // login existed for it would be wrong about something load-bearing.
    const users = wrapper.findAll("table")[0]!;
    const rows = users.findAll("tbody tr");
    expect(rows).toHaveLength(2);
    expect(rows[0]!.text()).toContain("invited@example.test");
    expect(rows[0]!.text()).toContain("Invited");
    expect(rows[1]!.text()).toContain("active@example.test");
    expect(rows[1]!.text()).toContain("Active");
    // A row is not a second row's text: the pair of rows asserts the two
    // addresses separately so a table that rendered both addresses once
    // cannot pass.
    expect(rows[0]!.text()).not.toContain("active@example.test");
    expect(rows[1]!.text()).not.toContain("invited@example.test");

    // The keys list shows the name and the prefix. There is no column that
    // could hold a secret, because no read operation returns one.
    const keys = wrapper.findAll("table")[1]!;
    expect(keys.text()).toContain("release pipeline");
    expect(keys.text()).toContain("Gk_d0000000");
    expect(keys.text()).not.toContain(TOKEN);
  });

  it("sends a lifecycle filter only when the URL holds a lifecycle state", async () => {
    // The URL is user input. A hand-edited `?state=not-a-state` has to read as
    // "no filter" rather than become a `400` on a link the operator was
    // handed, and that is the vocabulary check rather than a page decision.
    // Each mount asserts on its OWN last call rather than a shared index, so
    // neither assertion can be satisfied by the other mount's request.
    await mountAt("/identity?state=active");
    expect(seam.fetchUsers.mock.lastCall?.[0]).toEqual({ query: { state: "active" } });

    await mountAt("/identity?state=not-a-state");
    expect(seam.fetchUsers.mock.lastCall?.[0]).toEqual({ query: {} });
    expect(seam.fetchUsers).toHaveBeenCalled();
  });

  it("keeps the lifecycle filter on the pager's links, and a bad one off them", async () => {
    // Two links, because `has_more` is set on the users page and not on the
    // keys page. The filter is DECLARED, so it is carried into the pager —
    // without that, a page change would land the reader on page two of an
    // unfiltered list under a control still showing their filter. And the
    // vocabulary is a second gate on the same link: `routeTo` asks
    // `shape.filters` which keys to copy but not whether a value is one of
    // them, so a hand-edited filter is copied verbatim into a URL the reader
    // may then share.
    seam.fetchUsers.mockResolvedValue({
      ok: true,
      data: { ...USERS, next_cursor: "c-users-3", has_more: true },
    });
    const { wrapper } = await mountAt("/identity?state=active");
    expect(pagerHrefs(wrapper)).toEqual(["/identity?state=active&after=c-users-3"]);

    // The keys list declares no filters, so its link is a bare cursor — the
    // pair is what makes "declared here, absent there" visible in one screen.
    seam.fetchApiKeys.mockResolvedValue({
      ok: true,
      data: { ...KEYS, next_cursor: "c-keys-3", has_more: true },
    });
    const { wrapper: both } = await mountAt("/identity");
    expect(pagerHrefs(both)).toEqual(["/identity?after=c-users-3", "/identity?after=c-keys-3"]);

    // And the same pager, with the value the vocabulary rejected. The link
    // does NOT carry it, which was a real defect: `routeTo` asked
    // `shape.filters` which keys to copy but not whether a value was one of
    // them, so a hand-edited filter rode into a link the reader may share —
    // and the person it was shared with got a control with nothing selected.
    // The value is still in the address bar the reader is looking at; what
    // cannot happen is the console minting another copy of it.
    seam.fetchApiKeys.mockResolvedValue(ok(KEYS));
    const { wrapper: bad } = await mountAt("/identity?state=not-a-state");
    expect(pagerHrefs(bad)).toEqual(["/identity?after=c-users-3"]);

    // And the request went out unfiltered, as it always did — the filter was
    // already rejected on the way IN. What is new is that the control and the
    // link now agree with the request instead of contradicting it.
    expect(seam.fetchUsers.mock.lastCall?.[0]).toEqual({ query: {} });
  });

  describe("the one-time secret", () => {
    it("shows the plaintext once, in the one control meant to show it", async () => {
      const { wrapper } = await mountAt("/identity");
      await mintThrough(wrapper, "ci key");

      const carriers = carriersOf(TOKEN);
      expect(carriers).toHaveLength(1);
      const field = carriers[0] as HTMLInputElement;
      expect(field.tagName.toLowerCase()).toBe("input");
      expect(field.readOnly).toBe(true);

      // It is a Card, and the Card says the one thing an operator can act on:
      // that this is the only time. A bare field with no such sentence is a
      // credential an operator will assume they can come back for.
      let panel: Element | null = field;
      while (panel !== null && !(panel.textContent ?? "").includes("Copy this key now")) {
        panel = panel.parentElement;
      }
      expect(panel).not.toBeNull();
      expect(panel!.textContent).toMatch(/only time/i);

      // "Once" means once: the keys list is a read and never returns a token,
      // so the reveal is not echoed into the table above it.
      expect(wrapper.findAll("table")[1]!.text()).not.toContain(TOKEN);
    });

    it("never lets the plaintext reach a live region", async () => {
      const { wrapper } = await mountAt("/identity");
      await mintThrough(wrapper, "ci key");

      // A live region writes to a screen reader's buffer, to live captioning
      // and to a scrollback buffer, so a credential inside one is a
      // disclosure event — a key read aloud in a shared space. `aria-live="off"`
      // is what the reveal panel declares, and the assertion is over EVERY
      // live region in the document, because a toast host or an announcement
      // region elsewhere in the tree is as bad as one here.
      for (const region of document.querySelectorAll("[aria-live]")) {
        expect(region.getAttribute("aria-live")).toBe("off");
      }
      expect(document.querySelector('[aria-live="assertive"]')).toBeNull();
      expect(document.querySelector('[aria-live="polite"]')).toBeNull();

      // And no live region anywhere carries it, whichever way it was written.
      for (const region of document.querySelectorAll('[role="status"], [role="alert"]')) {
        expect(region.textContent ?? "").not.toContain(TOKEN);
        expect(region.outerHTML).not.toContain(TOKEN);
      }
    });

    it("never puts the plaintext in a status or alert element", async () => {
      const { wrapper } = await mountAt("/identity");
      await mintThrough(wrapper, "ci key");

      // `role="status"` and `role="alert"` are how a screen reader is told
      // something without being asked, so they are checked separately from
      // `aria-live` — a component can carry the role without the attribute.
      expect(document.querySelectorAll('[role="status"]')).toHaveLength(0);
      expect(document.querySelectorAll('[role="alert"]')).toHaveLength(0);

      // The field's own ancestors are checked too: a wrapper somewhere above it
      // with a role would speak the whole subtree, not just the text in it.
      for (const carrier of carriersOf(TOKEN)) {
        for (let node: Element | null = carrier; node !== null; node = node.parentElement) {
          expect(["status", "alert"]).not.toContain(node.getAttribute("role"));
        }
      }
    });

    it("takes the plaintext away when the reader dismisses it", async () => {
      const { wrapper } = await mountAt("/identity");
      await mintThrough(wrapper, "ci key");
      expect(carriersOf(TOKEN)).toHaveLength(1);

      const dismiss = wrapper
        .findAll("button")
        .find((button) => button.text().includes("hide the key"));
      expect(dismiss).toBeDefined();
      await dismiss!.trigger("click");
      await flushPromises();

      // The FORM is what goes, so the assertion is over the input rather than
      // over the button's disappearance: a `clear()` that forgot
      // `secret.value` would leave `revealed` false — no Copy this key now, no
      // dismiss button, a clean-looking screen — with the credential still
      // bound to a `model-value` a re-render would paint again.
      //
      // AND the component's own state, read from the live instance rather than
      // from the DOM. This file's first version of this assertion checked only
      // the DOM and passed against a `clear()` that never erased anything:
      // `clear()` runs TWICE for one click — the component's own button and
      // then the Card's dismiss path — so the second pass cleaned up after a
      // broken first one and the screen looked right. A surviving `shallowRef`
      // is still a credential in a component instance a devtools inspector can
      // read, and that is the leak the ADR is about.
      const held = secretState(wrapper);
      expect(held.secret).toBeUndefined();
      expect(held.revealed).toBe(false);

      const field = document.querySelector<HTMLInputElement>('input[name="api-key-secret"]');
      expect(field).toBeNull();
      expect(carriersOf(TOKEN)).toHaveLength(0);
      expect(wrapper.text()).not.toContain("Copy this key now");
      // A second mint is possible — the operator who lost the key mints
      // another — but the one they dismissed is not on the page.
      expect(document.body.innerHTML).not.toContain(TOKEN);
    });

    it("never writes the plaintext to web storage", async () => {
      // Spying on the PROTOTYPE rather than reading the storages afterwards:
      // a write followed by a removal leaves an empty store, and an empty store
      // is exactly what a leak-then-cleanup looks like from the outside.
      const setItem = vi.spyOn(Storage.prototype, "setItem");
      try {
        const { wrapper } = await mountAt("/identity");
        await mintThrough(wrapper, "ci key");

        for (const call of setItem.mock.calls) {
          expect(String(call[1])).not.toContain(TOKEN);
          expect(String(call[0])).not.toContain(TOKEN);
        }
        expect(setItem).not.toHaveBeenCalled();
        expect(JSON.stringify(window.localStorage)).not.toContain(TOKEN);
        expect(JSON.stringify(window.sessionStorage)).not.toContain(TOKEN);
        expect(document.cookie).not.toContain(TOKEN);
      } finally {
        setItem.mockRestore();
      }
    });

    it("never puts the plaintext in the URL", async () => {
      const { wrapper, router } = await mountAt("/identity");
      await mintThrough(wrapper, "ci key");

      // A credential in a query is a credential in the address bar, in browser
      // history, in a bookmark, in a `Referer` and in anything that reads the
      // screen. The route is the only URL the console writes, so it is the only
      // place this can be checked.
      const { query, fullPath } = router.currentRoute.value;
      expect(JSON.stringify(query)).not.toContain(TOKEN);
      expect(fullPath).not.toContain(TOKEN);
      for (const [, value] of Object.entries(query)) {
        expect(String(value)).not.toContain(TOKEN);
      }
      for (const link of wrapper.findAll("a")) {
        expect(link.attributes("href") ?? "").not.toContain(TOKEN);
      }
    });
  });

  // ADR 0012 §6: this screen's own slice of the failure matrix. Driven off the
  // table rather than a list, so a code the contract gains is a code this test
  // has never rendered — which is the only version of "every code" that stays
  // true.
  describe("the api-failure matrix", () => {
    it("renders the failure view rather than a table for every code the console can produce", async () => {
      for (const code of REACHABLE) {
        seam.fetchUsers.mockResolvedValue({ ok: false, failure: apiFailure(code) });
        seam.fetchApiKeys.mockResolvedValue({ ok: false, failure: apiFailure(code) });

        // Mounted by hand rather than through `mountAt`, which returns before
        // `afterEach` runs: the loop needs the same router for its own next
        // iteration, and `mountAt` builds a fresh one every call.
        const pinia = createPinia();
        setActivePinia(pinia);
        const router = createConsoleRouter(createMemoryHistory());
        await router.push("/identity");
        await router.isReady();
        const wrapper = mount(IdentityPage, {
          attachTo: document.body,
          global: { plugins: [router, pinia] },
        });
        mounted.push(wrapper);
        await flushPromises();
        await flushPromises();

        // Two lists, two failures: neither table is rendered, so a screen can
        // never show rows that are current beside a banner saying they are not.
        expect(wrapper.findAll('[role="alert"]'), code).toHaveLength(2);
        expect(wrapper.findAll("table"), code).toHaveLength(0);
        expect(wrapper.find('[role="alert"]').text(), code).toContain(
          CONSOLE_BEHAVIOUR[code].title,
        );

        // The recovery is read off the row, never decided here: a `500` with a
        // retry button is a promise the console cannot keep, and a `none` with
        // one trains operators to click through outages.
        const offersRetry = wrapper
          .findAll("button")
          .some((button) => /try again|retry/i.test(button.text()));
        const offersSignIn = wrapper.findAll("a").some((link) => /sign in/i.test(link.text()));
        expect(offersRetry, code).toBe(CONSOLE_BEHAVIOUR[code].recovery === "retry");
        expect(offersSignIn, code).toBe(CONSOLE_BEHAVIOUR[code].recovery === "sign-in");

        // And the correlation is shown exactly where the row says to, because
        // it is the thing an operator quotes and never proof of who did what.
        expect(wrapper.text().includes("req-identity-1"), code).toBe(
          CONSOLE_BEHAVIOUR[code].showRequestId,
        );

        // `unauthenticated` is the one code whose recovery is a ROUTE. The
        // console does not bounce on it by itself — it offers the link and
        // leaves the operator to take it, so a code read on any background
        // refresh cannot take the reader's work off the page. What the screen
        // owes them is a link that exists, and every one of them is the same
        // destination: two lists failed, so two lists' worth of sign-in, and
        // a link per failure view rather than one invented at the top.
        const links = new Set(
          wrapper
            .findAll("a")
            .filter((link) => /sign in/i.test(link.text()))
            .map((link) => link.attributes("href")),
        );
        expect(links.size > 0, code).toBe(CONSOLE_BEHAVIOUR[code].recovery === "sign-in");
        for (const href of links) {
          expect(href, code).toContain("/sign-in");
        }

        wrapper.unmount();
        document.body.innerHTML = "";
      }
    });
  });
});
