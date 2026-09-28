// The shell test intentionally mounts the real Loom components. It proves
// their public package surface works inside this consumer, including an actual
// Button interaction that changes Loom's public theme state — not a local
// copy or a component stub.
//
// The route it lands on is the gateway-status screen, which is a real route
// behind the session gate like every product screen, so the API mock answers
// the session check the guard asks. The test's subject is the SHELL — the skip
// link, the header's theme control, the sidebar's current-route marking — and
// not any one screen's content, so the status page stands in as a signed-in
// destination.
import { createPinia, setActivePinia } from "pinia";
import { flushPromises, mount } from "@vue/test-utils";
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { nextTick } from "vue";

const api = vi.hoisted(() => ({
  getHealth: vi.fn(),
  getReadiness: vi.fn(),
  getSessionResult: vi.fn(),
  fetchAccountOverview: vi.fn(),
  signOutOfSession: vi.fn(),
  // The identity screen's two lists, and nothing else. They are here because
  // the marking test below needs a route that IS a nav entry, and every product
  // route is a real screen that reads the seam on mount — mounting one without
  // answering it throws rather than rendering, which would fail this file for a
  // reason that has nothing to do with the shell. Each is answered with an
  // empty page, so the screen renders quietly and the shell's chrome is the
  // only thing the assertions are about.
  fetchUsers: vi.fn(),
  fetchApiKeys: vi.fn(),
}));

vi.mock("@/lib/api", () => api);

import App from "./App.vue";
import router from "@/router";
import { useSessionStore } from "@/stores/session";
import type { Principal } from "@ecoma-io/llm-gateway-console-api-client";

const PRINCIPAL: Principal = {
  class: "user",
  account_id: "a0000000-0000-4000-8000-0000000000a1",
  user_id: "u0000000-0000-4000-8000-0000000000u1",
  email: "ops@example.test",
};

/**
 * One pinia per test, created and made active before the store is touched.
 *
 * The guard and the shell both read the session store, and Pinia resolves a
 * store against the ACTIVE instance — so a test that reads the store before
 * mounting has to have set one up first, or the guard throws on a navigation
 * that has nothing to do with what the test is checking.
 */
let pinia: ReturnType<typeof createPinia>;

/** Every `App` this file mounted, torn down after each test. See `mountApp`. */
const wrappers: ReturnType<typeof mount>[] = [];

function mountApp() {
  setActivePinia(pinia);
  const session = useSessionStore();
  session.remember({
    class: PRINCIPAL.class,
    accountId: PRINCIPAL.account_id,
    email: "ops@example.test",
  });
  const wrapper = mount(App, {
    global: {
      plugins: [pinia, router],
    },
  });
  // Every mount in this file is registered, because the file shares one
  // module-level router: a test that leaves its `App` mounted keeps a component
  // tree attached to the shared route table, and the next test's mount then
  // either renders into a document that is being torn down or finds a `null`
  // root. That is a leak between tests, not a defect in the shell, and it
  // shows up as failures in tests that never touched the router.
  wrappers.push(wrapper);
  return wrapper;
}

/**
 * The shell, mounted on a NAMED route.
 *
 * The file shares one module-level router across its tests, so `mountApp`
 * alone mounts whatever route the last navigation left behind. A test whose
 * subject is which screen the shell thinks the reader is on cannot leave that
 * to the previous test's `beforeEach` — the marking is computed from
 * `route.path`, so the route has to be stated where the assertion is.
 *
 * The navigation goes through the real guard, which is the point: a route the
 * guard refuses would land on sign-in, and the test would then be asserting
 * about the sign-in screen while believing it had mounted `/identity`.
 */
async function mountAt(path: string) {
  await router.push(path);
  await router.isReady();
  const wrapper = mountApp();
  // The screen behind the route reads the seam on mount and the shell's
  // `active` flags are a computed over `route.path`, so the assertion needs the
  // route committed AND the screen settled. Reading either one early is a test
  // that asserts about the previous route.
  await flushPromises();
  return wrapper;
}

/**
 * The sign-out control, found by what it SAYS rather than by a test id.
 *
 * `:has-text()` is Testing Library's and not available through a `VueWrapper`,
 * so this walks the shell's buttons and takes the one whose own text is the
 * label. A selector that could only ever match the one button would prove
 * nothing about which button the click lands on.
 */
function signOutButton(wrapper: ReturnType<typeof mountApp>) {
  const button = wrapper.findAll("button").find((candidate) => candidate.text() === "Sign out");
  if (!button) throw new Error("the shell rendered no Sign out button");
  return button;
}

