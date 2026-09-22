import { createPinia } from "pinia";
import { mount } from "@vue/test-utils";
import { describe, expect, it } from "vitest";

import HomeView from "./HomeView.vue";

describe("HomeView", () => {
  it("renders the scaffold placeholder", () => {
    const wrapper = mount(HomeView, {
      global: {
        plugins: [createPinia()],
      },
    });

    expect(wrapper.text()).toContain("Gateway scaffold — the console is not built yet.");
  });

  it("clicking the button increments the displayed count", async () => {
    const wrapper = mount(HomeView, {
      global: {
        plugins: [createPinia()],
      },
    });

    expect(wrapper.find('[data-testid="count"]').text()).toBe("0");

    await wrapper.find("button").trigger("click");
    expect(wrapper.find('[data-testid="count"]').text()).toBe("1");

    await wrapper.find("button").trigger("click");
    expect(wrapper.find('[data-testid="count"]').text()).toBe("2");
  });
});
