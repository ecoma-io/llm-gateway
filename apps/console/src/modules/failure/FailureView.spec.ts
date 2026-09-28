// The failure view, against every row the matrix has.
//
// ADR 0012 §6 makes `CONSOLE_BEHAVIOUR` the single place the console decides
// what a failure MEANS. This component is the only thing that reads it, so
// the tests here are the proof that the table is actually load-bearing: if
// the component ever re-derived behaviour from a status number, the
// matrix would quietly stop mattering and every one of these would still
// pass. So the assertion is over the TABLE, not over hand-picked codes —
// adding a code to `shared/errors.yaml` without a row here fails this file.
//
// "Over the table" needs one word of care, and this file's first version got
// it wrong in the most expensive direction: the code list was read off
// `CONSOLE_BEHAVIOUR` with `Object.keys`, which is a derivation from the
// thing under test. Every assertion below that is "for each code, the view
// renders the row's title" is a real claim about the VIEW; the claim about
// the TABLE having a row per contract code is a claim about the two SETS, and
// it is asserted by comparing them rather than by looping over one. The list
// and the type that ties it to the generated union are stated below.
import { mount } from "@vue/test-utils";
import { describe, expect, expectTypeOf, it } from "vitest";
import { createRouter, createWebHistory } from "vue-router";

import { CONSOLE_BEHAVIOUR, UNREACHABLE_CODES, type ApiErrorCode } from "@/lib/failure-matrix";
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

/**
 * The contract's `Error.code` enum, written out.
 *
 * **Why a literal list and not `Object.keys(CONSOLE_BEHAVIOUR)`.** An earlier
 * version of this file derived its code list from the table it was asserting
 * about, which is a tautology: `Object.keys` can only return the object's own
 * keys, so "every derived code has a row" is unfalsifiable — it holds for a
 * table declaring every contract code and for one declaring none, and
 * the only way it can fail is by the table becoming empty, in which case the
 * loop never runs. The claim it NAMES is the one thing the test could not
 * see. Deriving from the table under test is the one derivation that proves
 * nothing about it.
 *
 * **Why the list is not derived from the generated union at runtime.** It
 * cannot be. `Error` is exported from the generated client as a `type`, so
 * `Error["code"]` is erased before the bundle ships — there is no runtime value
 * to enumerate, and a test that claimed to have enumerated it would be
 * enumerating a copy of the same list. The union is therefore tied to this one
 * by the COMPILER, below, rather than by JavaScript.
 *
 * The twelve members are the `enum` under
 * `components.schemas.Error.properties.code` in
 * `api/openapi/shared/errors.yaml`, in the order that document declares them.
 * `conflict` is the newest of them: it is the Console API's payment surface
 * refusing a well-formed request because the server's own state says no, and it
 * is the code that proves this list is a claim rather than a copy — adding a row
 * to `CONSOLE_BEHAVIOUR` for it without touching this list fails the set
 * comparison below on the next run.
 */
const CONTRACT_CODES = [
  "not_found",
  "method_not_allowed",
  "invalid_request",
  "unauthenticated",
  "cursor_expired",
  "unsupported_version",
  "revision_gap",
  "snapshot_required",
  "conflict",
  "upstream_unavailable",
  "service_unavailable",
  "internal",
] as const satisfies readonly ApiErrorCode[];

/**
 * The exactness tie between the list above and the generated union, in both
 * directions and in the type system rather than in an assertion.
 *
 * `satisfies` alone only proves the list is a SUBSET — a list that named one
 * code and stopped would satisfy it happily, and a contract that gained a code
 * would leave the list compiling against a union it no longer matches. So the
 * exactness is stated separately: `toEqualTypeOf` is symmetric, and the union
 * is this list's element type precisely when the two are the same set. Add a
 * code to `shared/errors.yaml`, regenerate the client and run `vue-tsc`, and
 * this line is a compile error naming the code it is missing.
 */
expectTypeOf<(typeof CONTRACT_CODES)[number]>().toEqualTypeOf<ApiErrorCode>();

