// The console's own check on its own comment blocks.
//
// Every module and screen under `apps/console/src` opens with a comment that
// states the contract the file holds to. Those are the longest comments in the
// codebase and they are the first thing a reviewer reads, and for a long time
// nothing checked them — which is how five of them were destroyed. Because the
// substituted `//` marker sat INSIDE an already-open line comment, the text
// after it was not commented out: it was left in the file as bare prose that
// the Vue SFC parser happened to tolerate, because it only needs the top-level
// block tags that survived.
//
// **Every automated gate was green on it.** `vue-tsc` passed, because the file
// still parsed. `eslint --max-warnings 0` passed, because no rule there covers
// comment prose. `prettier --check` passed — and worse, `prettier --write` is
// what PRODUCED the damage, which rule 1 below is about. A file whose contract
// had been shredded was indistinguishable from a file whose contract was
// intact, by every measure the repository actually runs, and it took a human
// reading the file to see it. This check is here so the next one is a red build
// instead.
//
// The rules are the mechanical half of the corruption, and there are three
// because the substitution broke three things at once:
//
//   1. **A comment before a block marker is one Prettier will destroy.** A `.vue`
//      file's opening comment has to live INSIDE its first top-level block, not
//      above it. Outside the blocks, an SFC has no statements, so Prettier
//      formats the file as one long expression: it joins the lines, re-wraps
//      them, and re-inserts a marker at the end of every source line it joins.
//      That is the same substitution the corruption was made of, and it is not
//      a hazard to be remembered — it is a round trip. The other eleven screens
//      already keep the comment inside the script block; these five are the
//      ones that did not, which is why a formatter run is all it took to lose
//      them. Rule 1 is the one that stops the next run.
//   2. **A header is a comment, so every line of it is commented.** A line of a
//      file's opening block that is neither a comment line nor blank is text
//      the author read as a contract and the compiler reads as code. The `//`
//      after which prose resumes, mid-sentence, is the exact fingerprint.
//   3. **A `//` surrounded by whitespace is a line marker, not text.** A marker
//      on the same physical line as the prose before it is a collapsed block:
//      still commented, still an unreadable wall. Whitespace on BOTH sides is
//      what distinguishes it from the `//` in `https://`, which is the case that
//      keeps this from being a search for a two-character string.
//
// It is deliberately not a comment STYLE rule. Nothing here decides how a
// comment should be worded, wrapped or punctuated; it decides only whether the
// text a comment claims to hold is actually commented out and is in a place the
// formatter will leave alone, both of which are correctness questions about
// what the compiler, the formatter and the reader are looking at.
//
// What it does NOT do, and the reason rule 2 is scoped to `.vue`:
//   - It does not check indentation or width. Prettier does not reflow a
//     comment, so width is a human judgement here and this file has no opinion
//     on it. A checker that started enforcing it would be a second formatter
//     fighting the first, and the first does not even run on this text.
//   - It does not apply the header rule to `.ts` or `.mjs` files. A line of
//     stray prose in a TypeScript module is a PARSE ERROR, loudly and
//     immediately, which is exactly why the corruption could survive only in a
//     `.vue` file — where the SFC parser tolerates text outside the top-level
//     blocks. The rule earns its keep where the parser is forgiving, and a rule
//     that fired on `import` statements would have been switched off on day one.
//   - It does not judge the CONTENT. A comment that is intact and says nothing
//     useful passes here, because usefulness is the reviewer's call and this is
//     a build gate, not a reviewer.
//
// One detail of rule 3 is worth stating at the top rather than discovering at
// the bottom: this file quotes the corruption, and a quote of a collapsed block
// CONTAINS a collapsed marker, so the checker reports its own prose. That is
// correct — a gate that is wrong about its own header is wrong about yours —
// and the fix is to write the examples the way the rule accepts, which is what
// the examples here do. A gate whose only demonstration is a false positive is
// a gate that gets switched off, and a real report buried under one is a gate
// that stops being read.
import { readdirSync, readFileSync, statSync } from "node:fs";
import { dirname, extname, join, relative, sep } from "node:path";
import { fileURLToPath } from "node:url";

