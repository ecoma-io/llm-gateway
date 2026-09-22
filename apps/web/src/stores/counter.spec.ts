import { createPinia, setActivePinia } from "pinia";
import { beforeEach, describe, expect, it } from "vitest";

import { useCounterStore } from "./counter";

describe("counter store", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
  });

  it("increment moves the count", () => {
    const counter = useCounterStore();
    expect(counter.count).toBe(0);

    counter.increment();
    expect(counter.count).toBe(1);

    counter.increment();
    expect(counter.count).toBe(2);
  });
});
