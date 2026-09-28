// The page reaches the API only through lib/api, which re-exports the
// generated contract package. Mocking that boundary lets this test pin UI
// behavior without mirroring any OpenAPI shape in a Vue fixture.
//
// The badge assertions below are about the THREE CHANNELS, and the count is the
// claim (ADR 0012 §7): colour, an `aria-hidden` icon, and a human label. Two of
// the three survive greyscale and two survive a screen reader that ignores
// colour, so any one of them missing is a status some operator cannot read.
//
// This file's previous version asserted `text().match(/ok/g)` had length 2. That
// regex is unanchored, so it matches `ok` inside "book" and inside a URL; it
// counts matches rather than badges, so it cannot say whose badge it counted or
// distinguish liveness from readiness; and it pins the raw contract token the
// ADR says must stop rendering, so the test blocked the change its own ADR
// mandates. What replaces it reads each badge the way a reader does - off the
// card it belongs to - and asserts the channels rather than the word.
import { flushPromises, mount, type VueWrapper } from "@vue/test-utils";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  getHealth: vi.fn(),
  getReadiness: vi.fn(),
}));

vi.mock("@/lib/api", () => api);

import GatewayStatusPage from "./GatewayStatusPage.vue";
import { probeStatusPresentation } from "@/modules/status/presentation";

/** One rendered probe badge, read off the three channels. */
interface Badge {
  /** The sentence a reader is told. Never the contract token. */
  readonly label: string;
  /** The icon's Lucide name, read off the DOM and not off the page's import. */
  readonly icon: string | undefined;
  /** How many elements in the badge are hidden from the accessibility tree. */
  readonly hidden: number;
  /** The badge's own tag, so a shape claim cannot be met by the card's title. */
  readonly tag: string;
}

/**
 * The badge on the card titled `title`, located by that title and by nothing
 * else.
 *
 * An index into the page would let this pass against the wrong probe the moment
 * the grid above it gained an entry, which is the failure the old count-of-
 * matches assertion could not see at all. The card is found by its own title
 * text; the badge is found by SHAPE inside it, a `<span>` carrying an
 * `aria-hidden` icon beside a label, so a card whose badge went away reports a
 * missing badge rather than a card title found by accident.
 */
function badgeIn(title: string): Badge {
  const card = [...document.querySelectorAll("div")].findLast(
    (element) => element.querySelector(":scope > div > p")?.textContent === title,
  );
  expect(card, `no card titled ${title}`).toBeDefined();
  const badge = [...card!.querySelectorAll("span")].findLast(
    (element) => element.querySelector("[aria-hidden]") !== null,
  );
  expect(badge, `no badge on the card titled ${title}`).toBeDefined();
  const icon = badge!.querySelector("[aria-hidden]");
  return {
    label: badge!.textContent?.trim() ?? "",
    icon: lucideNames(icon).join(" "),
    hidden: badge!.querySelectorAll("[aria-hidden]").length,
    tag: badge!.tagName.toLowerCase(),
  };
}

/**
 * Lucide's own icon classes, `lucide lucide-<name>`, read off the element.
 *
 * Asserting an icon by IMPORTING the same icon the page imports would prove
 * only that two references to one module are equal: swap the map's icon for any
 * other icon and the assertion follows it straight through. Reading the names
 * the DOM carries puts literals in the test, so a change to the presentation
 * map is a change the test has to be told about.
 */
function lucideNames(element: Element | null): readonly string[] {
  return (element?.getAttribute("class") ?? "")
    .split(/\s+/)
    .filter((name) => name.startsWith("lucide-"));
}

/** Mounted with the two probes at the statuses given, after the page settles. */
async function mountProbes(health: unknown, readiness: unknown): Promise<VueWrapper> {
  api.getHealth.mockResolvedValue(health);
  api.getReadiness.mockResolvedValue(readiness);
  const wrapper = mount(GatewayStatusPage, { attachTo: document.body });
  await flushPromises();
  return wrapper;
}

/** Every mounted wrapper, torn down in one place; see the note on `afterEach`. */
const mounted: VueWrapper[] = [];

afterEach(() => {
  // Unconditional, not at the end of each test: a test that failed half way
  // through is exactly the one that would have left its wrapper behind, still
  // subscribed, for the next test's `beforeEach` to re-arm.
  while (mounted.length > 0) mounted.pop()!.unmount();
  document.body.innerHTML = "";
});

