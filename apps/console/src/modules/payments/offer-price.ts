// What a top-up offer is called, in the one place that decides.
//
// This is the exception to `modules/money/money.ts`, and the exception is the
// contract's rather than this file's. `Money` carries `minor_units` and NO
// currency and no exponent, so the console renders it as bare grouped digits
// with the unit named in prose — the rule that module's header states. A
// `TopUpOffer` is a different schema and carries both: `currency`, because an
// offer's currency IS the currency of the payment it opens, and
// `minor_unit_exponent`, "the exponent the server uses to place a decimal point
// when a human-facing amount is rendered", which "travels so a client can do the
// same without carrying its own currency table".
//
// So this file places a decimal point, and it does so ONLY from the exponent the
// server sent. It holds no currency table, assumes no two decimal places (a
// yen offer arrives with exponent 0 and must render as whole yen, not as
// hundredths of one), and paints no symbol: the currency is shown as the
// contract's own ISO code, which is a name and not a glyph this console chose.
//
// A `PaymentIntent` carries the same three fields, and for the same reason —
// the contract's `minor_unit_exponent` description says the exponent travels
// with a payment so a client need not look it up in a price list that may have
// changed since. So the same function renders a stored payment's amount, and
// that is why its name is about a price rather than about an offer.
//
// The arithmetic is a STRING operation and never a division. `amount_minor_units
// / 10 ** exponent` is the obvious line and it is the wrong one: minor units are
// an int64 and the transport gives JavaScript a `number`, so dividing before
// rendering is how a large amount loses its last digits to a float. The digits
// are sliced instead, which is exact at any size the contract admits.
import { UNRENDERABLE_AMOUNT } from "@/modules/money/money";
import type { TopUpOffer } from "@ecoma-io/llm-gateway-console-api-client";

/** The fields of a price this file reads. A price is not a chooser. */
type Priced = Pick<TopUpOffer, "amount_minor_units" | "currency" | "minor_unit_exponent">;

/**
 * The exponent's declared range, mirrored from the contract's own bounds
 * (`minimum: 0`, `maximum: 4`). A value outside them is one this surface could
 * not have received, so it is refused rather than clamped: a clamped exponent
 * is a plausible-looking wrong price.
 */
const MAX_EXPONENT = 4;

/**
 * The amount of a price, with the decimal point placed and the currency left
 * off: `10.00`, `1,250`, or the unrenderable sentinel.
 *
 * It is a separate function from `formatPrice` because a table can show the
 * currency in a column of its own, and a cell that repeated the code beside
 * every amount would be saying one thing twice.
 *
 * The sentinel is the same one `money.ts` uses, and for the same reason: an
 * amount that cannot be rendered exactly must not be rendered approximately.
 * The contract bounds an amount by `Number.MAX_SAFE_INTEGER` precisely so this
 * is unreachable in a well-behaved deployment — and the guard is here anyway,
 * because the one thing a price must never do is look right while being wrong.
 */
export function formatPriceAmount(price: Priced): string {
  const exponent = price.minor_unit_exponent;
  if (!Number.isInteger(exponent) || exponent < 0 || exponent > MAX_EXPONENT) {
    return UNRENDERABLE_AMOUNT;
  }
  const units = price.amount_minor_units;
  if (!Number.isSafeInteger(units)) return UNRENDERABLE_AMOUNT;

  // `padStart` is what makes exponent 0 and exponent 4 the same code path: a
  // 3-unit amount at exponent 2 is `003` -> `0.03`, and at exponent 0 it is `3`.
  const digits = Math.abs(units)
    .toString()
    .padStart(exponent + 1, "0");
  const cut = digits.length - exponent;
  const whole = digits.slice(0, cut).replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  const fraction = exponent === 0 ? "" : `.${digits.slice(cut)}`;
  const sign = units < 0 ? "-" : "";

  return `${sign}${whole}${fraction}`;
}

/**
 * One price, as the sentence a chooser shows: `10.00 EUR`, `1,250 JPY`, or the
 * unrenderable sentinel — the amount above, then the currency the server named.
 *
 * An unrenderable amount is not joined to a currency, because `10.00 EUR` with
 * the `10.00` replaced by a sentinel would read as a currency whose amount is a
 * word.
 */
export function formatPrice(price: Priced): string {
  const amount = formatPriceAmount(price);
  if (amount === UNRENDERABLE_AMOUNT) return UNRENDERABLE_AMOUNT;
  return `${amount} ${price.currency}`;
}