describe("application shell", () => {
  beforeAll(() => {
    // Loom's `SegmentedControl` sizes its selection indicator on mount and
    // jsdom ships no `ResizeObserver`. The identity screen renders one, so a
    // test that mounts a real nav route — which is what the current-route
    // marking is about — cannot mount at all without this. Stubbed rather than
    // polyfilled for the same reason `IdentityPage.spec.ts` stubs it: the
    // indicator's geometry is not what this file is about, and this file is
    // about the shell.
    class NoopResizeObserver {
      observe(): void {}
      unobserve(): void {}
      disconnect(): void {}
    }
    vi.stubGlobal("ResizeObserver", NoopResizeObserver);
  });

  beforeEach(async () => {
    // Theme's state is deliberately shared by Loom for an application's
    // lifetime. The public consumer API has no reset hook, so clear its
    // supported persistence/DOM surfaces rather than reaching into internals.
    window.localStorage.clear();
    document.documentElement.removeAttribute("data-theme");
    api.getHealth.mockResolvedValue({ data: { status: "ok" } });
    api.getReadiness.mockResolvedValue({ data: { status: "ok" } });
    api.getSessionResult.mockResolvedValue({ ok: true, data: PRINCIPAL });
    // The session ends idempotently, so a resolved `null` is what the server
    // sends for its 204 and what the client turns the empty body into.
    api.signOutOfSession.mockResolvedValue({ ok: true, data: null });
    // The shell test is about the chrome, not the dashboard's figures; the
    // dashboard's own spec owns what it renders.
    api.fetchAccountOverview.mockResolvedValue({
      ok: true,
      data: {
        account: {
          id: PRINCIPAL.account_id,
          name: "Northwind",
          state: "active",
          created_at: "2026-01-01T00:00:00Z",
          updated_at: "2026-01-01T00:00:00Z",
        },
      },
    });
    const emptyPage = (items: readonly unknown[]) => ({
      ok: true,
      data: { items, next_cursor: "c-2", has_more: false },
    });
    api.fetchUsers.mockResolvedValue(emptyPage([]));
    api.fetchApiKeys.mockResolvedValue(emptyPage([]));

    pinia = createPinia();
    setActivePinia(pinia);
    // The guard asks the session before admitting a product route, and this
    // stub is what it is answered with.
    await router.push("/sign-in").catch(() => undefined);
    await router.push("/gateway-status").catch(() => undefined);
    await router.isReady();
  });

  afterEach(() => {
    // Teardown first: an `App` still mounted when the next test's router
    // navigation commits is a component tree rendering into a document the
    // runner is already tearing down.
    for (const wrapper of wrappers.splice(0)) wrapper.unmount();
    window.localStorage.clear();
    document.documentElement.removeAttribute("data-theme");
  });

  afterAll(() => {
    // A global stub outlives the file unless it is taken back, and this file's
    // last act is to leave the environment as it found it.
    vi.unstubAllGlobals();
  });

  it("renders the accessible shell around the gateway-status route", async () => {
    const wrapper = mountApp();
    await flushPromises();

    // The skip link targets the `main` landmark and the main is focusable, so a
    // keyboard reader can actually move past the shell chrome.
    expect(wrapper.find('a[href="#main"]').exists()).toBe(true);
    expect(wrapper.find("main#main").attributes("tabindex")).toBe("-1");
    expect(wrapper.find('nav[aria-label="Console navigation"]').exists()).toBe(true);
    expect(wrapper.text()).toContain("Liveness");
    expect(wrapper.text()).toContain("Readiness");
  });

  it("marks the current screen in the sidebar navigation", async () => {
    // The marking is `aria-current="page"`, which Loom's `SidebarNav` puts on
    // the link whose `active` the host declared. Asserting on it rather than on
    // the presence of a link is what makes this a test: the previous version
    // asserted only that one link pointed at `/`, and setting
    // `AppLayout.vue`'s `active: route.path === "/"` to `active: false` left it
    // green — a test that could not tell a marked link from an unmarked one.
    const wrapper = await mountAt("/");
    const current = wrapper.findAll('nav[aria-label="Console navigation"] a[aria-current="page"]');
    // Exactly one, and it is the Dashboard — a link the reader is on marked
    // while every other destination is not.
    expect(current).toHaveLength(1);
    expect(current[0]?.text()).toContain("Dashboard");
    expect(current[0]?.attributes("href")).toBe("/");

    // Exactly ONE because the marking is a position, not a section: if the
    // comparison were ever a prefix or a `startsWith`, two links would light up
    // at once and a reader would be told they are on two screens.
    const others = wrapper
      .findAll('nav[aria-label="Console navigation"] a')
      .filter((link) => link.attributes("href") !== "/");
    for (const link of others) {
      expect(link.attributes("aria-current"), link.text()).toBeUndefined();
    }
  });

  it("moves the marking with the route, and leaves none on a screen it does not offer", async () => {
    // The same rule seen from the other two directions, because a marking that
    // is computed once and never recomputed passes the test above on the
    // landing route and is wrong everywhere else.
    const onIdentity = await mountAt("/identity");
    const identityCurrent = onIdentity
      .findAll('nav[aria-label="Console navigation"] a[aria-current="page"]')
      .map((link) => link.attributes("href"));
    expect(identityCurrent).toEqual(["/identity"]);

    // `/gateway-status` is not a nav entry — it is the contract's test entry
    // point — so no nav link is current, which is the honest answer for a
    // destination the sidebar does not offer. Asserting the negative is the
    // other half: a sidebar that fell back to marking the first link, or to
    // marking the nearest by prefix, would leave one lit here.
    const onStatus = await mountAt("/gateway-status");
    expect(
      onStatus.findAll('nav[aria-label="Console navigation"] a[aria-current="page"]'),
    ).toHaveLength(0);
    // And the dashboard link is still on the page, unmarked — the negative
    // above is "nothing is current", not "the sidebar went missing".
    expect(onStatus.find('nav[aria-label="Console navigation"] a[href="/"]').exists()).toBe(true);
  });

  it("names every product screen in the sidebar", async () => {
    const wrapper = mountApp();
    await flushPromises();

    const labels = wrapper
      .findAll('nav[aria-label="Console navigation"] a')
      .map((link) => link.text());
    for (const screen of [
      "Dashboard",
      "Identity",
      "Catalog",
      "Commerce",
      "Accounting",
      "Reconciliation",
    ]) {
      expect(labels).toContain(screen);
    }
  });

  it("shows who is signed in and offers a way out", async () => {
    const wrapper = mountApp();
    await flushPromises();

    // The address is the signed-in person's own and is a label, never a lookup
    // key and never in a URL.
    expect(wrapper.text()).toContain("Signed in as ops@example.test");
    // A way out is a CONTROL, not a sentence. The assertion used to be
    // `expect(wrapper.find("button").exists()).toBe(true)`, which the header's
    // three theme buttons satisfy on their own — it held for a shell with no
    // sign-out control at all. Found by what the button SAYS, the same
    // selection the click test below uses, so this test and that one can never
    // disagree about which control is the one under test.
    expect(signOutButton(wrapper).attributes("type")).toBe("button");
  });

  it("signs the visitor out on a click, which the one above never proved", async () => {
    // "Sign out" appearing is not a way out. This clicks the control, which is
    // the only thing that proves the session is ended server-side and the
    // console is no longer wearing a signed-in shell: a button that renders the
    // words and does nothing is the failure ADR 0012 §2 names, and asserting on
    // the text is how one ships.
    const wrapper = mountApp();
    await flushPromises();
    setActivePinia(pinia);
    expect(useSessionStore().signedIn).toBe(true);

    await signOutButton(wrapper).trigger("click");
    await flushPromises();

    expect(api.signOutOfSession).toHaveBeenCalledTimes(1);
    expect(useSessionStore().signedIn).toBe(false);
    expect(router.currentRoute.value.path).toBe("/sign-in");
  });

  it("drops the local session even when the sign-out call fails", async () => {
    // The server's call is idempotent, so this is safe to run from a button and
    // from a 401 handler alike — and the local state is cleared either way,
    // because a console left wearing a signed-in shell after the sign-out failed
    // is the defect this one rule exists to prevent.
    api.signOutOfSession.mockResolvedValue({
      ok: false,
      failure: { kind: "transport", error: new TypeError() },
    });
    const wrapper = mountApp();
    await flushPromises();
    setActivePinia(pinia);

    await signOutButton(wrapper).trigger("click");
    await flushPromises();

    expect(useSessionStore().signedIn).toBe(false);
    expect(router.currentRoute.value.path).toBe("/sign-in");
  });

  it("selects and persists a dark theme through a Loom Button interaction", async () => {
    const wrapper = mountApp();
    await flushPromises();

    // Loom holds the theme preference at module scope for the process
    // lifetime and persists it only on change, so clearing localStorage in
    // beforeEach cannot reset it. Each test establishes its starting
    // preference through a public interaction instead of assuming
    // fresh-module state, keeping every test runnable on its own.
    await wrapper.find('button[aria-label="System theme"]').trigger("click");
    await nextTick();
    await wrapper.find('button[aria-label="Dark theme"]').trigger("click");
    await nextTick();

    expect(document.documentElement.dataset.theme).toBe("dark");
    expect(window.localStorage.getItem("loom:theme")).toBe("dark");
    expect(wrapper.find('button[aria-label="Dark theme"]').attributes("aria-pressed")).toBe("true");
  });

  it("selects system theme preference through the Loom theme control", async () => {
    const wrapper = mountApp();
    await flushPromises();

    // Same isolation rule as the dark test: force a real change away from
    // "system" first, so the click under test is always a persisted change.
    await wrapper.find('button[aria-label="Dark theme"]').trigger("click");
    await nextTick();
    await wrapper.find('button[aria-label="System theme"]').trigger("click");
    await nextTick();

    expect(window.localStorage.getItem("loom:theme")).toBe("system");
    expect(wrapper.find('button[aria-label="System theme"]').attributes("aria-pressed")).toBe(
      "true",
    );
  });
});