// The console's own `src`, from this file rather than from a configured root:
// a checker whose root is a constant anyone can edit is a checker whose scope a
// later change can quietly move, and the scope is the whole claim.
const HERE = dirname(fileURLToPath(import.meta.url));
const CONSOLE_SRC = join(HERE, "..", "..");

/**
 * The block markers that make a `.vue` file what it is.
 *
 * Text outside the top-level blocks is what the SFC parser tolerates and what
 * Prettier reformats, so these are the anchors both `.vue` rules stop at.
 */
const BLOCK_MARKERS = ["<script", "<template", "<style"];

/**
 * A line that is a comment, in either style the console writes.
 *
 * The bare-asterisk form is in the set because a JSDoc block's interior lines
 * start with one and are as much a comment as the opener. Writing the closing
 * marker out in this sentence is deliberately avoided: a block comment ends at
 * the first one it contains, and this file would then parse its own header as
 * code — which is a smaller cousin of the defect this checker exists to catch.
 */
const COMMENT_LINE = /^\s*(\/\/|\/\*|\*|\*\/)/;

/**
 * A `//` that is a line marker which landed in the MIDDLE of prose: a
 * non-space character, then whitespace, then the marker, then whitespace or
 * end of line.
 *
 * All four parts are load-bearing, and the first two are the ones a first draft
 * gets wrong. Requiring only whitespace on both sides matches the OPENING
 * `//` of every indented comment in the tree — an indented comment is a
 * comment, and a rule that flags those is a rule switched off on day one. So
 * the marker must be preceded by a non-space character and by whitespace, in
 * that order: prose, a space, then a marker that has no business being there.
 *
 * The trailing `(?=\s|$)` is what keeps a path mid-sentence — "see
 * `//api/v1/subscriptions` for the contract" — out of it, and `URL_MARKER`
 * removes the longer cases before this ever runs.
 */
const COLLAPSED_MARKER = /(?<=\S\s)\/\/(?=\s|$)/;

/**
 * A `//` that belongs to a URL or to a path, which is the one legitimate thing
 * a two-character marker is part of.
 *
 * The `(?<=\S)` is load-bearing and is the whole reason this strip is safe to
 * run over a comment line: a marker in column zero, or one that follows a
 * space, has no non-space character before it and is therefore never removed.
 * Without the lookbehind this replaced the OPENING `//` of every comment in the
 * tree and left nothing for the collapsed-marker search to find — a check that
 * silently stops checking, which is worse than not having it.
 *
 * It runs before that search so the two rules cannot consume each other:
 * `"… // // Two lists …"` is a marker that follows a marker, and the first one
 * is the one being reported.
 */
