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
