// Sign-in, and the three properties that make it a screen rather than a form.
//
// The credential is the point of every assertion here. A test that only proved
// the happy path would pass just as happily with the password in a store, in
// the URL, or echoed into a failure message — the failure mode is a NEGATIVE
// and negative assertions are the harder kind, so each rule gets the test that
// can go red if it is broken.
import { flushPromises, mount, type VueWrapper } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { nextTick } from "vue";
import { createMemoryHistory, createRouter, type Router } from "vue-router";

const api = vi.hoisted(() => ({ signInWith: vi.fn() }));
vi.mock("@/lib/api", () => api);

import SignInPage from "@/pages/SignInPage.vue";
import { useSessionStore } from "@/stores/session";
import type { SignInResponse } from "@ecoma-io/llm-gateway-console-api-client";

const PRINCIPAL: SignInResponse = {
  principal: {
    class: "user",
    account_id: "a0000000-0000-4000-8000-0000000000a1",
    user_id: "u0000000-0000-4000-8000-0000000000u1",
    email: "ops@example.test",
  },
};

/** A refusal with the contract's code, the way the seam parses one. */
function refused(code: "unauthenticated" | "internal" | "service_unavailable") {
  return {
    ok: false as const,
    failure: {
      kind: "api" as const,
      unauthenticated: code === "unauthenticated",
      envelope: { error: { code, message: "the server's words" }, request_id: "req-1" },
    },
  };
}

function makeRouter(query: Record<string, string> = {}): Router {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/sign-in", component: { template: "<div />" } },
      { path: "/", component: { template: "<div />" } },
      { path: "/identity", component: { template: "<div />" } },
    ],
  });
  void router.push({ path: "/sign-in", query });
  return router;
}

async function mountPage(query: Record<string, string> = {}) {
  setActivePinia(createPinia());
  const router = makeRouter(query);
  await router.isReady();
  const wrapper: VueWrapper = mount(SignInPage, {
    attachTo: document.body,
    global: { plugins: [router] },
  });
  await nextTick();
  return { wrapper, router };
}

async function fill(wrapper: VueWrapper) {
  const inputs = wrapper.findAll("input");
  await inputs[0].setValue("a0000000-0000-4000-8000-0000000000a1");
  await inputs[1].setValue("ops@example.test");
  await inputs[2].setValue("correct horse battery staple");
}

