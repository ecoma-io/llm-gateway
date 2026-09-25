# Routing

Reference page for the routing pipeline. The decision and its rationale are
[ADR 0002](../adr/0002-routing-and-fallback-ownership.md); this page is the
operational detail.

## The pipeline

```text
client: model = "<logical model alias>"
        │
        ▼
 ① alias resolution            Catalog lookup, one consistent snapshot of the alias + candidates
        │
        ▼
 ② candidate selection          position order (1 = primary); skips only candidates whose
        │                       backend is disabled (see "Policy inputs" below)
        ▼
 ③ candidate execution          adapter translates the request for this backend's protocol;
        │                       bounded provider retries (same candidate) happen inside this step
        ▼
 ④ normalized result            success stream (+ usage report) or typed failure
        │
        ├── failure before commitment ──▶ ⑤ logical fallback: advance to next candidate → ③
        │
        └── success / committed ────────▶ ⑥ return to client
```

Every pass through ③ is one `RequestAttempt` row; retries inside ③ append
additional attempt rows against the **same** candidate. ⑤ never happens once
a response is committed.

**The pipeline starts where admission ends.** A request reaches step ① only
after the runtime's admission transaction has committed — key authenticated,
account identified, intake checked, alias resolved and priced, hold secured
against the quota projection, reservation row on disk
([request lifecycle](request-lifecycle.md)). Admission and routing are two
separated concerns with an order between them, not one pipeline's stages: a
request that did not commit admission never reaches a router, and the router
has no admission decision of its own to make. What admission hands the router
is a **committed state**, not a verdict in flight: the reservation exists, the
capacity is drawn, and the router's failure modes are therefore never
admission's.

Steps ①–⑥ run inside `apps/dataplane`, the Data Plane runtime, and nowhere
else. Alias resolution and the configuration it reads — aliases, candidates,
backend and egress policy — come from the runtime's own persistence and
cache, never from a per-request call to a management API: an LLM request
reaches the runtime and the provider behind it, and the Control Plane being
down does not make a `/v1/*` request fail
([ADR 0006](../adr/0006-control-plane-and-data-plane.md) §4).

## Layer ownership

```text
┌───────────────────────────────────────────────────────────────┐
│ Router        alias → candidates · selects · owns cross-provider logical fallback │
├───────────────────────────────────────────────────────────────┤
│ Adapters      provider-protocol translation · normalized results · provider retry │
├───────────────────────────────────────────────────────────────┤
│ Egress        proxy pools, rotation, health; invisible above this line           │
└───────────────────────────────────────────────────────────────┘
```

All three layers live in one place. `apps/dataplane` — the Data Plane runtime
— is their only home: the runtime is the process that serves `/v1/*`, so there
is no second implementation of the router, the adapters or egress, and no
plane for one to drift into. `console-api` owns the Control Plane — identity,
commerce, the console's orchestration — and never gains a routing type; when
the console needs a Data Plane operation it goes through the management
surface, `dataplane-api`, which configures this domain rather than
re-implementing it ([ADR 0006](../adr/0006-control-plane-and-data-plane.md)
§§3, 9). The separation is mechanical rather than conventional: each backend
is its own Go module, so a routing type here does not compile into the Control
Plane, and architecture tests assert the rest (§4).

Hard rules (ADR 0002):

- Adapters never fall back, never see aliases, never see billing.
- The router never parses provider payloads; it reacts to typed failures.
- Egress is configurable per backend and invisible to routing and billing.
- The domain exposes configuration as data (`Backend`, candidates); no
  provider-specific code path exists for a provider that speaks an existing
  protocol family.

## Retry vs fallback

