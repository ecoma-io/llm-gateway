// The checker is plain JavaScript on purpose — it is a build gate, it has no
// dependency on Vue, and it runs under `node` with no build step. It is also
// consumed by a TypeScript spec, so its two exports are declared here rather
// than inferred, because `vue-tsc` treats an untyped `.mjs` import as `any` and
// `noImplicitAny` then fails every use of it in the spec.
//
// `Violation` is spelled out rather than inferred so the `kind` a caller
// switches on is a closed union. That is the whole contract of the check: three
// rules, and a report says which one fired.
/**
 * The three ways a file's comment blocks can be wrong, kept apart because they
 * are different defects with different fixes.
 *
 * `header outside the script block` is a `.vue` file whose opening comment sits
 * above the first top-level block rather than inside it, which is the shape
 * Prettier 3.9.8 rewrites — it joins those lines and re-inserts a marker at the
 * end of every one, which is how the five shredded headers were produced. It is
 * a prevention rather than a repair, and it is the only one of the three that
 * fires on an otherwise pristine file.
 *
 * `not commented out` is a `.vue` file's header containing a line that is
 * neither a comment nor blank — text the author read as a contract and the
 * parser reads as code. `collapsed comment block` is a line that IS a comment
 * but carries a second `//` in the middle of its prose, which is what a header
 * reflowed onto one physical line looks like: still commented, still unreadable.
 */
export type ViolationKind =
  "header outside the script block" | "not commented out" | "collapsed comment block";

/** One problem with one line of one file, named by where it is. */
export interface Violation {
  /** The file, relative to the checker, with `/` separators. */
  readonly path: string;
  /** The 1-based physical line, so a reviewer is told where to look. */
  readonly line: number;
  /** The line's own text, trimmed and cut, so a report identifies itself. */
  readonly text: string;
  /** Which of the three rules fired. */
  readonly kind: ViolationKind;
}

/** Check one file's source and return what is wrong with it, in line order. */
export function checkLine(source: string, path: string): Violation[];

/** Run over the console's own sources. Returns the process exit code. */
export function main(): number;
