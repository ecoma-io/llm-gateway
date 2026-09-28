/**
 * The console's automated accessibility gate, and the axe vocabulary it runs.
 *
 * ADR 0012 §7 promises a gate and this file is the part of it that was missing:
 * the rule list was on the roster and nothing consumed it. The shape follows
 * `lib/arch/roster.ts` deliberately — that module is the console's other
 * DOM-level gate, it holds its rule list in a source file rather than in a
 * config file, and it reads the RENDERED DOCUMENT rather than the source. Both
 * properties are load-bearing:
 *
 *  - **The rule list is Loom's, imported, never restated.** `BROWSERLESS_RULES`
 *    comes from `@ecoma-io/loom/a11y` and this file does not extend, filter or
 *    reorder it. A gate that kept its own copy of the list would be a second
 *    opinion about what counts as a violation, and the drift Loom's own docblock
 *    warns about is precisely a list that was restated somewhere. What this file
 *    adds is the partition BETWEEN the two tiers — an audit that silently ran
 *    browser-required rules in jsdom would be claiming a verdict jsdom cannot
 *    reach, which is worse than not running them.
 *  - **The rules run against the rendered document.** A screen can import a
 *    component and hide it behind a `v-if` that is false at mount; a source
 *    scan sees the import and passes. `axe-probe.spec.ts` exists because this
 *    claim is only worth something if the gate has been watched to fail.
 *
 * ── Why `runOnly` and not `rules` ────────────────────────────────────────────
 *
 * `BROWSERLESS_RULES` is a readonly array of rule ID STRINGS — verified against
 * the installed package, not assumed: `typeof BROWSERLESS_RULES[0] === "string"`,
 * 51 entries, disjoint from the 17 `BROWSER_REQUIRED_RULES`, every id resolving
 * against `axe.getRules()`. axe's own `options.rules` is a MAP keyed by id
 * (`RuleObject`, `{ [ruleId]: { enabled } }`), not an array; handing it an array
 * makes axe iterate its indices and throw ``unknown rule `0` in options.rules``.
 * So there are two ways to run an id array, and this file takes the one Loom's
 * own gates take: `runOnly: { type: "rule", values: [...] }`.
 *
 * `runOnly` is the correct spelling here for a second reason, and it is not a
 * style preference. axe's normalizer treats a `runOnly` value as a rule to RUN
 * WHATEVER ITS `enabled` FLAG SAYS, while a tag-type `runOnly` EXCLUDES the
 * WCAG-tagged rules axe ships disabled. One member of this list,
 * `aria-roledescription`, is exactly such a rule: axe-core deprecated it with no
 * successor and ships it `enabled: false`. Under a tag-selected run it would
 * never execute; under this one it does. That is Loom's documented intent, and
 * it is the difference between 51 rules being configured and 50 silently not
 * running.
 *
 * ── What 51 configured rules means in practice ──────────────────────────────
 *
 * Configured is not the same as executed, and the difference is worth stating
 * once here rather than leaving a reader to assume a number. A rule lands in
 * `inapplicable` when it does not match anything on the page — a page with no
 * `<dl>` makes `definition-list` inapplicable, not passed — so the count of
 * rules that actually JUDGED a screen is well below 51 on any given screen and
 * is reported per screen in the sweep's own output. Two rules additionally read
 * the document root rather than a subtree (`aria-hidden-body`,
 * `nested-interactive`) and are inapplicable under an element context; see
 * `audit` below for why that is the right trade and what it costs.
 */
import type { NodeResult, Result, RunOnly } from "axe-core";
import { BROWSER_REQUIRED_RULES, BROWSERLESS_RULES } from "@ecoma-io/loom/a11y";

/**
 * The run, as axe takes it: every browserless rule, named by id, with none of
 * the browser-required tier asked for.
 *
 * Exported so a reader can see the whole run configuration in one place and a
 * test can assert what it is — `a11y-gate.spec.ts` asserts the partition is
 * still Loom's and that no browser-required rule leaked in, which is the only
 * thing standing between this file and a gate that quietly claims `color-contrast`
 * in jsdom (where it returns `cantTell` on every node and would pass a page whose
 * contrast is unreadable).
 */
export const RUN_ONLY: RunOnly = { type: "rule", values: [...BROWSERLESS_RULES] };

/**
 * The rules this gate deliberately does NOT run, and why, in one sentence a
 * failure message can print.
 *
 * `color-contrast` and `target-size` are the two an operator is most likely to
 * assume are covered, and both are in this array: `color-contrast` reads the
 * computed-color pipeline and `target-size` reads hit-test rectangles, neither
 * of which jsdom implements. Running them here would return `cantTell`/zero
 * rectangles and report a pass — the silent false-pass Loom's own docblock calls
 * out by name. Claiming WCAG 2.2 AA on a green run of this gate would be false
 * for exactly these seventeen.
 */
export const NOT_RUN = BROWSER_REQUIRED_RULES;

/** The outcome of one screen's audit, kept whole so a failure can print all of it. */
export interface Audit {
  readonly violations: readonly Result[];
  /** `incomplete` is axe saying "I could not decide", which is not a pass. */
  readonly incomplete: readonly Result[];
  readonly passes: readonly Result[];
}

