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

import {
  createApiKey,
  createPaymentForOffer,
  fetchPaymentIntents,
  fetchTopUpOffers,
  signInWith,
  signOutOfSession,
} from "./api";

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
  /**
   * The serialised request body, when the call carried one.
   *
   * Added for the payment write, which is the console's one money-moving POST:
   * what it carries is as much of a claim as where it goes, and the assertion
   * that it names an OFFER and never an amount is one only a body can support.
   * The existing tests ignore it, which is why it is optional.
   */
  readonly body?: string | undefined;
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

/**
 * The serialised body `fetch` was handed, from either call shape.
 *
 * A `Request` is cloned before it is read: reading the original would consume
 * the body the code under test is still going to send, and a clone is the only
 * way to see what was written without changing it.
 */
async function sentBody(input: unknown, init?: RequestInit): Promise<string | undefined> {
  if (typeof Request !== "undefined" && input instanceof Request) {
    return await input.clone().text();
  }
  const body = init?.body;
  return typeof body === "string" ? body : undefined;
}

let seen: Seen[] = [];

function stubFetch(body: unknown, status = 200) {
  const fetchMock = vi.fn(async (input: unknown, init?: RequestInit) => {
    const { method, headers } = sentRequest(input, init);
    seen.push({
      method,
      url: typeof input === "string" ? input : (input as Request).url,
      headers,
      body: await sentBody(input, init),
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

/**
 * The payment write, driven through the real seam.
 *
 * Three claims live here and none of them is visible from a screen's spec,
 * because every screen's spec mocks this module — which is the right way to test
 * a screen and exactly the wrong way to find out what the console puts on the
 * wire.
 *
 * **1. The 409 has a code, and it is `conflict`.** The status → code table has
 * no row for every status the contract names, on purpose: a code invented there
 * is a code `CONSOLE_BEHAVIOUR` has no row for and a reader would be shown a
 * behaviour nobody wrote. `409` is the one row the payment surface added, and
 * without it a refusal whose body did not parse would render as `internal` — a
 * bug with a request id nobody can look up — while the matrix's real `conflict`
 * row sat unreachable.
 *
 * **2. A 403 is classified by what the envelope says, not by the status.** This
 * is the coordinator's point made permanent: `createPaymentIntent` is the ONE
 * operation on this surface that declares a 403 — it carries the cross-origin
 * and double-submit guard, which a GET does not — and the contract declares that
 * refusal's code as `invalid_request`. So there is no `case 403` in the table:
 * the parsed envelope already carries the code, and a second opinion derived
 * from the status number is exactly the drift the seam exists to prevent. A 403
 * with NO envelope at all falls to `internal`, which is the honest answer for a
 * status this console has not been given a code for.
 *
 * **3. The list calls carry no CSRF header.** Also the same point from the other
 * side. `listTopUpOffers` and `listPaymentIntents` declare no 403 — a GET has no
 * `Origin` and no unsafe method to guard — so the seam sends no token on them,
 * and a test that asserted one would be asserting a contract the document does
 * not contain.
 */
describe("the payment write", () => {
  beforeEach(() => {
    seen = [];
    setJar("");
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    delete (document as unknown as Record<string, unknown>).cookie;
    Object.defineProperty(Document.prototype, "cookie", realCookie!);
  });

  it("classifies a 409 whose body did not parse as `conflict`", async () => {
    // A body that is JSON and is not the contract's envelope. This is the case
    // the table exists for: the status is the only thing left to classify by,
    // and `internal` here would show a payment refusal as a console bug.
    stubFetch({ detail: "the account is suspended" }, 409);

    const result = await createPaymentForOffer({
      offer: "o0000000-0000-4000-8000-0000000000o1",
      idempotency_key: "key-1",
    });

    expect(result.ok).toBe(false);
    if (result.ok) throw new Error("a 409 must not be a success");
    expect(result.failure.kind).toBe("api");
    if (result.failure.kind !== "api") throw new Error("expected an api failure");
    expect(result.failure.envelope.error.code).toBe("conflict");
    // And the correlation is honest rather than invented: the response carried
    // no `X-Request-Id`, and a fabricated identifier is worse than a stated
    // absence because it looks like something an operator could look up.
    expect(result.failure.envelope.request_id).toBe("unreported");
    expect(result.failure.unauthenticated).toBe(false);
  });

  it("passes a contracted envelope through untouched, code included", async () => {
    stubFetch(
      {
        error: { code: "conflict", message: "the account is suspended" },
        request_id: "req-409-1",
      },
      409,
    );

    const result = await createPaymentForOffer({
      offer: "o0000000-0000-4000-8000-0000000000o1",
      idempotency_key: "key-1",
    });

    if (result.ok) throw new Error("a 409 must not be a success");
    if (result.failure.kind !== "api") throw new Error("expected an api failure");
    expect(result.failure.envelope.error.code).toBe("conflict");
    expect(result.failure.envelope.request_id).toBe("req-409-1");
  });

  it("reads a 403's code off the envelope rather than inventing one from the status", async () => {
    // The shape the contract declares for this operation's 403: "the status is
    // 403 with the `invalid_request` code — the request is well-formed HTTP and
    // is refused for being untrusted, not malformed".
    stubFetch(
      {
        error: { code: "invalid_request", message: "the request did not come from this origin" },
        request_id: "req-403-1",
      },
      403,
    );

    const result = await createPaymentForOffer({
      offer: "o0000000-0000-4000-8000-0000000000o1",
      idempotency_key: "key-1",
    });

    if (result.ok) throw new Error("a 403 must not be a success");
    if (result.failure.kind !== "api") throw new Error("expected an api failure");
    expect(result.failure.envelope.error.code).toBe("invalid_request");
  });

  it("names no code for a 403 that carried no envelope", async () => {
    // Deliberately NOT a second guess at the security refusal. `403` has no row
    // in the table because the one operation that declares it declares its code
    // in the envelope; a body this console cannot read is a failure it can only
    // call ours.
    stubFetch({}, 403);

    const result = await createPaymentForOffer({
      offer: "o0000000-0000-4000-8000-0000000000o1",
      idempotency_key: "key-1",
    });

    if (result.ok) throw new Error("a 403 must not be a success");
    if (result.failure.kind !== "api") throw new Error("expected an api failure");
    expect(result.failure.envelope.error.code).toBe("internal");
  });

  it("echoes the double-submit token, names the offer, and sends no amount", async () => {
    issueToken();
    stubFetch(
      {
        id: "y0000000-0000-4000-8000-0000000000y1",
        status: "created",
        amount_minor_units: 2_500,
        currency: "EUR",
        minor_unit_exponent: 2,
        transfer_instructions: null,
        created_at: "2026-09-20T09:00:00Z",
        expires_at: "2026-09-20T09:30:00Z",
      },
      201,
    );

    await createPaymentForOffer({
      offer: "o0000000-0000-4000-8000-0000000000o1",
      idempotency_key: "key-1",
    });

    expect(seen).toHaveLength(1);
    expect(seen[0]!.method).toBe("POST");
    expect(seen[0]!.url).toContain("/payment-intents");
    expect(seen[0]!.headers[CSRF_HEADER.toLowerCase()]).toBe(TOKEN);

    // What it carries, which is the request side of "a client may not choose a
    // price". The contract has no `amount_minor_units` and no `currency` on
    // this request, and a client that sent one would be inventing a field the
    // server refuses — so the assertion is over the WHOLE parsed body rather
    // than over the two keys this change expects to see.
    const body = JSON.parse(seen[0]!.body ?? "{}") as Record<string, unknown>;
    expect(Object.keys(body).sort()).toEqual(["idempotency_key", "offer"]);
    expect(body.offer).toBe("o0000000-0000-4000-8000-0000000000o1");
    expect(body.idempotency_key).toBe("key-1");
    expect(body).not.toHaveProperty("amount_minor_units");
    expect(body).not.toHaveProperty("currency");
  });

  it("sends the token on the write and on neither list read", async () => {
    // The asymmetry the contract states and the coordinator's note rests on.
    issueToken();
    stubFetch({ items: [] });

    await fetchTopUpOffers();
    await fetchPaymentIntents();

    expect(seen).toHaveLength(2);
    for (const request of seen) {
      expect(request.method).toBe("GET");
      expect(request.headers).not.toHaveProperty("x-console-csrf");
    }
  });
});