describe("GatewayStatusPage", () => {
  beforeEach(() => {
    api.getHealth.mockReset();
    api.getReadiness.mockReset();
    document.body.innerHTML = "";
  });

  it("renders successful generated-contract probe responses", async () => {
    const wrapper = await mountProbes({ data: { status: "ok" } }, { data: { status: "ok" } });
    mounted.push(wrapper);

    expect(api.getHealth).toHaveBeenCalledOnce();
    expect(api.getReadiness).toHaveBeenCalledOnce();
    expect(wrapper.text()).toContain("Liveness");
    expect(wrapper.text()).toContain("Readiness");
  });

  it("says a healthy probe is healthy, and never says the contract token", async () => {
    // `ok` is the SERVER's word for a healthy probe. It is a word from
    // `console.yaml`, and an operator reading a status page has none of the
    // context that would make it meaningful: a badge reading it renders the
    // question back to the reader. ADR 0012 §7 names this exact rendering as
    // the thing that stops.
    const wrapper = await mountProbes({ data: { status: "ok" } }, { data: { status: "ok" } });
    mounted.push(wrapper);

    for (const title of ["Liveness", "Readiness"]) {
      expect(badgeIn(title).label, title).toBe("Healthy");
    }
    // Anchored, unlike the regex this replaces: a word boundary on both sides,
    // over the whole document. "book" and "/sign-in" would have satisfied the
    // old assertion and are here to be caught rather than counted.
    expect(document.body.textContent).not.toMatch(/(?:^|[^A-Za-z0-9_])ok(?:[^A-Za-z0-9_]|$)/i);
  });

  it("carries one aria-hidden icon beside the label and no second name", async () => {
    // The shape is for a sighted reader in greyscale; the word is for everyone
    // else. The icon is hidden from the accessibility tree so it does not add a
    // SECOND spoken name for the same fact, and "exactly one" is the assertion
    // that holds the line: a second decorative glyph inside the badge is a
    // second thing the shape channel is saying, and nothing is left to say it.
    //
    // The icon's OWN `aria-hidden` is worth a word, because it is a fact about
    // Lucide rather than about this file. `buildLucideIconNode` adds
    // `aria-hidden="true"` to every icon it renders and `hasA11yProp` then
    // SUPPRESSES it for an icon handed an `aria-*` prop, so
    // `StatusBadge`'s explicit one is load-bearing against the opposite
    // failure - an `aria-hidden="false"` reaching the svg, which would put a
    // decorative glyph into the tree as a name of its own. Removing that
    // attribute is therefore a mutation this suite cannot catch, and the
    // mutation check in the pull request says so rather than pretending
    // otherwise.
    const wrapper = await mountProbes({ data: { status: "ok" } }, { data: { status: "ok" } });
    mounted.push(wrapper);

    for (const title of ["Liveness", "Readiness"]) {
      const badge = badgeIn(title);
      // And the hidden thing is a Lucide SVG rather than a text character: a
      // glyph rendered as a character is read aloud whatever its parent's
      // `aria-hidden` says, because it is text.
      expect(badge.hidden, title).toBe(1);
      expect(badge.icon, title).toContain("lucide-circle-check");
      expect(badge.tag, title).toBe("span");
    }
  });

  it("distinguishes the two probes in greyscale, not only in colour", async () => {
    // One healthy and one not, which is the whole reason the page exists. A
    // colour-only badge passes a glance at a monitor and fails in print, in
    // greyscale, and for a dichromatic reader. The two channels that survive
    // that are the LABEL and the ICON, and both are asserted to differ, so a
    // page that repainted the two states and labelled them alike still goes
    // red here.
    const wrapper = await mountProbes({ data: { status: "ok" } }, { data: { status: "degraded" } });
    mounted.push(wrapper);

    const healthy = badgeIn("Liveness");
    const degraded = badgeIn("Readiness");

    expect(healthy.label).toBe("Healthy");
    expect(degraded.label).toBe("Not healthy");
    expect(healthy.icon).toContain("lucide-circle-check");
    expect(degraded.icon).toContain("lucide-triangle-alert");
    expect(degraded.icon).not.toBe(healthy.icon);
  });

  it("renders an error state when the readiness probe fails", async () => {
    const wrapper = await mountProbes({ data: { status: "ok" } }, { error: new Error("no") });
    mounted.push(wrapper);

    expect(wrapper.text()).toContain("Probe unavailable");
  });

  it("will not call a probe that never reported healthy", () => {
    // The contract types a probe's `status` as a bare `string`, so there is no
    // union to key exhaustively: this map is a function with a fallback, and
    // the fallback has to be the honest one. A token this console has never
    // been told about is a probe in a state nobody can describe, and a badge
    // that guesses "fine" for it is the console asserting a fact the contract
    // never made.
    expect(probeStatusPresentation("ok")).toEqual(
      expect.objectContaining({ label: "Healthy", tone: "success" }),
    );
    expect(probeStatusPresentation("degraded")).toEqual(
      expect.objectContaining({ label: "Not healthy", tone: "destructive" }),
    );
    expect(probeStatusPresentation("a-status-nobody-has-contracted")).toEqual(
      expect.objectContaining({ label: "Not healthy", tone: "destructive" }),
    );
    // And no report is not a report of illness. "Not reported" is a different
    // sentence and a different shape, because the operator's next move differs:
    // one is "look at the gateway", the other is "look at the link".
    expect(probeStatusPresentation(undefined)).toEqual(
      expect.objectContaining({ label: "Not reported", tone: "destructive" }),
    );
  });
});
