# ADR 0002: Routing model — who owns fallback, translation, and egress

- Status: Accepted
- Date: 2026-09-23
- Issue: [#5](https://github.com/ecoma-io/llm-gateway/issues/5)

## Context

A client asks for a logical model name (`model: "fast-cheap"` or a passthrough
name like `claude-sonnet-5`); the gateway must decide which upstream provider
actually serves it, translate the request to that provider's protocol, react
to that provider failing, and present a uniform response — without billing the
client twice when a primary provider fails and a fallback serves the request.

Several concerns pile up here: alias resolution, candidate ordering,
provider-protocol translation, provider-level retries, cross-provider
fallback, streaming (SSE) semantics, and network egress (hiding the gateway's
egress IP behind proxies). Unless ownership is written down, each of these
leaks into the others — the historical failure mode is a "fallback" that is
really a retry, an adapter that knows about the billing consequences of its
own errors, or egress config buried inside provider credentials.

## Decision

### The routing pipeline

```text
logical model alias
        ↓
ordered candidates          (ModelAlias's candidate list, one consistent snapshot)
        ↓
candidate execution         (adapter translates; bounded provider retries)
        ↓
normalized result           (uniform success stream or typed failure)
        ↓
logical fallback            (router advances to the next candidate)
```

Ownership is split across three layers with hard boundaries:

| Concern                                  | Owner      | Rule                                                                                                                               |
| ---------------------------------------- | ---------- | ---------------------------------------------------------------------------------------------------------------------------------- |
| Resolving alias → ordered candidates     | Router     | Pure lookup against the Catalog; no provider knowledge.                                                                            |
| Cross-provider logical fallback          | Router     | **Only the router advances to the next candidate.** Adapters never fall back.                                                      |
| Request/response/stream translation      | Adapter    | One adapter per provider protocol family; produces the normalized result contract below. No routing, billing, or alias knowledge.  |
| Provider-specific retry                  | Adapter    | Bounded, same-candidate retry on retryable provider errors (e.g. 429/5xx/network). **Retry is not fallback** — see below.          |
| Model-name and parameter mapping         | Adapter    | Candidate carries the provider-side model ID and per-candidate parameter overrides.                                                |
| Outbound connection: proxy/egress choice | Egress     | Below adapters: adapters dial through an egress interface; egress config is referenced from the Backend, never from routing logic. |
| Billing/quota consequences of attempts   | Accounting | Via the Execution record only — see ADR 0004; no layer below the router knows what an attempt costs.                               |

### Provider retry is distinct from cross-provider fallback

These are different mechanisms with different budgets, and conflating them is
how double-billing and latency blowups happen:

- **Retry (adapter-owned):** re-issuing the same request to the **same
  candidate** after a retryable failure (429, 5xx, connect timeout), with
  backoff, bounded per candidate. Every upstream HTTP call — original or
  retry — is recorded as its own `RequestAttempt` row, so provider-side cost
  of retries is visible without touching client billing.
- **Fallback (router-owned):** abandoning the current **candidate** and moving
  to the next candidate in the alias's ordered list. Triggered when the
  adapter reports a failure that is not recoverable by retry (or the retry
  budget is exhausted), **and the response is not committed** (below).

### Commitment: the fallback gate

A response becomes **committed** when the gateway forwards the first
**content-bearing** byte to the client — for streaming, the first content
delta (text, or tool-call argument fragments, which are content); for a
non-streaming response, the first byte of the body. Protocol lifecycle
frames that carry no user-visible content (OpenAI-style role preambles,
Anthropic-style `message_start`/`content_block_start`) do not commit, **and
the gateway buffers them (bounded) instead of forwarding eagerly**, so a
provider that accepts a request and then fails before producing content is
still fallback-eligible. From the moment real content flows:

- **Cross-provider fallback is forbidden.** The client already holds partial
  content from this provider; switching providers mid-stream would corrupt
  the response (duplicate preamble, mismatched tool-call IDs, inconsistent
  model semantics).
- An upstream failure after commitment is reported to the client as a
  terminal error event in the stream (SSE mid-stream failures surface as an
  error payload inside an already-200 response — never a second HTTP
  status), the attempt is recorded as `failed_after_commitment`, and usage
  capture proceeds on best effort (invariant 8, ADR 0004).

Before commitment, a failed candidate costs the client nothing (invariant 6);
the router falls back to the next candidate, and the request is served — and
settled — from the attempt that actually delivered.

### The normalized result contract

Every adapter, regardless of provider, yields exactly one of:

- a **success stream** (normalized SSE events + final usage report) — and
  "success" is semantic, not just an HTTP status: a response that carries no
  content-bearing delta, ends before its protocol terminator, or fails
  framing/JSON validation is **not** a success, and
- a **typed failure**: error class (`authentication`, `rate_limited`,
  `provider_unavailable`, `provider_rejected_request`, `context_too_large`,
  `invalid_upstream_response`, `upstream_error`,
  `stream_failed_after_commitment`), provider error payload (preserved for
  debugging), and whether the failure is retryable.

`provider_rejected_request` is the provider-side class for "the upstream
refused this request" (invalid parameters for that model, unsupported
feature). The bare name `invalid_request` is deliberately **not** used here —
it is reserved for the gateway's own admission rejections (ADR 0004), and two
different failure kinds with one name is how an on-call engineer misroutes a
page.