|                          | Provider retry                                | Logical fallback                                 |
| ------------------------ | --------------------------------------------- | ------------------------------------------------ |
| Owner                    | Adapter                                       | Router                                           |
| Target                   | Same candidate, same provider model           | Next candidate, possibly another provider        |
| Triggers                 | Retryable error classes + connect timeouts    | Non-retryable failure, or retry budget exhausted |
| Budget                   | Bounded per candidate (backoff)               | Bounded by the candidate list length             |
| Allowed after commitment | No (nothing is re-sent to the client)         | **Never**                                        |
| Recorded as              | Additional attempt rows on the same candidate | Attempt rows on the next candidate               |
| Billing effect           | None for failed attempts (invariant 6)        | The committed attempt is the only billed subject |

Failure classes and their disposition (the router's decision table — the
adapter only classifies):

| Error class                                                       | Retryable? | Fallback?                                         |
| ----------------------------------------------------------------- | ---------- | ------------------------------------------------- |
| `rate_limited`                                                    | yes        | yes, if retry budget exhausted                    |
| `provider_unavailable`                                            | yes        | yes, if retry budget exhausted                    |
| `upstream_error` (5xx, unknown)                                   | yes        | yes, if retry budget exhausted                    |
| `invalid_upstream_response` (empty/malformed body before content) | yes        | yes, if retry budget exhausted                    |
| `provider_rejected_request` (upstream refused the request)        | no         | no — the client's request is at fault; surface it |
| `context_too_large`                                               | no         | no — surfacing it beats silently truncating       |
| `authentication` (our credentials)                                | no         | no — an operator problem, surfaced loudly         |
| `stream_failed_after_commitment`                                  | no         | no — commitment gate (invariant 7)                |

`provider_rejected_request` and `context_too_large` deliberately do **not**
fall back: the same request against the next candidate would fail identically
or silently produce a different answer. Fallback exists for provider
availability, not for client errors. (The name `invalid_request` is reserved
for the gateway's own admission rejections — ADR 0002 keeps the two families
of failure lexically distinct.)

## The handoff: no candidate is a determinate answer

Admission commits, and then the request is handed to routing. The handoff
between them is narrow: the reservation exists and the capacity is drawn, and
the router reads the alias and its candidates. There is no third thing the
router can discover at that moment that would make admission wrong — quota is
already secured, the alias is already resolved and the output bound already
fixed — so **the absence of a candidate is a determinate refusal, not a
transient state**: a request whose alias has no admissible candidate is
refused, not queued for one to appear.

That refusal is a **release**, not a rollback: the request is an admitted
request whose hold is returned whole, and the routing stage's compensation
transaction — enter through the reservation close's compare-and-set, return
the legs in stored order, finalise the request and its intake record, and
append the `released` usage fact **last** — runs before the client is
answered, so a compensation and the fact that reports it commit or vanish
together. The answer is `503 overloaded_error` with a fixed `Retry-After`,
because "nothing can serve this right now" is the true reading whether or not
a later pass would serve it. The capacity is never held for a request that
will not be attempted, and the Control Plane's projection is returned to
through the same `released` fact every other release uses. The same unit
answers an **exhausted** walk — every eligible candidate tried and fallen
through — with the identical cell: how deep the walk went before it knew
changes nothing a client can act on, and which of the two refusals produced
an answer is the log line's fact, never the wire's.

The stage exists in code as the application's routing half
(`apps/dataplane/internal/application`, `ChatRouting`), wrapped around
admission as the chat route's single use case: admission admits, and every
admitted request is walked — eligibility first (`domain/routing`), then one
try per eligible candidate in catalog order, each dispositioned by the two
tables ([ADR 0002](../adr/0002-routing-and-fallback-ownership.md)) — and
answered through the request's reply. The reply is the transport's half of
the same channel the executors write content into, which is what makes the
commitment point real on the wire: the first content byte freezes the
status line and the framing, and every ending after it knows which side of
the gate it stands on. The candidate **execution** layer — the adapters that
translate a request for a provider's protocol and call it — is deliberately
absent: the registry the stage reads is empty until B10 lands the first
executor, so today every admitted request is released as a no-candidate
answer, byte-identical with the endpoint's behaviour before the stage
existed. The walk, its dispositions, its endings and their transactions are
landed and pinned by tests; B10 registers an executor and the walk starts
trying candidates, and nothing above this paragraph changes when it does.

