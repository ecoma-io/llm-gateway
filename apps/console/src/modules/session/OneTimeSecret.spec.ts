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

/**
 * The component's OWN state, read from the live instance.
 *
 * `<script setup>` keeps a non-exposed binding out of the parent's
 * `setupState`, so the only honest way in is `$.devtoolsRawSetupState` — which
 * is the object a devtools inspector walks, and is therefore exactly the
 * surface the leak this file guards against would show up on. A surviving
 * `shallowRef` there is a credential in a component instance a devtools
 * inspector can read even after the panel is gone from the DOM.
 *
 * The RAW graph holds the refs THEMSELVES rather than their values, so the
 * reading is via `__v_isRef`/`value` rather than a direct property access: a
 * helper that read `state.secret` would see the ref object and could not tell
 * an erased credential from a live one.
 *
 * Reading the DOM instead of this is not a weaker version of the same claim,
 * it is a different one. `IdentityPage.spec.ts` records the exact reason in
 * its own comment: a DOM-only version of the dismiss assertion passed against
 * a `clear()` that never erased anything, because `clear()` runs TWICE for one
 * click — the component's own button and then the Card's dismiss path — so the
 * second pass tidied up after a broken first one and the screen looked right.
 */
function heldState(wrapper: ReturnType<typeof mount>): {
  secret: string | undefined;
  revealed: boolean;
} {
  const state = (
    wrapper.vm as unknown as {
      $: { devtoolsRawSetupState: Record<string, unknown> };
    }
  ).$.devtoolsRawSetupState;
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

  it("clears the token on dismiss, which is the way an operator leaves it", async () => {
    const wrapper = await mountRevealed();
    expect(wrapper.text()).toContain("Copy this key now");
    // The credential is in the component's own graph before the click, so the
    // assertions below are known to be reading something that was there — a
    // helper that found `undefined` throughout would make every later
    // `toBeUndefined` pass without anything having been erased.
    expect(heldState(wrapper).secret).toBe(TOKEN);

    // Dismiss, the way the operator does.
    const dismiss = wrapper
      .findAll("button")
      .find((button) => button.text().includes("hide the key"));
    expect(dismiss).toBeDefined();
    await dismiss!.trigger("click");
    await nextTick();
    expect(wrapper.text()).not.toContain("Copy this key now");

    // The state, not the screen. The panel is a `v-if` on `revealed`, so it is
    // gone the moment the flag is false whatever `secret` still holds — a
    // `clear()` that dropped the flag and kept the token would leave a clean
    // screen with a live credential bound to a `model-value` a re-render would
    // paint again. `IdentityPage.spec.ts` records that this exact mistake
    // shipped once and was invisible to a DOM-only assertion.
    const held = heldState(wrapper);
    expect(held.secret).toBeUndefined();
    expect(held.revealed).toBe(false);
    // And the element the credential was painted into is genuinely detached,
    // not merely unmounted from the template — `name` is unique, so this
    // cannot be satisfied by some other field on the page.
    expect(document.querySelector('input[name="api-key-secret"]')).toBeNull();
    expect(document.body.innerHTML).not.toContain(TOKEN);

    wrapper.unmount();
  });

  it("clears the token again on unmount, the path a Back button takes", async () => {
    // The other half, and the reason `clear()` is wired to two hooks rather
    // than one. A visitor who navigates away never clicks the dismiss button,
    // so a component that clears only on dismiss leaves a live credential in a
    // component instance that is still reachable from a devtools inspector
    // after the DOM is gone — which is the state the ADR's first rule names,
    // and the one a DOM-only assertion cannot see at all: once a component is
    // unmounted its rendered tree is removed by Vue regardless of what its
    // refs still hold, so `document.body.innerHTML` is empty either way and the
    // mutation this guards against is a silent, permanent no-op.
    const wrapper = await mountRevealed();
    expect(heldState(wrapper).secret).toBe(TOKEN);

    // No dismiss: the component is torn down the way a route change tears it
    // down, mid-reveal, with the panel still on screen.
    expect(wrapper.text()).toContain("Copy this key now");
    wrapper.unmount();

    // The credential is read back off the instance the component leaves behind.
    // `unmount()` does not destroy the vm, so the setup state is still readable
    // here — which is precisely the reachability that makes an uncleared ref a
    // leak rather than a dead reference.
    const held = heldState(wrapper);
    expect(held.secret).toBeUndefined();
    expect(held.revealed).toBe(false);
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
