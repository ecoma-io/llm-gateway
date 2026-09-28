// The one place an optional instant becomes the text a reader is shown.
//
// The contract marks most instants optional and a row that renders a missing one
// as an empty cell is a row a reader cannot distinguish from one with no data
// — so the three states are three strings, stated here once:
//
//   - a value: the server's own timestamp, character for character, and never
//     re-serialised. A console that parsed a timestamp into a local time and
//     rendered it back has introduced a second formatting authority, and one
//     whose answer changes with the reader's machine rather than with the data.
//   - `null`: the contract's "this did not happen yet" — a pending
//     subscription's cancellation, a closed bucket's closure. It reads as a
//     fact about the row rather than as missing data.
//   - `undefined`: the field was not sent. It is not rendered at all.
//
// The word is different in each case on purpose: an em dash is a placeholder
// where a value would go and a reader should not mistake it for a zero, while
// "Not yet" is a sentence about the row. `MoneyAmount` uses the same em dash
// for the same reason, and the two are named in each other's comments.

/** The placeholder for a field the contract did not send. */
export const ABSENT = "—";

/** The word for a `null` instant: a fact about the row, not a missing value. */
export const NOT_YET = "Not yet";

/** How an optional instant is read. */
export type Instant = string | null | undefined;

/**
 * The cell's text, or `undefined` when the field was not sent and the cell
 * should therefore render nothing at all.
 */
export function instantText(value: Instant): string | undefined {
  if (value === undefined) return undefined;
  return value === null ? NOT_YET : value;
}

/**
 * The timestamp inside a contract instant, for the `<time datetime="…">` that
 * makes the machine-readable value available to anything reading the markup.
 *
 * It is the value itself, never a re-formatting: `datetime` takes exactly the
 * format the contract sent, which is an RFC 3339 instant, and a parsed and
 * re-rendered date here would be a timestamp that can disagree with the one it
 * claims to describe.
 */
export function instantDateTime(value: Instant): string | undefined {
  return typeof value === "string" ? value : undefined;
}