/**
 * The three codes `api/openapi/console.yaml` cannot return.
 *
 * They belong to the Data Plane's projection operations — the contract says so
 * on the `code` enum itself — and the console declares them anyway, because the
 * exhaustiveness requirement is a property of the contract's union rather than
 * of today's reachability. Stated as a literal for the same reason
 * `CONTRACT_CODES` is: the table's own `reachable` flags are the thing under
 * test, so reading them would be a derivation from the thing under test.
 */
const PROJECTION_CODES = [
  "unsupported_version",
  "revision_gap",
  "snapshot_required",
] as const satisfies readonly ApiErrorCode[];

/** Every code the table declares, read off the table — the ROW side of the claim. */
const DECLARED_ROWS = Object.keys(CONSOLE_BEHAVIOUR);

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
    // Both directions, in JavaScript as well as in the type above. The set
    // comparison is what makes this falsifiable: a table missing a contract
    // code, and a table carrying a code the contract does not declare, are two
    // different ways for the two sets to differ, and the second one is the
    // drift `CONSOLE_BEHAVIOUR`'s `satisfies` clause exists to prevent — so it
    // is worth pinning at runtime as well, where a test can see it.
    expect([...DECLARED_ROWS].sort()).toEqual([...CONTRACT_CODES].sort());

    for (const code of DECLARED_ROWS) {
      // A row that exists but says nothing is not a case for the code, and
      // `Object.keys` returning it proves only that the key is spelled the way
      // the test expects.
      expect(CONSOLE_BEHAVIOUR[code as ApiErrorCode].title.length, code).toBeGreaterThan(0);
    }
  });

  /**
   * The test `failure-matrix.ts`'s header promises by name. It did not exist —
   * the name appeared nowhere in the repository but the comment — so the plane
   * boundary it advertised as test-pinned was pinned by nothing.
   *
   * **`reachable` is a hand-set boolean, and nothing else checks it.** The page
   * specs read it to decide which codes to drive through a screen, so flipping
   * `internal` to `reachable: false` takes a real, reachable code out of every
   * suite's loop and leaves everything green: the code still has a row, the row
   * is still keyed exhaustively, and the only thing that changed is a flag no
   * assertion ever looked at. This is the one test that looks at it, and it
   * looks at it from the CONTRACT side — the list below is the projection
   * vocabulary the Data Plane's management API returns and `console.yaml` does
   * not, stated here rather than read off the table, for the same reason
   * `CONTRACT_CODES` is.
   */
  function assertEveryCodeIsDeclaredOrUnreachable() {
    // Declared: every code the contract can return has a row, which is the set
    // comparison above. Re-stated here so the name is a whole claim and not a
    // pointer to a neighbouring test.
    expect([...DECLARED_ROWS].sort()).toEqual([...CONTRACT_CODES].sort());

    // Unreachable: exactly the three projection codes, and no others. The
    // second half is the part that catches a flipped `reachable`, because a code
    // marked unreachable that is NOT in the projection vocabulary is a code this
    // console declined to test while `console.yaml` can still return it.
    expect([...UNREACHABLE_CODES].sort()).toEqual([...PROJECTION_CODES].sort());
    for (const code of PROJECTION_CODES) {
      expect(CONSOLE_BEHAVIOUR[code].reachable, code).toBe(false);
    }
  }

  it("declares or marks unreachable every code the contract can return", () => {
    assertEveryCodeIsDeclaredOrUnreachable();
  });

  it("renders each code's own declared title, never a generic one", () => {
    for (const code of CONTRACT_CODES) {
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
    for (const code of CONTRACT_CODES) {
      const wrapper = mountView({ failure: apiFailure(code) });
      const shown = wrapper.text().includes("req-abc-123");
      expect(shown).toBe(CONSOLE_BEHAVIOUR[code].showRequestId);
      wrapper.unmount();
    }
  });

  it("offers exactly the recovery the row names", () => {
    for (const code of CONTRACT_CODES) {
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
    for (const code of CONTRACT_CODES) {
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
