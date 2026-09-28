// The comment-block check, and the corruption it exists to catch.
//
// The five page headers this file is named after were shredded by a `//`
// substitution that collapsed each file's opening comment onto a handful of
// very long physical lines. The substituted marker sat INSIDE an already-open
// line comment, so everything after it was left in the file as live prose that
// the Vue SFC parser happened to tolerate — it only needs the top-level block
// tags that survived.
//
// What makes this a check rather than a review note is that **every gate the
// repository runs was green on it**: `vue-tsc` passed because the file still
// parsed, `eslint --max-warnings 0` passed because no rule there covers comment
// prose, and `prettier --check` passed because Prettier does not reflow
// comments. A file whose contract had been destroyed was indistinguishable from
// a file whose contract was intact by every measure the build actually takes.
// These tests assert on the checker, and the checker is what turns the next one
// into a red line rather than a review finding.
//
// The corruption is reproduced here as a string rather than as a file on disk,
// so the test says what it means: this is the shape, and the shape must fail.
//
// Note that this file, like the checker, CONTAINS the corruption it is about —
// and the checker reports it. That is not an oversight to be worked around: a
// gate that is wrong about its own header is wrong about yours, and the honest
// arrangement is that the exemptions are narrow and printed. Every occurrence
// below is either inside a string literal (rule 1 stops at the `<script>` tag,
// so a string is code and is never a violation) or a prose line quoted in a
// way that does not itself form a collapsed marker.
import { describe, expect, it } from "vitest";

import { checkLine } from "./check-comment-blocks.mjs";

/**
 * The substitution's actual output, five physical lines from four paragraphs.
 *
 * Taken from the shape rather than from memory, and every line matters: the
 * first still opens with `//` so the file's first line looks untouched, the
 * even-numbered ones are bare prose, and the `//` markers that were inserted
 * appear both mid-line and doubled where a paragraph break was.
 */
const SHREDDED = [
  "// Identity: the people in this account. // // Two lists, one screen,",
  "and nothing here invents a relationship between them. // A user is a human",
  "with a lifecycle; a key is a credential with a prefix. // // The key's",
  "plaintext exists in exactly one response, rendered once by `OneTimeSecret`.",
  '<script setup lang="ts">',
  'import { computed } from "vue";',
  "</script>",
  "",
  "<template><div /></template>",
  "",
].join("\n");

/** The restored shape, which is what the rule accepts.
 *
 * Note where the comment sits: INSIDE the script block, not above it. That is
 * not a style preference — see the `describe` on rule 1 below — and it is why
 * the fixture opens with the block tag.
 */
const INTACT = [
  '<script setup lang="ts">',
  "// Identity: the people in this account and the keys that act for it.",
  "//",
  "// Two lists, one screen, and nothing here invents a relationship between them.",
  "// A user is a human with a lifecycle; a key is a credential with a prefix.",
  "// The key's plaintext exists in exactly one response, rendered once by",
  "// `OneTimeSecret` and erased on the way out.",
  'import { computed } from "vue";',
  "</script>",
  "",
  "<template><div /></template>",
  "",
].join("\n");

/** Only the line numbers, which is what the report leads with. */
function flagged(source: string, path = "Page.vue"): readonly number[] {
  return checkLine(source, path).map((violation) => violation.line);
}

describe("a header above the first block", () => {
  it("names a comment Prettier will rewrite, and calls it what it is", () => {
    // This is the CAUSE, where the two other rules are the symptom, and it is
    // the only one of the three that fires on a file nobody has damaged. The
    // five page headers were not vandalised: they were formatted. Prettier
    // 3.9.8 prints the region above a `.vue` file's first block as one long
    // expression — there are no statements out there to format — so it joins the
    // lines, re-wraps them at the print width, and re-inserts a marker at the end
    // of every source line it joined. A 100-line comment comes back as 8 lines
    // with a trailing marker on each, which is the corruption verbatim.
    //
    // A `//` line here is therefore reported as `header outside the script
    // block` rather than as `not commented out`, because the line IS a comment
    // and saying otherwise would send a reviewer to look for missing markers
    // that are all present. The fix is a move, not a repair.
    const aboveBlock = [
      "// A contract comment, intact and correctly commented,",
      "// sitting in the one place the formatter will destroy it.",
      '<script setup lang="ts">',
      "</script>",
      "",
    ].join("\n");

    expect(flagged(aboveBlock, "Above.vue")).toEqual([1, 2]);
    expect(checkLine(aboveBlock, "Above.vue").map((violation) => violation.kind)).toEqual([
      "header outside the script block",
      "header outside the script block",
    ]);
  });

  it("distinguishes that from a line that is not a comment at all", () => {
    // The same region, a different defect: this line is live code, and it is
    // the corruption the check was originally written for. Both rules read the
    // same lines, so the `kind` is the only thing that tells a reviewer which
    // defect they are holding — and it has to be assigned per line rather than
    // per file, because a shredded file has both on the same line in places.
    const mixed = [
      "// A comment above the block.",
      "and a line that is not a comment at all.",
      '<script setup lang="ts">',
      "</script>",
      "",
    ].join("\n");

    const kinds = checkLine(mixed, "Mixed.vue");
    expect(kinds.map((violation) => violation.line)).toEqual([1, 2]);
    expect(kinds.map((violation) => violation.kind)).toEqual([
      "header outside the script block",
      "not commented out",
    ]);
  });

  it("accepts the house shape: the comment first thing inside the block", () => {
    // The other eleven screens already keep the contract comment inside the
    // script block, and `INTACT` above is that shape. This is the arrangement
    // the rule is steering toward, so the test is here to say the rule has a
    // destination — a check that only ever rejects would be one people disable.
    expect(flagged(INTACT)).toEqual([]);
  });
});

