// The page reaches the API only through lib/api, which re-exports the
// generated contract package. Mocking that boundary lets this test pin UI
// behavior without mirroring any OpenAPI shape in a Vue fixture.
import { flushPromises, mount } from "@vue/test-utils";
import { beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  getHealth: vi.fn(),
  getReadiness: vi.fn(),
}));

vi.mock("@/lib/api", () => api);

import GatewayStatusPage from "./GatewayStatusPage.vue";
import { probeStatusVariant } from "@/lib/probe-status";

describe("GatewayStatusPage", () => {
  beforeEach(() => {
    api.getHealth.mockReset();
    api.getReadiness.mockReset();
  });

  it("renders successful generated-contract probe responses", async () => {
    api.getHealth.mockResolvedValue({ data: { status: "ok" } });
    api.getReadiness.mockResolvedValue({ data: { status: "ok" } });

    const wrapper = mount(GatewayStatusPage);
    await flushPromises();

    expect(api.getHealth).toHaveBeenCalledOnce();
    expect(api.getReadiness).toHaveBeenCalledOnce();
    expect(wrapper.text()).toContain("Liveness");
    expect(wrapper.text()).toContain("Readiness");
    // Badge internals are Loom's implementation detail; the public behavior
    // is that both generated probe results are visible to a console user.
    expect(wrapper.text().match(/ok/g)).toHaveLength(2);
  });

  it("renders an error state when the readiness probe fails", async () => {
    api.getHealth.mockResolvedValue({ data: { status: "ok" } });
    api.getReadiness.mockResolvedValue({ error: new Error("unavailable") });

    const wrapper = mount(GatewayStatusPage);
    await flushPromises();

    expect(wrapper.text()).toContain("Probe unavailable");
  });

  it("maps only a confirmed healthy probe status to the success variant", () => {
    expect(probeStatusVariant("ok")).toBe("success");
    // The contract's status is a plain string: any other value it reports —
    // today or after the contract grows one — must not wear the success
    // color, and neither may a probe that never reported.
    expect(probeStatusVariant("degraded")).toBe("destructive");
    expect(probeStatusVariant(undefined)).toBe("destructive");
  });
});
