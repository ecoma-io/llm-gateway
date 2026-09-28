// The payment state vocabulary, held to the contract rather than to itself.
//
// Two maps over one union — the badge's three channels and the sentence a
// customer who has just come back from a checkout needs — and a set of states
// the console treats as "the provider has not spoken yet". The claim this file
// makes is that all three are TOTAL over `PaymentIntentState`: the source is
// written with `satisfies Record<PaymentIntentState, …>`, which is the compile
// gate, and the list of ten below is the same claim made at runtime by a
// comparison of two SETS rather than by looping over the map.
//
// That distinction is the one `FailureView.spec.ts` learned the expensive way.
// `Object.keys(map)` is a derivation from the thing under test: "every derived
// key has a row" holds for a map that declares every contract state and for one
// that declares none. So the states are written out here as a literal, tied to
// the generated union by `expectTypeOf` in BOTH directions, and the assertions
// compare the map's keys against that literal.
//
// Two of the ten carry meaning that a wrong word would invert, and both are
// tested as claims rather than as spellings:
//
//   - `expired` must not promise the payment will never be funded. The contract
//     allows `expired → succeeded` and says a late delivery is still honoured,
//     because expiry is this platform's decision to stop WAITING and not a fact
//     about the money. A console that told a customer "not funded" there would
//     be contradicting a provider that can still prove them right.
//   - `quarantined` is not a payment failure. It is what an authenticated
//     delivery that could not be read is recorded as, and the state vocabulary
//     is shared with the payment so an operator reads one list. A label calling
//     it a failure would report the message's problem as the money's.
import { describe, expect, expectTypeOf, it } from "vitest";

import {
  PAYMENT_AWAITING_PROVIDER_STATES,
  PAYMENT_STATE_EXPLANATION,
  PAYMENT_STATE_PRESENTATION,
  type StatusPresentation,
} from "@/modules/status/presentation";
import type { PaymentIntentState } from "@ecoma-io/llm-gateway-console-api-client";

/**
 * The contract's `PaymentIntentState` union, written out.
 *
 * The ten members are the `enum` under
 * `components.schemas.PaymentIntentState` in `api/openapi/console.yaml`, in the
 * order that document declares them. A literal rather than a derivation, for
 * the reason the header gives: the maps are the thing under test, and reading
 * their keys would be reading the answer out of the question.
 */
const CONTRACT_STATES = [
  "created",
  "checkout_open",
  "requires_action",
  "succeeded",
  "failed",
  "cancelled",
  "expired",
  "partially_refunded",
  "refunded",
  "quarantined",
] as const satisfies readonly PaymentIntentState[];

/**
 * The exactness tie, in both directions and in the type system.
 *
 * `satisfies` alone proves only that the list is a SUBSET — a list naming one
 * state would satisfy it — so the exactness is stated separately: `toEqualTypeOf`
 * is symmetric, and the generated union is this list's element type precisely
 * when the two are the same set. Regenerate the client with an eleventh state
 * and `vue-tsc` fails here, naming the state this list is missing.
 */
expectTypeOf<(typeof CONTRACT_STATES)[number]>().toEqualTypeOf<PaymentIntentState>();

/** The five tones Loom's `Badge`/`Alert` declare, read from the type rather than restated. */
const TONES = new Set<StatusPresentation["tone"]>([
  "neutral",
  "info",
  "success",
  "warning",
  "destructive",
]);

describe("the payment state maps are total over the contract's union", () => {
  it("gives every state a badge, and no state a badge the contract has not declared", () => {
    expect([...Object.keys(PAYMENT_STATE_PRESENTATION)].sort()).toEqual(
      [...CONTRACT_STATES].sort(),
    );
  });

  it("gives every state a sentence, over exactly the same set", () => {
    // A state with a badge and no sentence would be a state the console renders
    // without explaining, and the explanation is where the money claims live.
    expect([...Object.keys(PAYMENT_STATE_EXPLANATION)].sort()).toEqual([...CONTRACT_STATES].sort());
  });

  it("carries all three channels on every state, never colour alone", () => {
    // ADR 0012 §7: colour, an `aria-hidden` icon, and a human label. The icon
    // is checked as present and as callable — `StatusBadge` is what marks it
    // `aria-hidden`, and a map with a missing icon would render a badge whose
    // only sighted channel in greyscale is gone.
    for (const state of CONTRACT_STATES) {
      const presentation = PAYMENT_STATE_PRESENTATION[state];
      expect(presentation.icon, state).toBeDefined();
      expect(typeof presentation.icon, state).not.toBe("string");
      expect(TONES.has(presentation.tone), `${state} tone`).toBe(true);
      expect(presentation.label.trim(), state).not.toBe("");
    }
  });

  it("labels a state with a phrase, never with the contract's own token", () => {
    // The defect the file header names on `GatewayStatusPage.vue:87`: a badge
    // reading `checkout_open` tells a customer nothing they could act on. The
    // test is that no label is the token, or the token with its underscores
    // turned into spaces and its first letter capitalised — both of which are
    // the same defect wearing a tidy-up.
    for (const state of CONTRACT_STATES) {
      const label = PAYMENT_STATE_PRESENTATION[state].label;
      const tidied = state.replace(/_/g, " ");
      expect(label, state).not.toBe(state);
      expect(label.toLowerCase(), state).not.toBe(tidied.toLowerCase());
      // A single-token state must not appear verbatim inside its own label
      // either: "succeeded" inside "Succeeded" is the token again.
      expect(label.toLowerCase(), state).not.toBe(state.charAt(0).toUpperCase() + state.slice(1));
    }
  });

  it("explains a state in a sentence rather than restating its label", () => {
    for (const state of CONTRACT_STATES) {
      const explanation = PAYMENT_STATE_EXPLANATION[state];
      // A sentence: more than one word, and punctuated. A fragment would be a
      // label in a paragraph's clothes.
      expect(explanation.split(/\s+/).length, state).toBeGreaterThan(3);
      expect(explanation, state).toMatch(/[.!?]$/);
      expect(explanation, state).not.toBe(PAYMENT_STATE_PRESENTATION[state].label);
    }
  });
});

