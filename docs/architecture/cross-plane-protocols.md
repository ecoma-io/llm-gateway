# Cross-plane protocols

Reference page for what crosses the boundary between the Control Plane and the
Data Plane, and how it is delivered. The decisions are
[ADR 0006](../adr/0006-control-plane-and-data-plane.md) §5 (the communication
rules) and §9 (the management surface and its service-auth boundary); that
record wins over this page wherever the two disagree. Which record belongs to
which plane is [planes and ownership](planes.md); how the calls are layered in
code is [ports and adapters](ports.md); the money the second direction feeds is
[accounting](accounting.md).

This page exists for the next cross-plane change. A new message between the
planes is either a configuration in the first direction or a fact in the
second, and the two have different obligations — a change that mixes them is
the mistake this page is written to make visible.

## The two directions

| Direction      | Term                       | What crosses                                                                                        | Keyed by                                 |
| -------------- | -------------------------- | --------------------------------------------------------------------------------------------------- | ---------------------------------------- |
| Control → Data | configuration / projection | API-key runtime projection, entitlement and quota grants, catalog and routing config, egress policy | the entity's own identifier — idempotent |
| Data → Control | fact / observation         | usage facts, request-outcome facts, quota-consumption facts, reconciliation state                   | `request_id` — idempotent                |

Neither direction is "data synchronization": one side states what the Data
Plane should hold, the other states what it observed. A projection in the first
row is a delivered copy the Data Plane reads on the hot path; a fact in the
second is not a copy of anything — it is the record.

## Control → Data: an idempotent command

A management call asks the Data Plane to hold something: a catalogue row, a
candidate order, a price revision, a key's runtime state, a capacity grant. The
call is request/response, it is keyed by the entity's own identifier, and it is
idempotent — applying the same instruction twice leaves the Data Plane in the
same state as applying it once, which is what makes a retry safe without a
distributed transaction.

Three properties hold for every call in this direction:

- **It is never on the LLM request path.** The runtime reads what it already
  holds; it does not wait for a management call to serve a completion
  (ADR 0006 §4).
- **It can be refused.** The Data Plane is the authority for what it serves, so
  a management call is a request the Data Plane may reject, and the caller
  handles the rejection rather than assuming the change landed.
- **Delivery to the runtime is a named, bounded property.** Where the
  instruction seeds a projection the hot path reads — a key's revocation, a
  capacity grant — the projection is stale by a bounded window rather than
  consistent on commit, and that bound is a deployment property rather than an
  accident (ADR 0006 §8).

The transport is the chain
`console-api application → DataPlaneManagementPort → HTTP adapter → dataplane-api → outbound port → HTTP adapter → dataplane private listener`
(ADR 0006 §9).

## Data → Control: facts, replayed

### The chain

```text
LLM → dataplane → UsageEvent committed durably
                        │
                        ▼
                 dataplane private listener        (internal, not the runtime surface)
                        ▲
                 dataplane-api management façade   (transport only, owns no state)
                        ▲
                 console-api poller                (owns its cursor)
                        │
                        ▼
                 Control Plane settlement
```

The runtime records the fact and stops there. It does not notify anyone, it has
no callback into the Control Plane, and it will serve the next request whether
or not the fact was ever read. The consumer arrives on its own schedule, over
the same authenticated management chain as the first direction, and pays its own
latency for the read (ADR 0006 §4, §5).

### The private hop is a protocol, not a contract

The hop from `dataplane-api` to the Data Plane's private listener is the one
message between the planes that nothing outside this repository reaches. It is
an implementation protocol between two processes of the same product, pinned on
each side by a literal route test, and it is deliberately not a fourth OpenAPI
document: `api/openapi/` holds the surfaces something outside this repository
talks to, and a document exists to state what such a caller may rely on. What a
caller does rely on is contracted where that caller's surface is —
`dataplane.yaml` declares `GET /internal/usage-events` on the façade, and
`shared/usage-facts.yaml` is the fragment both ends implement against. Changing
the private hop is therefore a change to two route tests and this page, and no
generated client moves (AGENTS.md rule 2).

### The cursor is opaque