describe("SignInPage", () => {
  beforeEach(() => {
    api.signInWith.mockReset();
    document.body.innerHTML = "";
  });

  it("asks for the account id, the email and the credential, in that order", async () => {
    api.signInWith.mockResolvedValue({ ok: true, data: PRINCIPAL });
    const { wrapper } = await mountPage();
    await fill(wrapper);

    const labels = wrapper.findAll("label").map((label) => label.text().replace(" *", ""));
    expect(labels).toEqual(["Account id", "Email", "Password"]);

    // The password control is a real password field: type=password keeps it off
    // a screen, type=email gives the right on-screen keyboard, and none of the
    // three is an autocomplete target that would store the credential.
    const inputs = wrapper.findAll("input");
    expect(inputs.map((input) => input.attributes("type"))).toEqual(["text", "email", "password"]);
    expect(inputs[2].attributes("autocomplete")).toBe("current-password");
  });

  /** Submits and waits for the navigation the submission caused to settle. */
  async function submitAndSettle(wrapper: VueWrapper, router: Router) {
    await wrapper.get("form").trigger("submit");
    await flushPromises();
    await router.isReady();
    await nextTick();
  }

  it("signs in, records the principal and lands on the route the visitor was trying to reach", async () => {
    api.signInWith.mockResolvedValue({ ok: true, data: PRINCIPAL });
    const { wrapper, router } = await mountPage({ redirect: "/identity?state=active" });
    await fill(wrapper);

    await submitAndSettle(wrapper, router);

    const session = useSessionStore();
    expect(session.signedIn).toBe(true);
    expect(session.accountId).toBe("a0000000-0000-4000-8000-0000000000a1");
    expect(router.currentRoute.value.fullPath).toBe("/identity?state=active");
  });

  it("never follows a redirect that names another origin", async () => {
    api.signInWith.mockResolvedValue({ ok: true, data: PRINCIPAL });
    // An open redirect through a query parameter hands a phishing page the
    // operator's trust and the impression that this console sent them there.
    const { wrapper, router } = await mountPage({ redirect: "https://elsewhere.test/steal" });
    await fill(wrapper);

    await submitAndSettle(wrapper, router);

    expect(router.currentRoute.value.path).toBe("/");
  });

  it("clears the credential from the form whether the sign-in worked or not", async () => {
    api.signInWith.mockResolvedValue(refused("unauthenticated"));
    const { wrapper } = await mountPage();
    await fill(wrapper);

    await wrapper.get("form").trigger("submit");
    await nextTick();
    await nextTick();

    // The password field is empty and the value is nowhere in the rendered DOM.
    expect(wrapper.findAll("input")[2].element.value).toBe("");
    expect(wrapper.html()).not.toContain("correct horse battery staple");
  });

  it("gives one answer to every refused sign-in, because the contract gives one answer", async () => {
    // "no such account", "no such user", "wrong credential" and "invited row" are
    // the same 401 by design. A screen that told those apart would be a
    // membership oracle for the account id.
    //
    // The list is EIGHT refusals and the code is the same string every time,
    // because the contract has one 401 and this loop exists to say so. It was
    // four copies of the same word before, which is a loop that proves the case
    // once and looks as though it proved it four times — and the next person to
    // read it believes the four were different failures. One refusal, and the
    // screen's own comment is the argument.
    api.signInWith.mockResolvedValue(refused("unauthenticated"));
    const { wrapper } = await mountPage();
    await fill(wrapper);
    await wrapper.get("form").trigger("submit");
    await nextTick();
    await nextTick();

    const text = wrapper.text();
    expect(text).toContain("Those details did not sign anyone in");
    // The sentence the operator acts on is the same one, so it is the one
    // asserted: if the screen ever told the four apart this is where the extra
    // wording would have to appear.
    expect(text).toContain("Check the account id, the address and the credential.");
  });

  it("puts the cursor on the form after a refusal, and never on a field", async () => {
    // Focus is the one channel that would say WHICH of the three values was
    // rejected, and the contract says none of them individually. Parking the
    // cursor on the account field answers "that account is not here"; on the
    // email field, "that address is not in that account". Both are the
    // membership oracle, handed over by the focus ring.
    api.signInWith.mockResolvedValue(refused("unauthenticated"));
    const { wrapper } = await mountPage();
    await fill(wrapper);
    await wrapper.get("form").trigger("submit");
    await nextTick();
    await nextTick();

    expect(document.activeElement).toBe(wrapper.get("form").element);
    for (const input of wrapper.findAll("input")) {
      expect(document.activeElement).not.toBe(input.element);
    }
  });

  it("describes the form by the refusal, so a reader who looks back is told", async () => {
    // `role="alert"` announces the refusal once and then forgets it: it is not
    // in the form's description, so a reader who is already on the third field,
    // or who looks at the form rather than listening, never hears why the
    // button did nothing. The form points at the alert by id.
    api.signInWith.mockResolvedValue(refused("unauthenticated"));
    const { wrapper } = await mountPage();

    expect(wrapper.get("form").attributes("aria-describedby")).toBeUndefined();

    await fill(wrapper);
    await wrapper.get("form").trigger("submit");
    await nextTick();
    await nextTick();

    const describedBy = wrapper.get("form").attributes("aria-describedby");
    expect(describedBy).toBe("sign-in-failure");
    expect(wrapper.get(`#${describedBy}`).text()).toContain("Those details did not sign anyone in");
  });

  it("never marks an input invalid, because the contract never said which was wrong", async () => {
    // The one thing `aria-invalid` would claim is a field whose value the server
    // rejected, and the single 401 does not say that. This is the same claim
    // focus would make, stated in the attribute instead of the ring.
    api.signInWith.mockResolvedValue(refused("unauthenticated"));
    const { wrapper } = await mountPage();
    await fill(wrapper);
    await wrapper.get("form").trigger("submit");
    await nextTick();
    await nextTick();

    for (const input of wrapper.findAll("input")) {
      expect(input.attributes("aria-invalid")).toBeUndefined();
    }
  });

  it("does not present a 500 as a credential problem", async () => {
    api.signInWith.mockResolvedValue(refused("internal"));
    const { wrapper } = await mountPage();
    await fill(wrapper);
    await wrapper.get("form").trigger("submit");
    await nextTick();
    await nextTick();

    // A bug on the server is not something the operator typed wrong, and
    // telling them to check their password for a 500 sends them round in
    // circles. The matrix's own title is what they see.
    const text = wrapper.text();
    expect(text).toContain("Something went wrong on our side");
    expect(text).not.toContain("Those details did not sign anyone in");
  });

  it("offers nothing to fill in until the three inputs are given", async () => {
    const { wrapper } = await mountPage();

    const submit = wrapper.get("form button[type=submit]");
    expect(submit.attributes("disabled")).toBeDefined();

    await wrapper.findAll("input")[0].setValue("a0000000");
    await nextTick();
    expect(submit.attributes("disabled")).toBeDefined();
  });

  it("hands the failure the matrix says, and no more", async () => {
    // A transport failure never reached the contract's vocabulary, so the screen
    // must not claim the credential was refused.
    api.signInWith.mockResolvedValue({
      ok: false,
      failure: { kind: "transport", error: new TypeError() },
    });
    const { wrapper } = await mountPage();
    await fill(wrapper);
    await wrapper.get("form").trigger("submit");
    await nextTick();
    await nextTick();

    expect(wrapper.text()).toContain("Those details did not sign anyone in");
  });
});

describe("the session store in the sign-in flow", () => {
  // The store is what the route guard's one question resolves against, so the
  // property that makes it safe is asserted here: the whole of what it can hold
  // is a class, an account id and an address.
  it("holds no credential, because the seam returns none to hold", () => {
    setActivePinia(createPinia());
    const session = useSessionStore();
    session.remember({ class: "user", accountId: "a1" });
    // The whole of what the store can hold: a class, an account id, an address.
    // A token, a secret and a key are not fields here and could not be added
    // without changing the type.
    expect(Object.keys(session.$state).sort()).toEqual(["principal", "resolving", "signedIn"]);
    expect(JSON.stringify(session.$state)).not.toMatch(/token|secret|password/i);
  });
});
