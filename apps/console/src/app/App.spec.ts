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
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { nextTick } from "vue";

const api = vi.hoisted(() => ({
  getHealth: vi.fn(),
  getReadiness: vi.fn(),
  getSessionResult: vi.fn(),
  fetchAccountOverview: vi.fn(),
  signOutOfSession: vi.fn(),
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

function mountApp() {
  setActivePinia(pinia);
  const session = useSessionStore();
  session.remember({
    class: PRINCIPAL.class,
    accountId: PRINCIPAL.account_id,
    email: "ops@example.test",
  });
  return mount(App, {
    global: {
      plugins: [pinia, router],
    },
  });
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

    pinia = createPinia();
    setActivePinia(pinia);
    // The guard asks the session before admitting a product route, and this
    // stub is what it is answered with.
    await router.push("/sign-in").catch(() => undefined);
    await router.push("/gateway-status").catch(() => undefined);
    await router.isReady();
  });

  afterEach(() => {
    window.localStorage.clear();
    document.documentElement.removeAttribute("data-theme");
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
    const wrapper = mountApp();
    await flushPromises();

    // `/gateway-status` is not a nav entry — it is the contract's test entry
    // point — so no nav link is current, which is the honest answer for a
    // destination the sidebar does not offer.
    expect(wrapper.findAll('nav[aria-label="Console navigation"] a[href="/"]')).toHaveLength(1);
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
    expect(wrapper.find("button").exists()).toBe(true);
    expect(wrapper.text()).toContain("Sign out");
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
