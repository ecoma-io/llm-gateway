// The double-submit token, tested at the seam that has to produce it.
//
// This file exists because every other spec in the console MOCKS `@/lib/api` —
// which is the right way to test a screen and exactly the wrong way to find out
// what the console puts on the wire. A screen's spec cannot see a missing
// header, and the whole product was unsignable for a while because of one: the
// server refuses every unsafe request that does not echo the token, the console
// read no cookie, and sign-in, sign-out and minting an API key were all
// impossible against a real backend. Every one of those specs passed.
//
// So this spec drives the REAL `lib/api.ts` against a stubbed `fetch` and asks
// the one question a screen's spec cannot: what did this request carry?
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { createApiKey, signInWith, signOutOfSession } from "./api";

/** The names the server mints the token under; the same ones the code reads. */
const CSRF_COOKIE = "__Host-console_csrf";
const CSRF_HEADER = "X-Console-Csrf";

/**
 * The token is a fixture VALUE, not a credential.
 *
 * It is 43 characters of base64url because the server mints 256 bits that way,
 * and a test that used a readable string would not exercise the parsing at all.
 * It grants nothing: it is meaningless without the session cookie that the page
 * cannot read either.
 */
const TOKEN = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA";

interface Seen {
  readonly method: string;
  readonly url: string;
  readonly headers: Record<string, string>;
}

/**
 * The method and headers `fetch` was handed.
 *
 * The generated client calls `fetch(request)` with a `Request` object rather
 * than `(url, init)`, so both shapes have to be read — a harness that assumed
 * the second one saw `GET` and no headers for every call, which is a green
 * test that has measured nothing.
 */
function sentRequest(
  input: unknown,
  init?: RequestInit,
): { method: string; headers: Record<string, string> } {
  const headers: Record<string, string> = {};
  const isRequest = typeof Request !== "undefined" && input instanceof Request;
  const method = isRequest ? (input as Request).method : String(init?.method ?? "GET");
  const source = isRequest ? (input as Request).headers : (init?.headers ?? {});
  const entries =
    source instanceof Headers
      ? [...source.entries()]
      : Object.entries(source as Record<string, string>);
  for (const [name, value] of entries) headers[name.toLowerCase()] = String(value);
  return { method, headers };
}

let seen: Seen[] = [];

