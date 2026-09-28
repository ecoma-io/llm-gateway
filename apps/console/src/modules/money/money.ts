// The money renderer's whole vocabulary (ADR 0012 §4, the money rules).
//
// Three properties hold this file to the contract, and each is a property a
// test can read rather than a rule a reviewer must remember:
//
//   - **It formats and nothing else.** There is no exported arithmetic. The
//     whole module's only operation on a number is grouping digits and
//     placing a sign, and a `Balances` is three values that arrive together
//     from the server and are rendered as three values. Nothing here sums,
//     subtracts, or derives `available = settled − held`; the database already
//     checks that identity and the console's copy of it would be an
//     arithmetic with no authority behind it.
//   - **It names no currency.** The `Money` schema in the contract carries
//     `minor_units` and NO currency field (the settlement currency lives in
//     configuration above the domain — issue #63). So the formatter renders
//     bare minor units and the UI says "minor units" in words. A number
//     presented as money with a currency symbol would assert a currency this
//     system does not have, which is exactly the defect the contract's own
//     description warns a client into making.
//   - **It is int64-safe.** A minor-unit amount is an int64. The transport
//     gives JavaScript a `number`, which is exact to 2^53; the formatter must
//     not do `toLocaleString` on a value it has not bounded, because a
//     grouping call on a float is where a money figure quietly loses its last
//     digits. The guard is explicit below.
//
// The sign is a *presentation of the sign the server sent*. A `delta` carries
// a meaningful sign; a `balance` never does, because this plane refuses to
// store a negative balance. The renderer does not decide which kind it is
// holding — a caller that wants a leading `+` for a delta says so — it only
// shows the sign that is there.
import type { Money } from "@ecoma-io/llm-gateway-console-api-client";

/**
 * The largest amount this formatter will render as an exact integer.
 *
 * JavaScript's `number` is exact to 2^53 − 1; the contract's amount is an
 * int64, which is larger. A value beyond this bound is one the transport
 * could not have carried exactly, so rendering its digits as if they were
 * money would be a lie with a currency-shaped wrapper on it. The formatter
 * refuses loudly instead: the amount is rendered as "too large to display
 * exactly" rather than as a plausible-looking wrong number.
 */
export const MAX_EXACT_MINOR_UNITS = Number.MAX_SAFE_INTEGER;

/**
 * Formats one `Money` — or a bare `minor_units` integer — for display.
 *
 * `minorUnits` is the int64 the server sent. The result groups the digits
 * with a thin separator for readability and prefixes a negative with an ASCII
 * minus. No currency, no unit suffix, no exponent: the display is bare minor
 * units and the surrounding UI names the unit in prose.
 *
 * It never throws and never guesses. An amount beyond the exact-integer bound,
 * or one that is not an integer at all, is rendered as the sentinel below so
 * a caller cannot read a wrong number as a right one.
 */
export const UNRENDERABLE_AMOUNT = "—";

/**
 * Renders a signed or unsigned minor-unit amount as grouped digits.
 *
 * Kept separate from the `Money` overload so a caller holding the two
 * `Money` fields of a `LedgerEntry` renders each without unwrapping them by
 * hand, and so the arithmetic stays impossible: this function takes one
 * number and returns a string. There is no variant of it that takes two.
 */
export function formatMinorUnits(minorUnits: number): string {
  if (!Number.isSafeInteger(minorUnits)) return UNRENDERABLE_AMOUNT;
  const sign = minorUnits < 0 ? "-" : "";
  // `abs` before grouping so the separator logic never sees the sign.
  const digits = Math.abs(minorUnits).toString();
  const grouped = digits.replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  return `${sign}${grouped}`;
}

/**
 * Formats a `Money` for display: grouped minor units, no currency.
 *
 * The `Money` wrapper is accepted so a screen can hand a `Balances` field
 * straight in and cannot forget to unwrap it, and so the "unrenderable"
 * guard lives in exactly one place.
 */
export function formatMoney(money: Money | undefined | null): string {
  if (!money) return UNRENDERABLE_AMOUNT;
  return formatMinorUnits(money.minor_units);
}

/**
 * Formats a signed minor-unit amount with an explicit leading `+` on a
 * positive value — the shape a ledger leg's `delta` reads best in.
 *
 * A `delta`'s sign is meaningful and an operator reads "in" and "out" from
 * it; a leading `+` is the third channel beside the word and the colour, so
 * a greyscale reader still sees the direction. `formatMinorUnits` is used
 * for a balance, which never carries a sign, precisely so a minus on a
 * balance would stand out as the anomaly it is.
 */
export function formatSignedMinorUnits(money: Money | undefined | null): string {
  const rendered = formatMoney(money);
  if (rendered === UNRENDERABLE_AMOUNT || rendered.startsWith("-")) return rendered;
  return rendered === "0" ? rendered : `+${rendered}`;
}

/**
 * Formats a `Balances` as the three independent figures the contract says
 * they are — `settled`, `held`, `available` — each rendered from its own
 * server value.
 *
 * The function takes the three and returns the three. It is a view over the
 * response, not a step in a calculation: `available` here is the string
 * rendered from `balances.available.minor_units`, and no branch in this file
 * reads `settled` to produce it. That is the whole point — the three
 * identities `domain/accounting` checks are the server's to keep, and a
 * client that recomputed one would be a second, unaudited ledger.
 */
export function formatBalances(
  balances: { settled: Money; held: Money; available: Money } | undefined,
): { settled: string; held: string; available: string } {
  return {
    settled: formatMoney(balances?.settled),
    held: formatMoney(balances?.held),
    available: formatMoney(balances?.available),
  };
}