## Commitment

Commitment is the moment the gateway forwards the first **content-bearing**
byte to the client — for streaming, the first content delta (text or tool-call
argument fragments; both are content); for a non-streaming response, the first
byte of the body. It is a one-way gate:

- **Before commitment** — the client has received no content. Protocol
  lifecycle frames (role preambles, `message_start`-style metadata) are
  **buffered, not forwarded**, so a provider that accepts the request and
  dies before content is still fallback-eligible: the buffer is discarded and
  the next candidate restarts the stream cleanly. The request may be served
  by any candidate; failed attempts are invisible to the client.
- **After commitment** — the response belongs to exactly one candidate. A
  failure is delivered as a terminal error event inside the (already-200)
  stream; the attempt is `failed_after_commitment`; usage settles on what was
  delivered (invariant 8). A client disconnect mid-stream is the same case
  from the gateway's side: the attempt stays the committed one, the gateway
  stops relaying (and cancels upstream where the protocol allows), and usage
  settles on best-known delivery (capture method `gateway_observed`).

The buffer is bounded: when a pre-content buffer reaches its cap the gateway
commits by flushing it, trading a sliver of fallback window for memory — a
cap-sized preamble is content for commitment purposes.

The gate is also where the hold stops being a reservation and becomes a
cost bound. Before commitment, a failed attempt costs provider-side effort
only — the hold is intact, the fallback restart is free to the account, and
a release gives back exactly what was drawn. After commitment, usage settles
on what was delivered against the hold admission secured
([commerce](commerce.md)): the hold is the ceiling the failure cannot exceed,
and the lease renewed while the stream runs is what keeps the reaper from
taking that ceiling back mid-flight. Admission sized the hold before any
candidate ran precisely so that no routing outcome — fallback, exhaustion, or
mid-stream death — ever needs to resize it.

Consequence for clients: cross-provider resilience operates at request
granularity. The gateway never splices two providers' output into one
response, and no configuration option will make it.

## Policy inputs available to selection

The router may consider, in addition to candidate order (all derivable from
the model as it stands, or explicitly deferred):

- candidate/backend state (`active` \| `disabled`) — a disabled backend is
  skipped, not removed. Quota is not a selection input: the reservation is
  sized and secured at admission, before any candidate is chosen —
  [request lifecycle](request-lifecycle.md) — so every candidate of an
  admitted request is servable. That admission is **Data-Plane-local**: the
  runtime conditionally draws down its own **quota projection**, the
  enforcement ceiling for the entitlement cycle or PAYG balance the request
  lands in, seeded from Control-Plane grants and updated only by the runtime
  — while the ledger bucket it will eventually settle against is the Control
  Plane's, and remains the source of truth for money
  ([ADR 0006](../adr/0006-control-plane-and-data-plane.md); [accounting](accounting.md)).

Deliberately **not** in the model yet: weights, cost-based load balancing,
latency-based scoring, per-candidate traffic splits, and cooldowns
(temporarily skipping a candidate that has just failed, as LiteLLM
deployments do). They are policy add-ons that would live on `ModelCandidate`;
naming them here so a future PR knows where they go.

## Kilo, and why there is no `kilo-free-gateway` in this model

Kilo is an upstream provider reachable over an OpenAI-compatible protocol.
Serving it requires nothing new: a `Backend` with adapter type
`openai-compatible`, credentials, and an egress policy, plus candidates on
whatever aliases should route to it.

Whether traffic reaches Kilo directly or through a separate proxying service
(`kilo-free-gateway` or anything like it) is an **egress/infrastructure
choice below the adapter boundary**. The domain model therefore requires no
Kilo-specific construct, and none should be added: a PR that introduces one
is adding a deployment detail to the domain, which this model rejects by
construction (ADR 0002).
