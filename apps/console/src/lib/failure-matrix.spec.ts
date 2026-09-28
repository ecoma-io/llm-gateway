// The `conflict` row, and the one thing it must not be confused with.
//
// `CONSOLE_BEHAVIOUR` is asserted as a WHOLE in `FailureView.spec.ts` — every
// contract code has a row, the projections are the only unreachable ones, and
// the view renders each row. This file is about the row the payment surface
// added, and about the single distinction that decides what the screen offers a
// customer after a refusal: is the REQUEST wrong, or is the SERVER's state
// refusing a request that is fine?
//
//   - `invalid_request` is the first. A cursor carrying a filter the page no
//     longer makes is the console's own case of it, and the console's answer is
//     to change the request — drop the cursor and read the first page. That is
//     why the row offers a retry.
//   - `conflict` is the second. `createPaymentIntent` refuses a top-up when the
//     account's own state does not permit a new payment, and the contract is
//     explicit that "there is nothing wrong with the request to correct, and no
//     edit to it would help". So the row offers NO retry: a retry button here
//     is not merely useless, it tells the reader an edit exists, and the one
//     thing a client must not do with this code is rebuild the payload and send
//     it again.
//
// The contract also forecloses the other reading, which is worth pinning
// because it is the intuitive one: "the idempotency key is deliberately NOT a
// second cause". A repeated key is not a conflict — it converges on the payment
// it already names, as a 201 — so a client that treated a 409 as "my key was
// reused" would be inventing a failure mode and repairing the wrong thing.
import { describe, expect, it } from "vitest";

import { behaviourFor, CONSOLE_BEHAVIOUR, UNREACHABLE_CODES } from "@/lib/failure-matrix";

describe("the conflict row", () => {
  it("exists, is reachable, and surfaces the correlation", () => {
    // `reachable: true` is load-bearing rather than descriptive: the page specs
    // read it to decide which codes to drive through a screen, so a code left
    // unreachable is a code no screen tests and no suite notices.
    const row = CONSOLE_BEHAVIOUR.conflict;
    expect(row.reachable).toBe(true);
    expect(UNREACHABLE_CODES).not.toContain("conflict");
    expect(row.showRequestId).toBe(true);
    expect(["neutral", "info", "success", "warning", "destructive"]).toContain(row.variant);
  });

  it("is reachable through `behaviourFor`, with no fallback in between", () => {
    expect(behaviourFor("conflict")).toBe(CONSOLE_BEHAVIOUR.conflict);
  });

  it("offers no retry, because no edit to the request would help", () => {
    // The recovery column IS the affordance: `FailureView` renders a retry
    // button for `retry` and nothing for `none`, so this one assertion is the
    // whole of "the screen does not invite the customer to try again" — there is
    // no separate "no retry" flag to keep in step with it.
    expect(CONSOLE_BEHAVIOUR.conflict.recovery).toBe("none");
  });

  it("does not tell the reader to try again, or to change anything", () => {
    // The title is the sentence a customer reads, and a `none` recovery with a
    // title like "try again later" would be the affordance coming back in prose
    // after being removed from the button. The refusal is stated; the next move
    // is somebody who can look it up.
    const title = CONSOLE_BEHAVIOUR.conflict.title;
    expect(title).not.toMatch(/\btry again\b|\bretry\b/i);
    expect(title).not.toMatch(/\bcorrect\b|\bfix\b|\bchange\b|\bedit\b/i);
    expect(title.trim()).not.toBe("");
  });
});

describe("conflict is not invalid_request", () => {
  it("differs in the recovery, which is the whole of the client's next move", () => {
    // `invalid_request` is the row that DOES navigate back to a usable state
    // (the console drops the unusable cursor through `onCursorLost`), and the
    // two must not converge: a reader whose account cannot fund itself must not
    // be offered the button that repairs a broken page position.
    expect(CONSOLE_BEHAVIOUR.invalid_request.recovery).toBe("retry");
    expect(CONSOLE_BEHAVIOUR.conflict.recovery).not.toBe(
      CONSOLE_BEHAVIOUR.invalid_request.recovery,
    );
  });

  it("differs in the sentence, so the two refusals are not read as one", () => {
    expect(CONSOLE_BEHAVIOUR.conflict.title).not.toBe(CONSOLE_BEHAVIOUR.invalid_request.title);
    // And the specific inversion: `invalid_request`'s title names a view that
    // cannot be paged, which is a statement about the console's own position.
    // `conflict` must not borrow that vocabulary — nothing about a position is
    // wrong when an account may not fund itself.
    expect(CONSOLE_BEHAVIOUR.conflict.title).not.toMatch(/page|paged|cursor|view/i);
  });

  it("is a statement about the account rather than about the wire", () => {
    // What the row may honestly say. The contract's own cause today is "an
    // account whose own state does not permit a new payment", and the title is
    // phrased for the customer who has to read it: something is unavailable on
    // THIS account, not malformed in a request they cannot see.
    expect(CONSOLE_BEHAVIOUR.conflict.title).toMatch(/account/i);
    expect(CONSOLE_BEHAVIOUR.conflict.title).not.toMatch(/request|payload|body|invalid/i);
  });
});