describe("a header that is not a comment", () => {
  it("names every line the substitution left as live prose", () => {
    // The whole point, and the reason this is a check and not a convention: the
    // `//` after which the prose continues does not comment it out, so the text
    // after the first marker is CODE — text the author read as a contract and
    // the compiler reads as whatever a Vue SFC parser tolerates at the top
    // level.
    expect(flagged(SHREDDED)).toEqual([1, 2, 3, 4]);

    // All four lines are reported, once each, and the `kind` of each is the
    // defect a reviewer can act on. Lines 2-4 are live code: they do not open
    // with `//` at all, so nothing about them is a comment. Line 1 DOES open
    // with `//` — it is a comment — and what is wrong with it is that it sits
    // above the block, which is the shape Prettier destroys. It also carries a
    // collapsed marker, and the orphaned rule is the one that reports it,
    // because one report per line keeps the actionable defect in view: moving
    // the block inside the script tag fixes the marker and the placement
    // together, and telling a reviewer only about the marker would send them to
    // repair a line that a formatter is about to damage again.
    const byKind = checkLine(SHREDDED, "Page.vue");
    expect(byKind.filter((v) => v.kind === "not commented out").map((v) => v.line)).toEqual([
      2, 3, 4,
    ]);
    expect(
      byKind.filter((v) => v.kind === "header outside the script block").map((v) => v.line),
    ).toEqual([1]);
  });

  it("names a line that is neither a comment nor blank, even without a marker", () => {
    // The rule is about the header being a COMMENT, not about the presence of
    // a `//`. The substitution's marker is the fingerprint, but a header that
    // has lost its markers entirely is the same defect — text the author
    // believes is commented out and the parser does not — and a check written
    // against the fingerprint alone would miss it.
    //
    // This is a `.vue` file because that is the only place the rule applies;
    // the test below is the one that says so. And note that this fixture is
    // ABOVE the block, with no marker on the first line at all: a header that
    // has lost its `//` entirely is the same defect, and a check written
    // against the marker's fingerprint rather than against "is this a comment"
    // would miss it. Nothing here is commented out, so nothing here is safe.
    const unmarked = [
      "A header whose markers are all gone.",
      "and a second line that is not a comment either.",
      '<script setup lang="ts">',
      "</script>",
      "",
    ].join("\n");
    expect(flagged(unmarked, "Unmarked.vue")).toEqual([1, 2]);
  });

  it("does not treat a blank line in a header as a violation", () => {
    // Headers separate paragraphs with a bare `//`, but an author who left a
    // blank line is still writing a comment, and a checker that fired on
    // whitespace would be a checker people switch off.
    const spaced = ['<script setup lang="ts">', "// One.", "", "// Two.", "", "</script>", ""].join(
      "\n",
    );
    expect(flagged(spaced)).toEqual([]);
  });

  it("accepts a JSDoc block and a blank comment line", () => {
    // The console's own headers include `/** … */` blocks and bare `//` lines
    // carrying no text. A rule that only understood `//` would flag the
    // majority of the tree it was written to protect.
    const jsdoc = [
      '<script setup lang="ts">',
      "/**",
      " * A block comment, whose interior lines are comments too.",
      " *",
      " */",
      "const x = 1;",
      "</script>",
      "",
    ].join("\n");
    expect(flagged(jsdoc)).toEqual([]);
  });

  it("does not apply the header rule outside a `.vue` file", () => {
    // A line of stray prose in a TypeScript module is a PARSE ERROR, loudly
    // and immediately — which is exactly why the corruption could survive only
    // in a `.vue` file. The rule earns its keep where the parser is forgiving;
    // applied to `import` statements it would have been switched off on day one.
    const module = ["// A header.", 'import { computed } from "vue";', ""].join("\n");
    expect(flagged(module, "Module.ts")).toEqual([]);
  });
});

