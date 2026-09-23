// jsdom provides the history API vue-router needs to push and resolve.
import { describe, expect, it } from "vitest";

import router from "./index";

describe("router", () => {
  it("initializes the gateway-status route at /", async () => {
    await router.push("/");

    expect(router.currentRoute.value.name).toBe("gateway-status");
    expect(router.resolve("/").matched).toHaveLength(1);
    expect(router.resolve("/").href).toBe("/");
  });
});

// There is one page because the API itself has only two infrastructure probes
// today. More routes arrive with their designed contract domain — not as empty
// navigation residue beside the shell.