The cursor names a position in the Data Plane's fact order, and it is issued by
the Data Plane alone. No consumer may parse it, synthesise one, compare two, or
order by it; `dataplane-api` forwards it verbatim even though it never uses it.
The encoding is the Data Plane's to change, and treating the value as
meaningful is what would make that change breaking
(`api/openapi/shared/usage-facts.yaml`).

The Control Plane stores the `next_cursor` of the last page it applied. An
empty stored position means "from the beginning of what is retained" — it is
the consumer's own convention and never a value the Data Plane issued.

### Replay and retries

Replay is normal, not an error path. The same range may be requested any number
of times and returns the same facts, because reading is side-effect free: there
is no acknowledgement endpoint, no consume-and-delete, and no way for a consumer
to tell the Data Plane it has finished with anything. A Control Plane outage
therefore loses no facts, and a redelivered fact costs nothing.

The retry rules follow from that:

- **The consumer advances only after applying.** The `next_cursor` is stored in
  the same local transaction that durably records the effects of the facts it
  covers, so a crash between applying and advancing replays the page instead of
  skipping it.
- **Applying a fact twice is a no-op.** `request_id` is the fact's immutable
  business identity and the logical idempotency key for everything derived from
  it — one settlement, one consume leg and one release leg per `request_id`,
  however many times the fact is delivered. It is not the HTTP `X-Request-Id`,
  which correlates a call and means nothing across a retry.
- **A transport failure is a retry; a refused position is not.** An unreadable
  or unreachable Data Plane is retried. A cursor past the retained history fails
  explicitly with `cursor_expired` (HTTP 410) and is never resumed from a newer
  position: silently skipping facts turns a retention decision into lost
  settlement, and it needs a human rather than another attempt.

### Reconciliation

The two planes converge rather than synchronise. The Control Plane derives its
records — the settlement, its consume and release legs, the bucket projections —
from the facts it has applied, idempotently by `request_id`, and it must be able
to do so **without a synchronous call back into the Data Plane**. That is why
the fact carries the allocation and bucket identities and the immutable
reservation figures needed to derive those legs and the settled total, and why
the fact feed carries at most one settlement-relevant terminal fact per runtime
request (ADR 0006 §5 and its amendments to
[ADR 0004](../adr/0004-reserve-and-settle-accounting.md); the contract's
requirement is stated in `api/openapi/shared/usage-facts.yaml`).

What reconciliation cannot repair is a fact that was never written or never
read within retention; it repairs a **lag**, not an absence. A reconciliation
path that needed to ask the Data Plane a question mid-settlement would be a
synchronous cross-plane call wearing the fact model's clothes, and it is the one
thing this protocol does not permit.

## The failure model

Stated once, here. Every cross-plane behaviour reduces to one of these four
lines:

```text
Control unavailable  → Data continues serving
Data unavailable     → management operations fail or defer
Delivery failure     → the fact stays durable, replay later
Duplicate delivery   → idempotent no-op
```

The first line is what the split exists for (ADR 0006 §2, §4); the second is
why a management call is request/response with a defined failure rather than a
two-phase commit; the third is why the consumer, not the producer, holds the
position; the fourth is why nothing in the derivation needs a lock.

## What this page does not decide

- **The durable fact source.** `usage_events` is the future table the facts are
  read from; its columns, its ordering sequence and its retention are the schema
  PR's, and this page names the requirements they have to satisfy rather than
  the shape (ADR 0006 §5; [data implications](data-implications.md)).
- **The ingestion-cursor schema.** `control.usage_ingestion_cursor` is the
  future table the Control Plane stores its position in. Today the position is a
  repository-level port in `apps/console-api/internal/ports/outbound/persistence`
  with the future table documented beside it and test fakes behind it; a
  production implementation arrives with the schema rather than before it. On
  the other side of the same seam, the Data Plane's production fact reader
  refuses with a source-unavailable error instead of serving an in-memory feed
  that would lose every fact on restart.
- **The settlement consumer loop.** Nothing schedules the replay: there is no
  worker, no ticker and no background process. The loop that calls the ingestion
  use case belongs to the pull the schema PR builds, beside the table the cursor
  is stored in.
- **mTLS or signed service credentials.** The mechanism today is a shared secret
  per hop, and either replacement proves the same identity cryptographically
  without changing application semantics (ADR 0006 §9).