`invalid_upstream_response` is the class for empty or malformed provider
output before commitment — it is a provider fault, so it is retryable within
the candidate and fallback-eligible after the retry budget is spent. It must
never be presented to the client as an empty successful answer.

The router's fallback policy is a pure function of `(error class, retryable,
committed?)` — it never parses provider payloads. Adapters normalize; the
router decides. The success shape carries the usage report in the terminal
chunk of a stream (OpenAI-compatible norm), so settlement has one place to
read it from.

### The v1 client-facing surface is the OpenAI-compatible chat-completions protocol

The client contract is **an OpenAI-compatible Chat Completions API**
(`POST /v1/chat/completions`, streaming via SSE) — the de-facto integration
surface every existing SDK already speaks. Concretely:

- the client addresses a **logical alias** in `model`; a provider model ID is
  never a valid value there;
- idempotency travels in an **`Idempotency-Key` request header** (ADR 0004);
- streamed usage arrives in the **terminal chunk** (OpenAI
  `stream_options.include_usage` semantics), matching the normalized result
  contract above;
- the v1 surface is **chat completions only** — embeddings, images, audio,
  and responses-API-style surfaces are deferred; adding one is a new ADR, not
  an extension of this model.

This is a domain decision, not a detail: alias scoping, idempotent replay,
and usage capture at the delivery boundary all hang off this protocol. The
concrete OpenAPI document (paths, schemas, error bodies, status codes) is
**derived** from it in `api/openapi/runtime.yaml`; this ADR fixes the
semantics, and that document is where they reach a client.

### Egress sits below adapters

A `Backend` names an egress policy (which proxy pool / direct). Adapters
receive a dialer configured per that policy. Egress concerns — proxy health,
rotation, IP affinity — are invisible to routing, translation, and billing:
**egress is infrastructure under the adapter boundary**, not a domain concept
a business decision can depend on.

### Kilo is served by the generic OpenAI-compatible backend

"Kilo" is an upstream provider that speaks an OpenAI-compatible protocol. In
the domain model it appears **only** as a `Backend` row of adapter type
`openai-compatible` (endpoint, credentials, egress policy) and as candidate
entries referencing that backend — exactly like any other OpenAI-compatible
endpoint.

**`kilo-free-gateway` is not a domain concept.** Whether Kilo traffic is
reached directly or via a separate proxying service is an egress/deployment
choice below the adapter boundary; the domain model requires no
provider-specific construct for it, and a PR proposing one is out of scope of
this model by construction. New providers that speak an existing protocol
family are configuration (a new `Backend` + candidates), not code.

## Consequences

- Adding a provider = adding a `Backend` (+ maybe an adapter if a new protocol
  family). Routing, fallback, and billing are untouched.
- The OpenAPI contract exposes logical aliases only; provider model IDs never
  appear in it.
- Attempt-level telemetry (per-candidate latency, retry counts, error classes)
  falls out of the attempt rows and feeds analytics without new
  instrumentation concepts.
- The commitment rule pins streaming behaviour: once bytes flow, the request's
  fate is bound to one provider. Clients that need cross-provider resilience
  get it only at request granularity, not mid-stream.

## Alternatives considered

- **Adapter-owned fallback** (adapter tries other providers): rejected — an
  adapter would need alias/catalog/billing knowledge; every protocol family
  would reimplement fallback differently, and double-billing protection would
  live in N places.
- **Global retry budget shared with fallback** (a single attempt counter):
  rejected — it makes "one flaky provider" indistinguishable from "alias
  unavailable", and hides provider-side retry cost from analytics.
- **Fallback after commitment with stream rewind**: rejected — impossible to
  do correctly for SSE (client-visible corruption), and no major gateway does
  it; the commitment gate is the honest boundary.
- **Post-commitment continuation fallback** (rebuild the input from the
  partial output and finish on another provider, merging both streams'
  usage): rejected at this stage — it changes the model's answer to
  "what did the client receive?" from one provider to a synthesis of two,
  which no accounting invariant in ADR 0004 can price honestly; revisit only
  with an explicit ADR.
- **Passthrough-only model names** (no logical aliases): rejected — passthrough
  is just an alias whose candidates name one provider; keeping the alias layer
  uniform is what makes fallback and entitlement scoping expressible at all.
