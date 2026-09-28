// The money renderer's rules, asserted. The load-bearing test here is the
// NEGATIVE one the brief names: the module must be UNABLE to compute. A test
// that only checks "1,250" renders "1,250" passes just as happily when a
// `sum` or a `settled − held` is added alongside it, so this file spends most
// of its weight on the assertions that fail if any arithmetic appears.
import { mount } from "@vue/test-utils";
import { describe, expect, it } from "vitest";

import MoneyAmount from "./MoneyAmount.vue";
import {
  formatBalances,
  formatMinorUnits,
  formatMoney,
  formatSignedMinorUnits,
  UNRENDERABLE_AMOUNT,
} from "./money";
import type { Money } from "@ecoma-io/llm-gateway-console-api-client";

const money = (minor_units: number): Money => ({ minor_units });

describe("the money renderer", () => {
  describe("formats, and nothing else", () => {
    it("groups minor units and keeps the sign the server sent", () => {
      expect(formatMinorUnits(0)).toBe("0");
      expect(formatMinorUnits(7)).toBe("7");
      expect(formatMinorUnits(1234)).toBe("1,234");
      expect(formatMinorUnits(1234567)).toBe("1,234,567");
      expect(formatMinorUnits(1000)).toBe("1,000");
      // The sign is a presentation of what the server sent, not a
      // computation: a negative delta is rendered negative, nothing more.
      expect(formatMinorUnits(-1234)).toBe("-1,234");
      expect(formatMinorUnits(-999)).toBe("-999");
    });

    it("renders a Money wrapper and a delta with a leading plus on the positive case", () => {
      expect(formatMoney(money(2500))).toBe("2,500");
      expect(formatSignedMinorUnits(money(2500))).toBe("+2,500");
      expect(formatSignedMinorUnits(money(-2500))).toBe("-2,500");
      // Zero has no direction, so it takes no plus — a "+0" would assert a
      // movement that did not happen.
      expect(formatSignedMinorUnits(money(0))).toBe("0");
    });

    it("names no currency, because the schema has no currency field", () => {
      // A symbol or a code here would be the defect the contract's own
      // `Money` description warns a client into: a number rendered without a
      // unit that guesses the unit. So the rendered text is bare digits and
      // bare separators — nothing that names a currency.
      for (const amount of [0, 1, 1234, 1000000, -2500]) {
        const rendered = formatMoney(money(amount));
        expect(rendered).not.toMatch(/[A-Za-z]{3}/); // no USD/EUR/...
        expect(rendered).not.toMatch(/[$€£¥₹]/); // no symbol
        expect(rendered).toMatch(/^-?[\d,]+$/); // digits and separators only
      }
    });
  });

  describe("cannot compute — the negative that matters", () => {
    it("has no exported arithmetic: nothing in the module sums or subtracts", async () => {
      // Read the module's own source and assert the absence of every
      // arithmetic operator on an amount. A renderer that grew a `sum` or a
      // `subtract` would introduce one of these the moment a screen asked it
      // for a total, and the only way to catch that before review is to
      // forbid the operators outright in the one file that formats money.
      const source = await import("./money?raw").then((module) => module.default as string);
      const withoutComments = source
        .replace(/\/\*[\s\S]*?\*\//g, "")
        .replace(/\/\/.*$/gm, "")
        // Drop import statements: a package specifier such as
        // `@ecoma-io/llm-gateway-…` is full of hyphens and is not arithmetic
        // on an amount. The scan is over the module's own logic only.
        .replace(/^import[\s\S]*?from\s+["'][^"']+["'];?$/gm, "");
      // No `+`/`-` applied between two values — a summing or subtracting
      // renderer would introduce one the moment a screen asked for a total —
      // and no reduction over a collection. `Math.abs` is formatting (it
      // strips a sign so grouping sees digits), not a balance, so it is
      // allowed and asserted present: the rule reads as "no ledger
      // arithmetic" rather than "no Math at all".
      expect(withoutComments).not.toMatch(/\w\s*[+]\s*\w/);
      expect(withoutComments).not.toMatch(/\w\s*-\s*\w(?!\s*[<>=])/);
      expect(withoutComments).not.toMatch(/\.reduce\(/);
      expect(withoutComments).not.toMatch(/\.sum\(/);
      expect(withoutComments).toMatch(/Math\.abs\(/);
    });

    it("does not derive available from settled and held, even when they disagree", () => {
      // The identity the database checks is `available = settled − held`. A
      // renderer that computed it would agree when the data is healthy and
      // silently DISAGREE when the cache and the legs are out of step — the
      // one moment a computed figure is least wanted. So: feed a `Balances`
      // whose three figures do not add up, and assert the renderer shows the
      // server's `available` verbatim, not the subtraction.
      const balances = {
        settled: money(1000),
        held: money(300),
        available: money(999), // deliberately NOT 700
      };
      const rendered = formatBalances(balances);
      expect(rendered.settled).toBe("1,000");
      expect(rendered.held).toBe("300");
      // The point of the test: the renderer printed 999, the server's value.
      expect(rendered.available).toBe("999");
      // And specifically, the value a subtracting renderer would have
      // produced is nowhere in the output.
      expect(Object.values(rendered)).not.toContain("700");
    });

    it("renders each of the three balances from its own server value", () => {
      // The three are independent facts. This asserts the renderer reads each
      // one and does not, for example, render `held` twice or leave
      // `available` equal to `settled`.
      const rendered = formatBalances({
        settled: money(5),
        held: money(6),
        available: money(7),
      });
      expect(rendered).toEqual({ settled: "5", held: "6", available: "7" });
    });
  });

  describe("refuses to render a number it cannot show exactly", () => {
    it("sentinels an amount outside the exact-integer range instead of guessing", () => {
      // A float is one the transport could not have carried as an int64
      // exactly. Rendering its digits as money would be a wrong number
      // wearing a money shape; the renderer says so instead.
      expect(formatMinorUnits(Number.MAX_SAFE_INTEGER + 2)).toBe(UNRENDERABLE_AMOUNT);
      expect(formatMinorUnits(1.5)).toBe(UNRENDERABLE_AMOUNT);
      expect(formatMoney(undefined)).toBe(UNRENDERABLE_AMOUNT);
      expect(formatMoney(null)).toBe(UNRENDERABLE_AMOUNT);
      expect(formatBalances(undefined).available).toBe(UNRENDERABLE_AMOUNT);
    });
  });

  describe("as a component", () => {
    it("renders grouped digits with no currency glyph in the DOM", () => {
      const wrapper = mount(MoneyAmount, { props: { amount: money(1234) } });
      expect(wrapper.text()).toBe("1,234");
      expect(wrapper.text()).not.toMatch(/[A-Za-z$€£¥₹]/);
      wrapper.unmount();
    });

    it("renders a delta with a sign and a balance without one", () => {
      const balance = mount(MoneyAmount, { props: { amount: money(500) } });
      expect(balance.text()).toBe("500");
      balance.unmount();

      const delta = mount(MoneyAmount, { props: { amount: money(500), signed: true } });
      expect(delta.text()).toBe("+500");
      delta.unmount();
    });

    it("announces an unrenderable amount as a data problem, not as a number", () => {
      const wrapper = mount(MoneyAmount, {
        props: { amount: money(Number.MAX_SAFE_INTEGER + 10) },
      });
      expect(wrapper.text()).toBe(UNRENDERABLE_AMOUNT);
      // The label overrides the digits so a screen reader does not read an em
      // dash as "minus".
      expect(wrapper.attributes("aria-label")).toBe("Amount too large to display exactly");
      wrapper.unmount();
    });

    it("does not colour a money figure, because a balance has no health", () => {
      const wrapper = mount(MoneyAmount, { props: { amount: money(1) } });
      // No status tone: a money figure is not a status, and a green balance
      // would claim a health the contract never asserts.
      const classAttr = wrapper.attributes("class") ?? "";
      expect(classAttr).not.toMatch(/text-(success|destructive|warning)/);
      wrapper.unmount();
    });
  });
});