const URL_MARKER = /(?<=\S)\/\/[^\s"'`)]*/g;

/**
 * The lines of a `.vue` file that precede its first top-level block.
 *
 * Rule 1 says there must not be any: Prettier prints that region as one long
 * expression and hands it back re-wrapped with a marker at the end of every
 * line it joined. Rule 2 says that if there are any, every one of them must be
 * commented. Both read the same region, and both stop at the first block
 * marker, so a file that gets this right is checked from the same place the
 * other way round.
 */
function beforeFirstBlock(lines) {
  const first = lines.findIndex((line) => BLOCK_MARKERS.some((marker) => line.startsWith(marker)));
  return first === -1 ? lines : lines.slice(0, first);
}

/** The lines above the first block that are neither a comment nor blank.
 *
 * Every one of them is wrong, and the KIND is the part that tells a reviewer
 * which of the two defects they are looking at, so the two are separated here
 * rather than by two identical passes in `checkLine`. A first draft ran this
 * region twice with the same filter under two names, once for each rule; because
 * `record` keeps only the first report per line, the second could never fire.
 * That is the same failure as the `URL_MARKER` lookbehind above — a check that
 * silently stops checking is worse than no check, because the build is green.
 *
 * So: a line that is a comment is a header Prettier will REWRITE, and a line
 * that is not is a header that is not a comment AT ALL, which is the corruption
 * proper. Both are reported, on the same lines, under different names.
 */
function orphanedHeaderLines(lines) {
  const out = [];
  for (const [index, line] of beforeFirstBlock(lines).entries()) {
    if (line.trim() === "") continue;
    out.push({
      line: index + 1,
      kind: COMMENT_LINE.test(line) ? "header outside the script block" : "not commented out",
    });
  }
  return out;
}

/** The lines of any file that carry a collapsed marker. */
function collapsedLines(lines) {
  const out = [];
  for (const [index, line] of lines.entries()) {
    // Only a COMMENT line can hold a collapsed comment block, and saying so is
    // what keeps this rule off ordinary trailing comments — `expect(x).toBe(1);
    // the number is one` is a perfectly good comment, and a rule that reported
    // every one of them in the tree would be reported as broken and then
    // disabled. A collapsed block is a whole paragraph squeezed onto lines
    // that were each meant to be a line of prose, so it can only have happened
    // to a line that IS prose.
    if (!/^\s*\/\//.test(line)) continue;
    // The URL is taken out FIRST, so a marker that merely follows a path is
    // still the thing being reported. See `URL_MARKER`.
    const withoutUrls = line.replace(URL_MARKER, " ");
    if (COLLAPSED_MARKER.test(withoutUrls)) out.push(index + 1);
  }
  return out;
}

/**
 * Check one file's source, and return what is wrong with it.
 *
 * `path` is used for two things and both are the file's IDENTITY rather than
 * anything read from the source: its extension decides whether the `.vue` rules
 * apply, and it is the name a violation is reported under. It is an argument
 * because a checker that only ever read one directory would not be a rule about
 * the console, it would be a fact about the console.
 */
export function checkLine(source, path) {
  const lines = source.split("\n");
  const found = new Map();

  const record = (line, kind) => {
    // One report per line: a collapsed header can be both an orphaned line and
    // a collapsed block, and telling a reviewer the same line twice is noise.
    if (!found.has(line)) {
      found.set(line, { path, line, text: lines[line - 1].trim().slice(0, 72), kind });
    }
  };

  if (extname(path) === ".vue") {
    for (const { line, kind } of orphanedHeaderLines(lines)) record(line, kind);
  }
  for (const line of collapsedLines(lines)) record(line, "collapsed comment block");

  return [...found.values()].sort((a, b) => a.line - b.line);
}

function readAll(dir) {
  const out = [];
  for (const entry of readdirSync(dir)) {
    const path = join(dir, entry);
    if (statSync(path).isDirectory()) {
      out.push(...readAll(path));
    } else {
      out.push(path);
    }
  }
  return out;
}

/** Run over the console's own sources, and report. Returns the process code. */
export function main() {
  const files = readAll(CONSOLE_SRC).filter((path) =>
    [".vue", ".ts", ".mjs"].includes(extname(path)),
  );
  const violations = files.flatMap((path) =>
    checkLine(readFileSync(path, "utf8"), relative(HERE, path).split(sep).join("/")),
  );

  if (violations.length === 0) {
    console.log(
      `comment blocks: ${files.length} files, every header inside a block and every line commented out`,
    );
    return 0;
  }

  console.error("the rendered source breaks the console's comment-block check:");
  for (const violation of violations) {
    console.error(`  - ${violation.path}:${violation.line} (${violation.kind}): ${violation.text}`);
  }
  console.error(
    "\n  A comment line that does not start with `//` is live code, and a `//`\n" +
      "  in the middle of a line is a collapsed comment block: the text after it\n" +
      "  is not commented out. Restore the block rather than deleting the line,\n" +
      "  and keep a screen's opening comment inside its <script setup> block —\n" +
      "  Prettier reformats anything above the first block and will shred it.",
  );
  return 1;
}

// Run only when invoked directly, so a spec can import the checker without the
// import deciding the build's exit code.
if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  process.exit(main());
}
