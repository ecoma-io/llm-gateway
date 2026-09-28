// The route guard and the 401 announcement.
//
// `router/index.ts` is where the console decides who may see a screen, and the
// two decisions there — the gate and the announcement — are the whole of it. The
// tests below mock the SEAM, not the session store, because the store's answer
// comes from `getSession` and a guard test that stubbed the store would prove
// only that the guard reads a stub. The seam is the boundary the whole design
// rests on (ADR 0012 §7), so the tests are written against it.
//
// Every test builds its OWN router over the same route table, with a memory
// history. A shared singleton would carry its current route and its guard's
// memory of who is signed in from one test into the next, and a file where the
// fourth assertion is really about the third test's session is worse than no
// test at all.
import { flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { createMemoryHistory, type Router } from "vue-router";

const seam = vi.hoisted(() => ({
  listeners: new Set<(failure: unknown) => void>(),
  getSessionResult: vi.fn(),
  signOutOfSession: vi.fn(),
}));

vi.mock("@/lib/api", async () => {
  const actual = await vi.importActual<typeof import("@/lib/api")>("@/lib/api");
  return {
    ...actual,
    getSessionResult: seam.getSessionResult,
    signOutOfSession: seam.signOutOfSession,
    onSessionEnded: (listener: (failure: unknown) => void) => {
      seam.listeners.add(listener);
      return () => seam.listeners.delete(listener);
    },
  };
});

import {
  createConsoleRouter,
  LANDING_PATH,
  ROUTES,
  SIGN_IN_PATH,
  watchForSessionEnd,
} from "./index";
import type { Principal } from "@ecoma-io/llm-gateway-console-api-client";

const PRINCIPAL: Principal = {
  class: "user",
  account_id: "a0000000-0000-4000-8000-0000000000a1",
  user_id: "u0000000-0000-4000-8000-0000000000u1",
};

/** The contract's one refusal for a session that is not there. */
const REFUSAL = {
  ok: false as const,
  failure: {
    kind: "api" as const,
    unauthenticated: true,
    envelope: {
      error: { code: "unauthenticated" as const, message: "no session" },
      request_id: "r",
    },
  },
};

const established = { ok: true as const, data: PRINCIPAL };

function freshRouter(): Router {
  const router = createConsoleRouter(createMemoryHistory());
  return router;
}

beforeEach(() => {
  seam.listeners.clear();
  seam.getSessionResult.mockReset();
  seam.signOutOfSession.mockReset();
  setActivePinia(createPinia());
});

describe("the route table", () => {
  it("has one screen per product domain, plus sign-in", () => {
    // Every nav entry in `AppLayout` resolves to one of these, and every one of
    // these is reachable. A nav link to a route that is not here would be a 404
    // the console built itself.
    const paths = ROUTES.map((route) => route.path);
    for (const path of [
      "/",
      "/identity",
      "/catalog",
      "/commerce",
      "/accounting",
      "/reconciliation",
    ]) {
      expect(paths).toContain(path);
    }
  });

  it("marks exactly one route reachable without a session", () => {
    const publicRoutes = ROUTES.filter((route) => route.meta?.public === true);
    expect(publicRoutes.map((route) => route.name)).toEqual(["sign-in"]);
  });

  it("names a landing path that is also a real route", () => {
    // The guard returns `LANDING_PATH` as a redirect target. A redirect to a
    // path no route declares is a navigation failure with no screen behind it.
    expect(ROUTES.map((route) => route.path)).toContain(LANDING_PATH);
  });
});

describe("the session gate", () => {
  it("sends a visitor without a session to sign-in, remembering where they were going", async () => {
    seam.getSessionResult.mockResolvedValue(REFUSAL);
    const router = freshRouter();

    await router.push("/identity");

    expect(router.currentRoute.value.path).toBe(SIGN_IN_PATH);
    expect(router.currentRoute.value.query.redirect).toBe("/identity");
  });

  it("lets the sign-in screen through with no session at all", async () => {
    seam.getSessionResult.mockResolvedValue(REFUSAL);
    const router = freshRouter();

    await router.push(SIGN_IN_PATH);

    expect(router.currentRoute.value.path).toBe(SIGN_IN_PATH);
  });

  it("asks the session once and reuses that answer for later navigations", async () => {
    seam.getSessionResult.mockResolvedValue(established);
    const router = freshRouter();

    await router.push("/identity");
    await router.push("/accounting");

    expect(router.currentRoute.value.path).toBe("/accounting");
    // A navigation within a live session must not cost a round trip to prove
    // what the store already knows.
    expect(seam.getSessionResult).toHaveBeenCalledTimes(1);
  });

  it("does not send an already-signed-in visitor away from the sign-in form", async () => {
    seam.getSessionResult.mockResolvedValue(established);
    const router = freshRouter();

    await router.push(SIGN_IN_PATH);

    // Authorisation is the guard's job and presentation is the screen's. The
    // sign-in screen shows "you are already signed in" and offers the dashboard,
    // which is a usable answer; a guard that redirected would race the
    // announcement's own navigation to this path and could loop.
    expect(router.currentRoute.value.path).toBe(SIGN_IN_PATH);
    // And the screen already knows the session exists without a round trip.
    expect(seam.getSessionResult).not.toHaveBeenCalled();
  });

  it("asks again after the session ended, rather than trusting a stale answer", async () => {
    seam.getSessionResult.mockResolvedValue(established);
    const router = freshRouter();
    const stop = watchForSessionEnd(router);
    try {
      await router.push("/commerce");
      expect(router.currentRoute.value.path).toBe("/commerce");

      for (const listener of seam.listeners) listener(REFUSAL.failure);
      await flushPromises();
      expect(router.currentRoute.value.path).toBe(SIGN_IN_PATH);

      // The announcement cleared the store, so the guard re-asks rather than
      // letting a stale `true` bounce the visitor forward.
      seam.getSessionResult.mockResolvedValue(REFUSAL);
      await router.push("/commerce");
      expect(router.currentRoute.value.path).toBe(SIGN_IN_PATH);
    } finally {
      stop();
    }
  });
});

describe("the 401 announcement", () => {
  it("clears the session and replaces to sign-in with where the reader was", async () => {
    seam.getSessionResult.mockResolvedValue(established);
    const router = freshRouter();
    await router.push("/commerce");

    const stop = watchForSessionEnd(router);
    try {
      // The seam emits on the FIRST 401 of any call, from whichever screen
      // made it. The shell is the only subscriber, and it is subscribed for
      // exactly this reason.
      for (const listener of seam.listeners) listener(REFUSAL.failure);
      await flushPromises();

      expect(router.currentRoute.value.path).toBe(SIGN_IN_PATH);
      expect(router.currentRoute.value.query.redirect).toBe("/commerce");
    } finally {
      stop();
    }
  });

  it("does not stack a second sign-in when one is already showing", async () => {
    seam.getSessionResult.mockResolvedValue(established);
    const router = freshRouter();
    await router.push("/catalog");

    const stop = watchForSessionEnd(router);
    try {
      for (const listener of seam.listeners) listener(REFUSAL.failure);
      await flushPromises();
      const first = router.currentRoute.value.fullPath;

      // A second 401 from a screen that was still in flight must not rewrite
      // the sign-in screen's own URL.
      for (const listener of seam.listeners) listener(REFUSAL.failure);
      await flushPromises();

      expect(router.currentRoute.value.fullPath).toBe(first);
    } finally {
      stop();
    }
  });

  it("carries no credential into the sign-in URL", async () => {
    seam.getSessionResult.mockResolvedValue(established);
    const router = freshRouter();
    await router.push("/accounting?after=c-2&kind=consume");

    const stop = watchForSessionEnd(router);
    try {
      for (const listener of seam.listeners) listener(REFUSAL.failure);
      await flushPromises();

      // The redirect carries a PATH and the reader's own filters — nothing that
      // is a credential, because no product route has one to carry.
      const query = router.currentRoute.value.query;
      expect(String(query.redirect)).toBe("/accounting?after=c-2&kind=consume");
      expect(JSON.stringify(query)).not.toMatch(/token|secret|password/i);
    } finally {
      stop();
    }
  });
});