/** One offending node, rendered as a line a reviewer can act on. */
interface Offence {
  readonly rule: string;
  readonly impact: string;
  readonly help: string;
  readonly target: string;
  readonly html: string;
  readonly summary: string;
}

/** The rules axe declined to decide on, as bare ids — the shape a roster compares. */
export function undecidedIds(audit: Audit): string[] {
  return audit.incomplete.map((result) => result.id);
}

function selectorOf(node: NodeResult): string {
  // `target` is a selector array; the empty case means axe matched something it
  // could not name (a pseudo-element, a shadow boundary), and an empty line is
  // more honest than a fabricated one.
  return Array.isArray(node.target) ? node.target.join(" >> ") : String(node.target);
}

function offencesOf(result: Result): Offence[] {
  return result.nodes.map((node) => ({
    rule: result.id,
    impact: result.impact ?? "unknown",
    help: result.help,
    target: selectorOf(node),
    html: node.html,
    summary: node.failureSummary ?? "",
  }));
}

/**
 * Run the gate over a document and return everything axe said.
 *
 * The context is an ELEMENT, and deliberately not `document`. Two of the
 * browserless rules — `document-title` and `html-has-lang` — judge the host
 * document rather than anything a screen renders, and the console's `<title>`
 * and `lang` live in `index.html`, which vitest does not load. Measured, not
 * assumed: run against `document` in this environment they report violations
 * (`document-title`, `html-has-lang`) that exist in no browser and describe
 * nothing about any screen; run against a mounted element they are simply
 * inapplicable and never appear in any result group.
 *
 * Auditing the element is therefore not a filter — nothing is dropped from the
 * results after the fact, so a genuine violation could never be swallowed by the
 * same rule that excludes the host-document pair. What it does cost is
 * `aria-hidden-body` and `nested-interactive`, the two rules whose checks read
 * the document root rather than a subtree; both are listed in the rule count
 * above and neither fires here. `nested-interactive` is the one worth naming,
 * because it is a rule about a control inside a control and the console has
 * exactly the markup it judges — a `Button` inside a `RouterLink`-styled
 * element, or a row made clickable. That is a coverage gap this gate does not
 * close, and it is stated rather than papered over.
 */
export async function audit(element: Element): Promise<Audit> {
  // `axe-core` is CommonJS with a single `export =`, so under an ESM bundler
  // the namespace object carries it as `.default` and the named `axe` is
  // absent. Both spellings are accepted rather than one guessed at, because a
  // gate that throws `Cannot read properties of undefined` on every screen is a
  // gate that reports nothing and looks broken rather than absent.
  const imported = (await import("axe-core")) as unknown as {
    default?: typeof import("axe-core");
    axe?: typeof import("axe-core");
  };
  const axe = imported.axe ?? imported.default;
  if (!axe) throw new Error("axe-core resolved without a usable export");
  const results = await axe.run(element, { runOnly: RUN_ONLY });
  return {
    violations: results.violations,
    incomplete: results.incomplete,
    passes: results.passes,
  };
}

/**
 * The single sentence a screen's spec prints when this fails, in the shape
 * `lib/arch/roster.ts` already established: the rule, the element, and what to
 * do — not a count.
 *
 * VIOLATIONS ONLY. An `incomplete` result is axe declining to decide, which is
 * neither a pass nor a defect, and folding it in here would make this function
 * useless for both jobs: a screen could not be asserted violation-free while a
 * rule is legitimately undecided, and a decision nobody has taken would read as
 * a failure to fix. `describeIncomplete` prints the second thing separately, and
 * the sweep keeps a named roster of exactly which screens carry which
 * undecided rules — so neither is hidden, and each is asserted on its own terms.
 */
export function describe(audit: Audit, screen: string): string {
  if (audit.violations.length === 0) return "";
  const lines: string[] = [
    `${screen} breaks the automated accessibility gate (axe, browserless rules):`,
  ];
  for (const result of audit.violations) {
    lines.push(`  rule ${result.id} (${result.impact ?? "unknown"}): ${result.help}`);
    lines.push(`    ${result.helpUrl}`);
    for (const offence of offencesOf(result)) {
      lines.push(`    - ${offence.target}`);
      lines.push(`      ${offence.html}`);
      for (const line of offence.summary.split("\n").filter((entry) => entry.trim() !== "")) {
        lines.push(`      ${line}`);
      }
    }
  }
  return lines.join("\n");
}

/** The undecided rules, printed when the sweep's roster of them changes. */
export function describeIncomplete(audit: Audit, screen: string): string {
  if (audit.incomplete.length === 0) return "";
  const lines: string[] = [
    `${screen} left these browserless rules undecided (incomplete, not a pass):`,
  ];
  for (const result of audit.incomplete) {
    lines.push(`  rule ${result.id} (${result.impact ?? "unknown"}): ${result.help}`);
    for (const offence of offencesOf(result)) {
      lines.push(`    - ${offence.target}`);
      lines.push(`      ${offence.html}`);
    }
  }
  return lines.join("\n");
}
