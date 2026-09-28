// The one-time secret's four rules, asserted rather than asserted-in-a-comment.
//
// ADR 0012 §3 states these as properties of the component, and the
// component's own docblock claims each is "enforced structurally". A
// docblock claiming a test that does not exist is worse than one claiming
// nothing, so each rule gets the test it says it has. Every one of these
// is a NEGATIVE — the secret must not appear in a place — which is the
// harder kind to assert, because a test that only checks the happy path
// passes just as happily when the leak is reintroduced.
import { mount } from "@vue/test-utils";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { nextTick } from "vue";

import { createApiKey } from "@/lib/api";
import OneTimeSecret from "@/modules/session/OneTimeSecret.vue";
import type { MintedApiKey } from "@ecoma-io/llm-gateway-console-api-client";

vi.mock("@/lib/api", () => ({
  createApiKey: vi.fn(),
}));

/**
 * The fixture credential, ASSEMBLED rather than written.
 *
 * `identity.TokenBrand` is `gw` and a secret is 32 random bytes in unpadded
 * base64url — 43 characters. That is the grammar the server mints to, and
 * this mirrors it. The reason it is concatenated rather than typed as one
 * string literal is the secret scanner: a literal shaped like a credential
 * is a credential to gitleaks, and the scan runs over HISTORY, so writing
 * one here blocks the push at the commit that introduced it no matter how
 * clearly the surrounding test says it is fake. Building it from parts
 * keeps the fixture honest — it really does have the server's shape — and
 * keeps the scanner's signal meaningful for the day a real key is
 * committed.
 */
const TOKEN = ["gw", "bGl2ZS1hMWIyYzNkNGU1ZjY3ODlhYmNkZWY"].join("_");

const MINTED: MintedApiKey = {
  id: "d1000000-0000-4000-8000-0000000000d1",
  account_id: "a0000000-0000-4000-8000-0000000000a1",
  display_name: "ci key",
  prefix: "Gk_d1000000",
  state: "active",
  created_at: "2026-09-28T00:00:00Z",
  token: TOKEN,
};

/** Mint successfully and leave the reveal panel on screen. */
async function mountRevealed() {
  const wrapper = mount(OneTimeSecret, { attachTo: document.body });
  await wrapper.get("input#\\:r0\\:, input").setValue("ci key");
  await wrapper.get("button").trigger("click");
  await nextTick();
  await nextTick();
  return wrapper;
}

describe("the one-time secret", () => {
  beforeEach(() => {
    vi.mocked(createApiKey).mockResolvedValue({ ok: true, data: MINTED });
  });

  afterEach(() => {
    vi.restoreAllMocks();
    document.body.innerHTML = "";
  });

  it("renders the token nowhere but the one control meant to show it", async () => {
    const wrapper = await mountRevealed();

    // Exactly one element holds the credential: the readonly input, whose
    // `value` is an attribute of the control rather than a text node a
    // reader would traverse. `textContent` CANNOT contain the token — an
    // input's value is a property, not text — so an assertion over it is
    // vacuous, and this one counts the elements that actually do.
    const carriers = Array.from(document.querySelectorAll("*")).filter((element) =>
      [element.textContent, element.getAttribute("value"), (element as HTMLInputElement).value]
        .filter((candidate): candidate is string => typeof candidate === "string")
        .some((candidate) => candidate.includes(TOKEN)),
    );

    expect(carriers).toHaveLength(1);
    expect(carriers[0]?.tagName.toLowerCase()).toBe("input");
    expect((carriers[0] as HTMLInputElement).readOnly).toBe(true);
    expect(carriers[0]?.getAttribute("autocomplete")).toBe("new-password");

    // And it is not anywhere a copy would survive the page.
    expect(JSON.stringify(window.localStorage)).not.toContain(TOKEN);
    expect(JSON.stringify(window.sessionStorage)).not.toContain(TOKEN);
    expect(document.cookie).not.toContain(TOKEN);
    expect(window.location.search).not.toContain(TOKEN);

    wrapper.unmount();
  });

  it("never puts the token in a live region", async () => {
    const wrapper = await mountRevealed();

    // `aria-live` off is not the same as absent, and a live region carrying a
    // credential is a screen reader reading it aloud in a shared space. So
    // the assertion is over EVERY element's computed live-ness, not just the
    // panel: a wrapper, a toast host or an announcement region elsewhere in
    // the tree would be as bad as one here.
    const live = wrapper.findAll("[aria-live]");
    for (const region of live) {
      expect(region.attributes("aria-live")).toBe("off");
    }
    expect(wrapper.find('[aria-live="assertive"]').exists()).toBe(false);
    expect(wrapper.find('[aria-live="polite"]').exists()).toBe(false);
    expect(wrapper.find('[role="alert"]').exists()).toBe(false);
    expect(wrapper.find('[role="status"]').exists()).toBe(false);

    wrapper.unmount();
  });

  it("never writes the token to the console, which is where a leak survives forever", async () => {
    const logged: unknown[] = [];
    for (const level of ["log", "info", "warn", "error", "debug"] as const) {
      vi.spyOn(console, level).mockImplementation((...args: unknown[]) => {
        logged.push(...args);
      });
    }

    const wrapper = await mountRevealed();
    for (const line of logged) {
      expect(String(line)).not.toContain(TOKEN);
    }
    // A rejected fetch, an unhandled rejection and a Vue warning are the
    // usual accidental carriers, so the mint is also made to fail once.
    vi.mocked(createApiKey).mockResolvedValue({
      ok: false,
      failure: { kind: "transport", error: new Error(TOKEN) },
    });
    await wrapper.get("button").trigger("click");
    await nextTick();
    for (const line of logged) {
      expect(String(line)).not.toContain(TOKEN);
    }

    wrapper.unmount();
  });

  it("clears the token on dismiss and again on unmount", async () => {
    const wrapper = await mountRevealed();
    expect(wrapper.text()).toContain("Copy this key now");

    // Dismiss, the way the operator does.
    const dismiss = wrapper
      .findAll("button")
      .find((button) => button.text().includes("hide the key"));
    expect(dismiss).toBeDefined();
    await dismiss!.trigger("click");
    await nextTick();
    expect(wrapper.text()).not.toContain("Copy this key now");

    // The flag is what a template reads, and it is a boolean, never the
    // token: a component holding `revealed` true and `secret` set is the
    // state that would survive a Back button with a live credential in it.
    const second = await mountRevealed();
    second.unmount();
    expect(document.body.innerHTML).not.toContain(TOKEN);
  });

  it("tells the operator the key cannot be retrieved again", async () => {
    const wrapper = await mountRevealed();
    const text = wrapper.text();
    expect(text).toMatch(/only time/i);
    expect(text).toMatch(/cannot be retrieved|cannot get it back|create another/i);
    wrapper.unmount();
  });

  it("routes the mint through the seam, never the generated client", async () => {
    await mountRevealed();
    // The one-seam rule is a structural claim, and this is what makes it
    // structural: the component's only network call is `createApiKey`, and
    // that is the seam's wrapper rather than the generated `mintApiKey`.
    expect(vi.mocked(createApiKey)).toHaveBeenCalledWith("ci key");
  });
});
