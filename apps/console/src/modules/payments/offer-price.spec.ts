// The price list's one formatting rule, tested at the edges rather than in the
// middle.
//
// What this file is really about is the exponent. `Money` is rendered by
// `modules/money/money.ts` as bare grouped digits with the unit named in prose,
// because a `Money` carries no currency and no exponent. A `TopUpOffer` carries
// both, so a chooser has to place a decimal point — and the only honest source
// for where is `minor_unit_exponent`, the field the contract says "travels so a
// client can do the same without carrying its own currency table".
//
// Two failures are guarded here and neither is visible in a happy-path test. A
// screen that assumed two decimal places would render a yen offer as
// hundredths of a yen, which is a price a hundred times too small printed
// beside the name of the thing it buys. And a screen that computed
// `minor_units / 10 ** exponent` would lose the last digits of an amount near
// the contract's ceiling: the transport is a JavaScript `number`, the domain is
// int64, and the contract bounds the field at `Number.MAX_SAFE_INTEGER` exactly
// because above it the client is already holding a value it cannot represent.
// The digits are sliced here, so the rendering is exact for every value the
// field can hold — and the guard refuses the one case where the INPUT has
// already lost its digits, rather than rendering the rounded value as though it
// were the price.
import { describe, expect, it } from "vitest";

import { UNRENDERABLE_AMOUNT } from "@/modules/money/money";
import { formatPrice } from "@/modules/payments/offer-price";

/** A price, in the three fields this file reads. */
function price(amount_minor_units: number, currency: string, minor_unit_exponent: number) {
  return { amount_minor_units, currency, minor_unit_exponent };
}

describe("the offer's own exponent decides the decimal point", () => {
  it("renders a two-decimal currency the way a price list does", () => {
    expect(formatPrice(price(2_500, "EUR", 2))).toBe("25.00 EUR");
    // Padding, not stripping: a 5-unit amount at exponent 2 is five hundredths,
    // and a renderer that printed "0.5" would be showing a tenth of the price.
    expect(formatPrice(price(5, "EUR", 2))).toBe("0.05 EUR");
    expect(formatPrice(price(0, "EUR", 2))).toBe("0.00 EUR");
  });

  it("renders a currency with NO minor unit as whole units", () => {
    // The yen case, and the whole reason the exponent is read rather than
    // assumed. `1_250` at exponent 0 is one thousand two hundred and fifty yen;
    // a renderer that assumed two places would print "12.50 JPY", which is a
    // price a hundred times smaller and a number a customer would believe.
    expect(formatPrice(price(1_250, "JPY", 0))).toBe("1,250 JPY");
    expect(formatPrice(price(3, "JPY", 0))).toBe("3 JPY");
    // No decimal point at all, rather than a trailing one.
    expect(formatPrice(price(1_250, "JPY", 0))).not.toContain(".");
  });

  it("renders the four-decimal currencies the contract admits", () => {
    // The contract's stated bound is 0 to 4, so exponent 4 is a real offer and
    // not a robustness exercise.
    expect(formatPrice(price(1, "KWD", 4))).toBe("0.0001 KWD");
    expect(formatPrice(price(123_456_789, "KWD", 4))).toBe("12,345.6789 KWD");
  });

  it("groups thousands and keeps the currency as the contract's own code", () => {
    // No symbol is painted: the ISO code is a name the contract sent, and a
    // symbol would be a currency table this console does not have.
    expect(formatPrice(price(100_000_000, "USD", 2))).toBe("1,000,000.00 USD");
    expect(formatPrice(price(10_000, "EUR", 2))).not.toMatch(/[€$£]/);
  });

  it("is exact at the largest amount the contract admits", () => {
    // `Number.MAX_SAFE_INTEGER` minor units at exponent 4 — the single largest
    // price this field can carry, and the case a division would round. Every
    // digit below is the amount's own; the point is that the rendering is a
    // string operation and cannot lose one.
    const digits = "900,719,925,474.0991";
    expect(formatPrice(price(Number.MAX_SAFE_INTEGER, "KWD", 4))).toBe(`${digits} KWD`);
  });
});

describe("a price this console cannot render exactly is refused, not approximated", () => {
  it("refuses an exponent outside the contract's declared range", () => {
    // The contract bounds the field at 0..4 and enforces it, so a value outside
    // it is one this surface could not have received. Clamping would be worse
    // than refusing: a clamped exponent is a plausible-looking wrong price.
    for (const exponent of [-1, 5, 6, 100]) {
      expect(formatPrice(price(2_500, "EUR", exponent)), String(exponent)).toBe(
        UNRENDERABLE_AMOUNT,
      );
    }
  });

  it("refuses an exponent that is not an integer", () => {
    for (const exponent of [1.5, Number.NaN, Number.POSITIVE_INFINITY]) {
      expect(formatPrice(price(2_500, "EUR", exponent)), String(exponent)).toBe(
        UNRENDERABLE_AMOUNT,
      );
    }
  });

  it("refuses an amount past the exact-integer range rather than rounding it", () => {
    // Beyond `Number.MAX_SAFE_INTEGER` the value in hand has ALREADY lost its
    // low digits — the contract's own comment says the ceiling exists because
    // "the console is JavaScript, where an integer above that value has no exact
    // representation". Rendering it would print a price the server never
    // published, which is the one thing a price must never do.
    for (const amount of [
      Number(BigInt("9007199254740993")),
      Number.MAX_SAFE_INTEGER + 2,
      Number.POSITIVE_INFINITY,
    ]) {
      expect(formatPrice(price(amount, "EUR", 2)), String(amount)).toBe(UNRENDERABLE_AMOUNT);
    }
  });
});
