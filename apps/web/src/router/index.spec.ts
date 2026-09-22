// jsdom provides the history API vue-router needs to push and resolve.
import { describe, expect, it } from "vitest";

import router from "./index";

describe("router", () => {
  it("resolves / to the home route", async () => {
    await router.push("/");

    expect(router.currentRoute.value.name).toBe("home");
  });
});
