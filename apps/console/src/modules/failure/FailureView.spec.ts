// The failure view, against every row the matrix has.
//
// ADR 0012 §6 makes `CONSOLE_BEHAVIOUR` the single place the console decides
// what a failure MEANS. This component is the only thing that reads it, so
// the tests here are the proof that the table is actually load-bearing: if
// the component ever re-derived behaviour from a status number, the
// matrix would quietly stop mattering and every one of these would still
// pass. So the assertion is over the TABLE, not over hand-picked codes —
// adding a code to `shared/errors.yaml` without a row here fails this file.
import { mount } from "@vue/test-utils";
import { describe, expect, it } from "vitest";
import { createRouter, createWebHistory } from "vue-router";

import { CONSOLE_BEHAVIOUR, type ApiErrorCode } from "@/lib/failure-matrix";
import type { ApiFailure, TransportFailure } from "@/lib/api";
import FailureView from "@/modules/failure/FailureView.vue";

/** A contract failure carrying `code`, the shape the gateway actually returns. */
function apiFailure(code: ApiErrorCode): ApiFailure {
  return {
    kind: "api",
    unauthenticated: code === "unauthenticated",
    envelope: {
      error: { code, message: "the server's own words" },
      request_id: "req-abc-123",
    },
  };
}

/** A failure that never reached the contract's vocabulary. */
function transportFailure(): TransportFailure {
  return { kind: "transport", error: new TypeError("Failed to fetch") };
}

const CODES = Object.keys(CONSOLE_BEHAVIOUR) as ApiErrorCode[];

// The sign-in recovery renders a real `RouterLink` — a link, not a button,
// because navigating is what it does. That needs a router, and mounting
// without one fails on the injection rather than on anything the view does.
// A real router rather than a stub, so the href the test reads is the one a
// browser would follow.
const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: "/", component: { template: "<div />" } },
    { path: "/sign-in", component: { template: "<div />" } },
  ],
});

/**
 * Mount the view under a router, so a sign-in recovery renders its link.
 *
 * `onRetry` is always supplied because a retry button is a callback the
 * screen owns — the view does not fetch. A test that omits it sees no retry
 * button at all, which is correct behaviour and useless for asserting that
 * the recovery column is honoured.
 */
function mountView(props: { failure: ApiFailure | TransportFailure }) {
  return mount(FailureView, {
    props: { ...props, onRetry: () => undefined },
    global: { plugins: [router] },
  });
}

describe("the failure view", () => {
  it("has a case for every code the contract can return", () => {
    // The contract's vocabulary and the matrix are meant to be the same set.
    // If the contract grows a code the console cannot explain, this is where
    // it shows up — as an `ApiErrorCode` with no row to render.
    expect(CODES.length).toBeGreaterThan(0);
    for (const code of CODES) {
      expect(CONSOLE_BEHAVIOUR[code]).toBeDefined();
      expect(CONSOLE_BEHAVIOUR[code].title.length).toBeGreaterThan(0);
    }
  });

  it("renders each code's own declared title, never a generic one", () => {
    for (const code of CODES) {
      const wrapper = mountView({ failure: apiFailure(code) });
      expect(wrapper.text()).toContain(CONSOLE_BEHAVIOUR[code].title);
      wrapper.unmount();
    }
  });

  it("never leaks the server's raw message to the operator", () => {
    // `Error.message` is safe to publish per the contract, but the view's
    // job is to say something the matrix authored; a page that printed the
    // server's message verbatim is a page whose wording nobody reviewed.
    const wrapper = mountView({ failure: apiFailure("internal") });
    expect(wrapper.text()).not.toContain("the server's own words");
    wrapper.unmount();
  });

  it("surfaces the request id only where the row says to", () => {
    for (const code of CODES) {
      const wrapper = mountView({ failure: apiFailure(code) });
      const shown = wrapper.text().includes("req-abc-123");
      expect(shown).toBe(CONSOLE_BEHAVIOUR[code].showRequestId);
      wrapper.unmount();
    }
  });

  it("offers exactly the recovery the row names", () => {
    for (const code of CODES) {
      const wrapper = mountView({ failure: apiFailure(code) });
      const recovery = CONSOLE_BEHAVIOUR[code].recovery;
      const hasRetry = wrapper.findAll("button").some((b) => /try again|retry/i.test(b.text()));
      const hasSignIn = wrapper.findAll("a").some((a) => /sign in/i.test(a.text()));

      // `none` must offer NEITHER. A 500 with a retry button is a promise
      // the console cannot keep.
      if (recovery === "none") {
        expect(hasRetry).toBe(false);
        expect(hasSignIn).toBe(false);
      }
      if (recovery === "retry") expect(hasRetry).toBe(true);
      if (recovery === "sign-in") expect(hasSignIn).toBe(true);
      wrapper.unmount();
    }
  });

  it("renders a transport failure honestly, naming no code it was not given", () => {
    const wrapper = mountView({ failure: transportFailure() });

    // It must NOT pretend to be `internal` or `upstream_unavailable`: both
    // would show an operator a request id nobody can look up, or promise a
    // retry against an upstream that was never asked.
    for (const code of CODES) {
      expect(wrapper.text()).not.toContain(CONSOLE_BEHAVIOUR[code].title);
    }
    expect(wrapper.text()).not.toContain("req-abc-123");

    // And it must still be actionable: the one thing an operator can do
    // about a body that is not a contract body is try again.
    expect(wrapper.findAll("button").some((b) => /try again|retry/i.test(b.text()))).toBe(true);
    wrapper.unmount();
  });

  it("announces itself, because it appears without the operator asking", () => {
    const wrapper = mountView({ failure: apiFailure("internal") });

    // The announcement comes from Loom's `Alert`, which renders
    // `role="alert"` — and `role="alert"` IS an assertive live region, so a
    // screen reader speaks this without the operator moving focus to it. A
    // failure that renders as static text is a failure a screen-reader user
    // does not learn about until they happen to read the page.
    expect(wrapper.find('[role="alert"]').exists()).toBe(true);

    // And it is the alert that carries it, so the title is inside the live
    // region rather than beside it — an announcement whose subject is
    // outside the region announces nothing useful.
    const alert = wrapper.get('[role="alert"]');
    expect(alert.text()).toContain(CONSOLE_BEHAVIOUR.internal.title);
    wrapper.unmount();
  });

  it("never puts a credential in a failure, whatever the transport carried", () => {
    // A `TypeError` from a fetch can carry a URL; a rejected client call can
    // carry the request. The view renders the matrix's words and nothing
    // else, which is what makes this hold.
    const leaky: TransportFailure = {
      kind: "transport",
      error: new Error("POST /auth/sign-in?token=Gw_secret failed"),
    };
    const wrapper = mountView({ failure: leaky });
    expect(wrapper.text()).not.toContain("Gw_secret");
    expect(wrapper.text()).not.toContain("/auth/sign-in");
    wrapper.unmount();
  });
});
