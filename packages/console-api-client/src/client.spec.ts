// These tests are the seam between the contract and the console: they pin
// what the generated client actually does (request shape, URL composition,
// result semantics) and what the generated types actually are. If
// api/openapi/console.yaml drifts and the client is regenerated, the type
// assertions below are what turns the drift red before the console compiles
// against a shape the service no longer produces.
import { describe, expect, expectTypeOf, it, vi } from "vitest";

import { client, getHealth, getReadiness, type HealthStatus } from "./index";

// The generated client constructs a platform Request around the final URL,
// and Node's Request requires an absolute one — so the stubs run against a
// base URL and the composed URL is read back off the Request the client made.
// The base URL also lets these tests pin the join the console depends on:
// the contract declares the server as "/", and a trailing slash must not
// double up in "/healthz".
const baseUrl = "http://gateway.test";

function stubJsonFetch(body: unknown, status = 200) {
  const fetch = vi.fn(
    async () =>
      new Response(JSON.stringify(body), {
        status,
        headers: { "content-type": "application/json" },
      }),
  );
  client.setConfig({ baseUrl, fetch: fetch as unknown as typeof fetch });
  return fetch;
}

describe("generated client", () => {
  it("pins HealthStatus to the OpenAPI document", () => {
    expectTypeOf<HealthStatus>().toEqualTypeOf<{ status: string }>();
  });

  it("requests GET /healthz against the configured base URL", async () => {
    const fetch = stubJsonFetch({ status: "ok" });

    const { data, error, response } = await getHealth();

    expect(fetch).toHaveBeenCalledTimes(1);
    const [request] = fetch.mock.calls[0] as unknown as [Request];
    expect(request.method).toBe("GET");
    expect(request.url).toBe(`${baseUrl}/healthz`);
    expect(response?.status).toBe(200);
    expect(error).toBeUndefined();
    expect(data?.status).toBe("ok");
    expectTypeOf(data).toEqualTypeOf<HealthStatus | undefined>();
  });

  it("requests GET /readyz against the configured base URL", async () => {
    const fetch = stubJsonFetch({ status: "ok" });

    const { data, error, response } = await getReadiness();

    expect(fetch).toHaveBeenCalledTimes(1);
    const [request] = fetch.mock.calls[0] as unknown as [Request];
    expect(request.method).toBe("GET");
    expect(request.url).toBe(`${baseUrl}/readyz`);
    expect(response?.status).toBe(200);
    expect(error).toBeUndefined();
    expect(data?.status).toBe("ok");
    expectTypeOf(data).toEqualTypeOf<HealthStatus | undefined>();
  });

  // The body is the contract's ErrorEnvelope, which is the only failure shape
  // this document declares — the contract declares no 503 anywhere, and the
  // scaffold's /readyz is always ready, so a 503 here would document a response
  // the service cannot produce. What is being pinned is the client's handling of
  // a status outside 2xx, not a response the contract promises; the status is
  // chosen because it is what an orchestrator reads a failing probe as, and the
  // body because an error the gateway does declare is the honest stand-in for
  // one it does not.
  it("reports a failing probe as an error result rather than throwing", async () => {
    stubJsonFetch(
      { error: { code: "internal", message: "readiness probe failed" }, request_id: "test" },
      503,
    );

    const { data, error, response } = await getReadiness();

    expect(response?.status).toBe(503);
    expect(data).toBeUndefined();
    expect(error).toBeDefined();
  });
});