describe("a header collapsed onto one line", () => {
  it("names the lines carrying a marker in the middle of prose", () => {
    // The other half of the same substitution, and why the check is not only
    // "is this line a comment". A block reflowed onto one physical line stays
    // commented out — rule 1 is satisfied, which is why the header above is the
    // only half that caught the real corruption — and is still the unreadable
    // wall with mid-sentence markers that a reviewer has to unpick. A marker
    // with prose on BOTH sides is the fingerprint: one that only follows text
    // is the normal way a wrapped comment ends.
    const collapsed = [
      '<script setup lang="ts">',
      "// One: a sentence. //",
      "// Two: the next sentence. //",
      "// Three: the last one.",
      "</script>",
      "",
    ].join("\n");

    expect(flagged(collapsed, "Collapsed.vue")).toEqual([2, 3]);
  });

  it("does not mistake a URL for a collapsed block", () => {
    // The case that keeps rule 2 from being a search for a two-character
    // string, and the case that would have bitten first: the console's own
    // comments quote the paths and origins this console talks to, so a
    // repository full of URLs and a check for mid-line `//` are on a collision
    // course from the first commit. The `//` in a URL is preceded by `:` or by
    // a space that belongs to the path — both of which mean the marker is part
    // of the address, not a comment marker that landed in the prose.
    const urls = [
      '<script setup lang="ts">',
      "// The dev server proxies the console-api's whole route table, so a",
      "// request for //api/v1/subscriptions is rewritten rather than refused.",
      "// The API is served from https://gateway.example.test, not from here.",
      'const ORIGIN = "https://gateway.example.test";',
      'const PATH = "//api/v1/subscriptions";',
      "</script>",
      "",
    ].join("\n");

    expect(flagged(urls, "Urls.vue")).toEqual([]);
  });

  it("still reports a collapsed block that follows a URL", () => {
    // The other side of that fix, and the one that says the URL handling is a
    // filter rather than an escape hatch. Stripping the URL out first is only
    // safe because the marker that FOLLOWS it has prose on both sides and is
    // therefore still found — which is what this line is.
    const withUrlThenMarker = [
      '<script setup lang="ts">',
      "// See //api/v1/subscriptions for the contract. // It is a real read.",
      "</script>",
      "",
    ].join("\n");

    expect(flagged(withUrlThenMarker, "Mixed.vue")).toEqual([2]);
  });

  it("does not report a comment line that simply ends in a marker", () => {
    // `//` in column zero has nothing to its left, so it is never a collapsed
    // marker. Every ordinary comment in the tree ends this way at some point,
    // and a rule that fired on them would bury the real report.
    expect(flagged(["// A trailing marker.", "//", ""].join("\n"), "Trailing.ts")).toEqual([]);
  });
});

describe("what the check is not", () => {
  it("passes a header that is what it claims to be", () => {
    // A check that only ever sees the broken form is indistinguishable from a
    // check that fires on everything, and this is the half that says the rule
    // is a rule rather than a prohibition on writing comments. The restored
    // headers are the exact shape this asserts against.
    expect(flagged(INTACT)).toEqual([]);
  });

  it("leaves a comment inside a template alone, because indented is still a comment", () => {
    // A block comment inside a `<template>` is indented, and it is a comment.
    // Requiring the marker in column zero would have flagged every one of the
    // console's template comments.
    const indented = [
      "<template>",
      "  <div>",
      "    <!--",
      "      A comment in a template, indented to the markup.",
      "    -->",
      "  </div>",
      "</template>",
      "",
    ].join("\n");

    expect(flagged(indented, "Template.vue")).toEqual([]);
  });

  it("has no opinion on width", () => {
    // Prettier does not reflow comments, so width is a human judgement. A
    // check that started enforcing it would be a second formatter fighting the
    // first, and the first does not even run on this text.
    const narrow = ['<script setup lang="ts">', "// A.", "// B.", "</script>", ""].join("\n");
    expect(flagged(narrow, "Narrow.vue")).toEqual([]);
  });
});