describe("expired does not promise the customer anything the provider can contradict", () => {
  it("does not call an expired payment one that will never be funded", () => {
    // The contract's own state machine admits `expired → succeeded`, and its
    // field documentation says a delivery arriving after the deadline "is still
    // honoured, because the provider alone gets to say whether the customer
    // paid". So "Not funded" is a claim this console is not entitled to make on
    // the money's behalf, and the promise it breaks is the customer's.
    const label = PAYMENT_STATE_PRESENTATION.expired.label;
    const explanation = PAYMENT_STATE_EXPLANATION.expired;

    expect(label).not.toMatch(/not funded/i);
    expect(explanation).not.toMatch(/\bnever\b/i);
    expect(explanation).not.toMatch(/not funded/i);
    // And the positive half, which is the claim worth making: a late
    // confirmation is still honoured. A test that only forbade words would pass
    // on an empty sentence.
    expect(explanation).toMatch(/honou?red/i);
    expect(explanation).toMatch(/provider/i);
  });

  it("says what expiry actually is: this platform stopped waiting", () => {
    expect(PAYMENT_STATE_EXPLANATION.expired).toMatch(/stopped waiting/i);
    expect(PAYMENT_STATE_PRESENTATION.expired.label).toMatch(/stopped waiting/i);
  });
});

describe("quarantined is a fact about a message, not about a payment", () => {
  it("does not render a quarantined payment as a payment that failed", () => {
    // The state is on the shared payment vocabulary because a delivery that
    // authenticated and could not be interpreted is recorded where an operator
    // will look, and the contract says the payment's own status "stays whatever
    // the money says it is". So the label must not read as a funding outcome,
    // and the sentence must say whose problem this is.
    const label = PAYMENT_STATE_PRESENTATION.quarantined.label;
    const explanation = PAYMENT_STATE_EXPLANATION.quarantined;

    expect(label).not.toMatch(/not funded|failed/i);
    expect(explanation).not.toMatch(/not funded/i);
    expect(explanation).toMatch(/message/i);
    expect(explanation).toMatch(/not about your payment/i);
    // An operator is named as the person who can act on it, which is the part a
    // customer cannot.
    expect(explanation).toMatch(/operator/i);
  });

  it("is not one of the states the console treats as waiting on the provider", () => {
    // The waiting set drives the sentence a customer sees when they come back
    // from a checkout. A quarantined payment is not waiting for anything, and a
    // card telling the customer their provider is still thinking would be the
    // console explaining the wrong problem to the wrong person.
    expect(PAYMENT_AWAITING_PROVIDER_STATES.has("quarantined")).toBe(false);
  });
});

describe("the states the console treats as waiting on the provider", () => {
  it("is exactly the three the provider has not spoken about", () => {
    expect([...PAYMENT_AWAITING_PROVIDER_STATES].sort()).toEqual([
      "checkout_open",
      "created",
      "requires_action",
    ]);
    // Explicitly the negative half, because the set is the input to the one
    // sentence this screen must not get wrong: every state where the provider
    // HAS decided is absent, so a settled payment can never be described as
    // awaiting a confirmation that has already arrived.
    for (const decided of [
      "succeeded",
      "failed",
      "cancelled",
      "expired",
      "partially_refunded",
      "refunded",
      "quarantined",
    ] as const) {
      expect(PAYMENT_AWAITING_PROVIDER_STATES.has(decided), decided).toBe(false);
    }
  });

  it("says, in the sentence a returning customer reads, that coming back proves nothing", () => {
    // The single most load-bearing sentence on the screen, asserted where it is
    // written rather than only where it is rendered. The browser is not a
    // financial boundary, and this is the copy that says so out loud.
    expect(PAYMENT_STATE_EXPLANATION.checkout_open).toMatch(
      /coming back does not mark anything paid/i,
    );
    expect(PAYMENT_STATE_EXPLANATION.checkout_open).toMatch(/waiting for the provider/i);
  });
});