function stubFetch(body: unknown, status = 200) {
  const fetchMock = vi.fn(async (input: unknown, init?: RequestInit) => {
    const { method, headers } = sentRequest(input, init);
    seen.push({
      method,
      url: typeof input === "string" ? input : (input as Request).url,
      headers,
    });
    return new Response(body === undefined ? null : JSON.stringify(body), {
      status,
      headers: { "content-type": "application/json" },
    });
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

/**
 * The jar this spec's tests read, and a way to put it back.
 *
 * `document.cookie = "name=value"` makes jsdom enforce the `__Host-` prefix
 * rule — no `Domain`, `Path=/`, `Secure` — and this test runs on `http://` with
 * no way to set those attributes through the property, so the write is REJECTED
 * and silently dropped. Measured rather than assumed: an assignment to
 * `__Host-console_csrf` leaves `document.cookie` holding only the other name.
 *
 * That is jsdom being correctly strict about a real rule, and the seam under
 * test is the READ. Describing the jar directly reaches the state a browser is
 * in after the server's `Set-Cookie` has been processed, which is the state
 * this function actually meets.
 *
 * The property is REPLACED rather than assigned, and restored in `afterEach`:
 * an accessor with no setter is what the real one looks like, and a spec that
 * left it replaced would hand every later test in this file a jar of its own
 * making — which is a test that stops testing the thing and starts testing
 * its own fixture.
 */
const realCookie = Object.getOwnPropertyDescriptor(Document.prototype, "cookie");
let jar = "";

function setJar(contents: string): void {
  jar = contents;
  Object.defineProperty(document, "cookie", {
    configurable: true,
    get: () => jar,
    set: () => undefined,
  });
}

function issueToken(value = TOKEN): void {
  setJar(jar === "" ? `${CSRF_COOKIE}=${value}` : `${jar}; ${CSRF_COOKIE}=${value}`);
}

describe("the double-submit token", () => {
  beforeEach(() => {
    seen = [];
    // Each test starts from a jar it describes itself, so a token left behind
    // by one test is never a token the next one believes the server issued.
    setJar("");
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    delete (document as unknown as Record<string, unknown>).cookie;
    Object.defineProperty(Document.prototype, "cookie", realCookie!);
  });

  it("echoes the token in the header when signing in", async () => {
    issueToken();
    stubFetch({ principal: { class: "user", account_id: "a1", user_id: "u1" } });

    await signInWith({ account_id: "a1", email: "ops@example.test", password: "pw" });

    expect(seen).toHaveLength(1);
    expect(seen[0]!.method).toBe("POST");
    expect(seen[0]!.headers[CSRF_HEADER.toLowerCase()]).toBe(TOKEN);
  });

  it("echoes the token when signing out", async () => {
    issueToken();
    stubFetch(undefined, 204);

    await signOutOfSession();

    expect(seen).toHaveLength(1);
    expect(seen[0]!.method).toBe("DELETE");
    expect(seen[0]!.headers[CSRF_HEADER.toLowerCase()]).toBe(TOKEN);
  });

  it("echoes the token when minting an API key", async () => {
    issueToken();
    stubFetch({ api_key: { id: "k1", display_name: "ci" } });

    await createApiKey("ci");

    expect(seen).toHaveLength(1);
    expect(seen[0]!.method).toBe("POST");
    expect(seen[0]!.headers[CSRF_HEADER.toLowerCase()]).toBe(TOKEN);
  });

  it("sends NO header at all when the page was issued no token", async () => {
    // The server refuses a request with a header and no cookie exactly as
    // firmly as one with a cookie and no header: a header alone is a value a
    // cross-origin caller chose for itself. An EMPTY header would be an attempt
    // rather than an absence, and would still be refused — so the honest
    // absence is no key in the headers object at all.
    stubFetch({ principal: { class: "user", account_id: "a1", user_id: "u1" } });

    await signInWith({ account_id: "a1", email: "ops@example.test", password: "pw" });

    expect(seen[0]!.headers).not.toHaveProperty("x-console-csrf");
    expect(Object.keys(seen[0]!.headers)).not.toContain("x-console-csrf");
  });

  it("reads the token out of a cookie jar that holds more than one cookie", async () => {
    // The server issues both a session cookie and this one, and a browser
    // sends both. A parser that took the first pair in `document.cookie` would
    // echo the SESSION TOKEN into the CSRF header — sending the credential the
    // page is not allowed to see, into a header the server logs.
    setJar("some_other=value");
    issueToken();
    stubFetch({ principal: { class: "user", account_id: "a1", user_id: "u1" } });

    await signInWith({ account_id: "a1", email: "ops@example.test", password: "pw" });

    expect(seen[0]!.headers[CSRF_HEADER.toLowerCase()]).toBe(TOKEN);
  });

  it("reads the LAST cookie of a repeated name, as a browser would", async () => {
    // `document.cookie` shows duplicates, most specific path first, and
    // `path=/` is the least specific — so the one this origin reads back is
    // the last. Taking the first is how a stale token at `/` gets echoed over
    // a fresh one and every write is refused for a reason nothing logs.
    setJar(`${CSRF_COOKIE}=stale; `);
    issueToken();
    stubFetch({ principal: { class: "user", account_id: "a1", user_id: "u1" } });

    await signInWith({ account_id: "a1", email: "ops@example.test", password: "pw" });

    expect(seen[0]!.headers[CSRF_HEADER.toLowerCase()]).toBe(TOKEN);
  });

  it("never writes the token to storage", async () => {
    issueToken();
    const setItem = vi.spyOn(Storage.prototype, "setItem");
    stubFetch({ principal: { class: "user", account_id: "a1", user_id: "u1" } });

    await signInWith({ account_id: "a1", email: "ops@example.test", password: "pw" });

    // The browser already keeps this value. A copy in localStorage would be a
    // copy that outlives the tab and outlives the session, which is strictly
    // worse than the thing the server already set.
    expect(setItem).not.toHaveBeenCalled();
    setItem.mockRestore();
  });
});
