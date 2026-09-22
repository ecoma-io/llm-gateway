// The shell test intentionally mounts the real Loom components. It proves
// their public package surface works inside this consumer, including an actual
// Button interaction that changes Loom's public theme state — not a local
// copy or a component stub.
import { createPinia } from "pinia";
import { flushPromises, mount } from "@vue/test-utils";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { nextTick } from "vue";

const api = vi.hoisted(() => ({
  getHealth: vi.fn(),
  getReadiness: vi.fn(),
}));

vi.mock("@/lib/api", () => api);

import App from "./App.vue";
import router from "@/router";

function mountApp() {
  return mount(App, {
    global: {
      plugins: [createPinia(), router],
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
    await router.push("/");
    await router.isReady();
  });

  afterEach(() => {
    window.localStorage.clear();
    document.documentElement.removeAttribute("data-theme");
  });

  it("renders the accessible shell around the gateway-status route", async () => {
    const wrapper = mountApp();
    await flushPromises();

    expect(wrapper.find('a[href="#main"]').exists()).toBe(true);
    expect(wrapper.find('nav[aria-label="Gateway navigation"]').exists()).toBe(true);
    expect(wrapper.find('[aria-current="page"]').text()).toContain("Gateway status");
    expect(wrapper.find("main#main").exists()).toBe(true);
    expect(wrapper.text()).toContain("Gateway status");
    expect(wrapper.text()).toContain("Liveness");
    expect(wrapper.text()).toContain("Readiness");
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

  it("exposes the current route through the sidebar navigation", async () => {
    const wrapper = mountApp();
    await flushPromises();

    const statusLink = wrapper.find('nav[aria-label="Gateway navigation"] a[href="/"]');
    expect(statusLink.exists()).toBe(true);
    expect(statusLink.attributes("aria-current")).toBe("page");
    expect(router.currentRoute.value.name).toBe("gateway-status");
  });
});
