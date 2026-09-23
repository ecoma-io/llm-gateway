# Request lifecycle

Reference page for what happens to one request, step by step, and what is
deliberately **not** part of that path. Decisions live in
[ADR 0001](../adr/0001-bounded-contexts-and-aggregates.md) (transaction
boundaries) and [ADR 0004](../adr/0004-reserve-and-settle-accounting.md)
(reserve/settle); routing behaviour in [routing](routing.md); which application
owns which step and which record in [planes](planes.md).

**Every step below runs in the Data Plane.** The request path is the runtime's
— `apps/dataplane` — from the first byte of authentication to the last byte of
the response, and the Control Plane is not on it
([ADR 0006](../adr/0006-control-plane-and-data-plane.md) §4). Two steps used to
be the exception and are annotated where they appear: the `hold` legs of step 5
and the settlement of step 10 are written by the Control Plane, from the
runtime's facts, after the runtime's own transaction has committed. Nothing on
this path calls `console-api` or `dataplane-api`, reads the Control Plane's
database, or fails because either is unavailable.

## The runtime request path

```text
 API key authentication
        ↓
 account identification
        ↓
 model alias resolution
        ↓
 entitlement / admission decision
        ↓
 reservation
        ↓
 candidate selection
        ↓
 attempt execution
        ↓
 fallback if allowed
        ↓
 usage capture
        ↓
 settlement
        ↓
 response
```

The ordering below is normative: later steps rely on guarantees established by
earlier ones. Steps 1–2 read Identity; step 3 reads Catalog; steps 4–5 are the
admission transaction, which writes the runtime's own rows — the request, the
intake record, the reservation and its drawdown on the quota projection;
steps 6–8 are Execution; steps 9–10 close the reservation and hand the usage
fact on; step 11 returns. The contexts are unchanged; what changed is where
their rows sit, and the two steps that cross a plane are marked.

| #   | Step                   | What happens                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   | On failure                                                                                                                                                                                                                                                                                                                                              |
| --- | ---------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1   | API key authentication | Key hash lookup against the runtime's **own** key record — its projection of Control-Plane key state, delivered so that authentication costs no cross-plane call; key must be `active`. No key semantics beyond identity (scoping is an open question, [overview](overview.md)).                                                                                                                                                                                                                               | Reject `unauthenticated` — no account context, nothing written.                                                                                                                                                                                                                                                                                         |
| 2   | Account identification | Key → account; account must be `active`. The account is the billing subject for everything below.                                                                                                                                                                                                                                                                                                                                                                                                              | Reject `account_suspended` / `account_closed` — a `rejected` request row is written (the account context exists from here on).                                                                                                                                                                                                                          |
| 3   | Model alias resolution | Alias lookup in the runtime's own alias and candidate rows, one consistent candidate snapshot (ADR 0001, rule 3); alias must be `active`. Also validates the request's canonical `max_output_tokens` (present, positive, within the alias limit) and counts input tokens with the gateway's canonical tokenizer.                                                                                                                                                                                               | Reject `unknown_alias` or `invalid_request` (missing/over-limit output bound) — a `rejected` request row is written with the fields known so far.                                                                                                                                                                                                       |
| 4   | Admission decision     | Check the client-supplied idempotency key against the relational intake record (scope `(account, key)`; same digest → return the original outcome metadata, different digest → reject `idempotency_conflict`). Then resolve matching entitlements (ADR 0003 waterfall) and the admission-time price revision; decide servability: entitled capacity, or PAYG if enabled.                                                                                                                                       | Reject `insufficient_entitlement` (no PAYG / available balance short), `no_access` (no entitlement, PAYG off), or `idempotency_conflict`. Every request that passes account identification — including rejections — gets a request row at admission with its reason; an `idempotency_conflict` reuses the original request's row and writes no new one. |
| 5   | Reservation            | The admission transaction, entirely in the Data Plane: insert the `Request` shell and its intake record, create the `Reservation` (`open`) priced at the admission revision, draw the waterfall down from the runtime's **quota projections** by conditional update, start the execution lease. The `hold` legs are **not** written here — they are Control-Plane rows, derived from the committed reservation afterwards, idempotently by `request_id`.                                                       | Transaction aborts atomically — no hold exists; the client sees a transient error and an idempotent replay re-admits.                                                                                                                                                                                                                                   |
| 6   | Candidate selection    | Router picks the first admissible candidate (candidate order + backend state; [routing](routing.md)).                                                                                                                                                                                                                                                                                                                                                                                                          | Reject `no_candidate` — the reservation is released (compensation): the runtime returns the capacity to its projection, and the Control Plane derives the release legs from that state change.                                                                                                                                                          |
| 7   | Attempt execution      | Adapter translates and calls the backend, enforcing the reservation's output ceiling; each upstream call appends an attempt row (retries included); no DB transaction is open here; the lease is committed state renewed in its own short write while the request runs.                                                                                                                                                                                                                                        | Per-attempt failure classes feed step 8; provider-side cost is recorded, never client-billed.                                                                                                                                                                                                                                                           |
| 8   | Fallback if allowed    | Pre-commitment failure (including `invalid_upstream_response`) → next candidate (repeat 6–8). After commitment: no fallback, ever (ADR 0002).                                                                                                                                                                                                                                                                                                                                                                  | All candidates exhausted → reject `no_candidate_succeeded`; release the reservation.                                                                                                                                                                                                                                                                    |
| 9   | Usage capture          | From the committed attempt, at the **customer-visible delivery boundary**: provider-reported usage (`reported`), else forwarded-delta counting (`gateway_observed`), else the held amount (`reservation_floor`); ADR 0004.                                                                                                                                                                                                                                                                                     | Unknowable usage still settles conservatively; a fact is never invented and never exceeds the hold.                                                                                                                                                                                                                                                     |
| 10  | Settlement             | **Crosses the plane, and is therefore two writes.** The runtime writes the `UsageEvent` and closes the reservation (`settled`) in one Data-Plane transaction. The Control Plane then creates the unique `Settlement`, appends the consume legs (split order preserved) and the release legs for the unconsumed tail, and updates its bucket projections — from that fact, idempotently by `request_id`. The runtime's half never waits for the Control Plane's, and the Control Plane's half follows the fact. | Process death before settlement → lease dies, reaper expires the reservation (state `expired`). Settlement after expiry is forbidden; a proven orphaned completion is an `unbillable_orphaned` usage event, never a customer charge — under-accounting is possible, over-billing is not.                                                                |
| 11  | Response               | Success body / stream (started earlier for streaming — see below) with the request's final status recorded.                                                                                                                                                                                                                                                                                                                                                                                                    | —                                                                                                                                                                                                                                                                                                                                                       |

### Streaming places step 11 earlier — the lifecycle does not change

For a streamed response, content begins flowing to the client between steps 7
and 8 (that moment **is** commitment; pre-content lifecycle frames are
buffered, [routing](routing.md)). The remaining semantics hold unchanged:

- commitment freezes the candidate (no fallback from step 8 on);
- the executor renews the reservation's **execution lease** for as long as
  the stream runs, so the hold cannot be expired under a live request;
- steps 9–10 run when the stream completes (or fails) — **asynchronous to
  the client's read, but direct database transactions**: no queue sits
  between the stream's ending and the runtime writing the usage fact and
  closing the reservation. The Control Plane's settlement is a **second**
  write and can lag the first: that window is real, bounded and named
  (ADR 0006), not an impossibility, and it does not delay the client, whose
  response already ended. If the process dies mid-stream, the lease dies with
  it and the reaper releases the hold;
- the client never waits for settlement to see its first content, and
  settlement never waits for the client to finish reading;
- a client disconnect ends delivery: billable usage is what was already
  forwarded (capture method `gateway_observed`), upstream is cancelled where
  the protocol allows, and the provider's usage for the cancelled remainder
  is provider-cost telemetry only (ADR 0004).

### Failure and compensation summary

- Rejection from account identification onward but before reservation → the
  request row is written with `rejected` status and its reason. An
  `unauthenticated` request writes nothing — there is no account to attach a
  row to. An `idempotency_conflict` writes no new row either; the original
  request's record answers.
- Failure after reservation but before any attempt could serve (steps 6–8)
  → **release** the reservation: the runtime returns the capacity to its
  projection and records the terminal state (`released`), and the Control
  Plane derives the release legs from it.
- Process crash mid-flight → lease dies; the reservation **expires** via the
  reaper, on the same terms. A completion proven orphaned afterwards is an
  `unbillable_orphaned` usage event — under-accounting is possible,
  over-billing is not.
- Success → **settle** (consume per bucket, release the tail). Exactly once,
  by the settlement's uniqueness constraint — which is a Control-Plane
  constraint, and the reason a redelivered usage fact costs nothing.

## Post-request fact delivery — a separate path

The runtime's journey ends at step 11. What happens to the usage fact afterwards
is a different path with a different schedule, and it is not a step 12: nothing
on the list above waits for it, and a Control Plane that is down when the fact
commits changes nothing about the request that produced it.

```text
UsageEvent committed durably (step 10)
        │
        ▼
dataplane private listener        ── the management surface, not the runtime surface
        ▲
dataplane-api management façade   ── forwards the page; owns no cursor and no fact
        ▲
console-api poller                ── owns the position it has applied through
        │
        ▼
Control Plane settlement          ── derived from the fact, idempotent by request_id
```

- **Pull, not push.** The runtime never notifies anyone and never calls the
  Control Plane; the consumer asks for facts at its own pace, over the
  authenticated management chain in [planes](planes.md). The fact is durable in
  the meantime, so a Control Plane outage loses nothing and delays nothing.
- **The cursor belongs to the consumer.** The Data Plane issues an opaque
  position and the Control Plane stores the `next_cursor` of the last page it
  applied, in the same local transaction that records the effects of those
  facts. The runtime never learns that position, and no consumer may parse it.
- **Redelivery is the norm, not an error.** There is no acknowledgement, no
  consume-and-delete and no completion signal: the same page may be read any
  number of times, and applying a fact twice is a no-op because `request_id` is
  the idempotency key. That is what makes a retry free and a crash between
  applying and advancing safe.
- **The client never sees any of it.** The response ended at step 11. The
  Control Plane's settlement is a second write, and it is invisible from the
  outside: the console reads the ledger, and the runtime does not wait for it.

The protocol, its ordering guarantee and its failure model are
[cross-plane protocols](cross-plane-protocols.md); the layering that carries it
is [ports and adapters](ports.md). The delivery model is ADR 0006 §5, and it is
why step 10 could be split across the plane boundary without the request path
noticing.

## The console management path — the other disjoint path

The console's work is a different journey with a different shape, and the two
must not be confused: nothing on the list above serves a management request,
and nothing below serves a completion.

```text
browser → console-api ──┬── control database (identity, commerce, the ledger)
                        │
                        └── dataplane.Management → HTTP → dataplane-api
                                                              (Data Plane configuration)
```

- **It is the Control Plane's path.** `console-api` owns identity, commerce and
  the ledger; the browser reaches it and nothing else, through the generated
  console client ([planes](planes.md)).
- **Data Plane operations are delegated, never performed.** Where the console
  needs one — publishing an alias, rotating a key — the call is
  `console-api application → dataplane.Management → HTTP adapter →
dataplane-api`, and the runtime is not involved
  ([ADR 0006](../adr/0006-control-plane-and-data-plane.md) §5). The port is
  where the boundary is legible in code; the fact half of it has an adapter
  spoken today ([above](#post-request-fact-delivery--a-separate-path)), while
  the management half has none until the first management operation lands.
- **It runs at a different tempo.** This path is human-paced and low-volume;
  the runtime's is latency-bound and the product's availability. Neither
  shares a process, a deploy or a database with the other.

There is deliberately no normative step list for this path yet. The domains it
would walk through — sign-in, subscription, configuration — are not built, and
a numbered lifecycle for them would be a design the ADRs have not made. What
is decided is that the path exists, that it is disjoint from the runtime's, and
that its only way to reach the Data Plane is the management port.

## Asynchronous analytics — a separate path

Everything the business wants to know later — per-candidate latency, retry
counts, error-class frequencies, provider-side cost of failed attempts,
capture-method ratios — is derived from the event tables
(`requests`, `request_attempts`, `usage_events`) and the append-only ledger,
in queries/jobs that:

- never write to the relational working set (balances are never "fixed up" by
  a job — corrections are compensating ledger entries, invariant 2);
- never sit in the synchronous path's latency budget;
- read continuous aggregates over the event tables when volume demands
  (ADR 0005);
- read either database from one side of the boundary, never both in one
  statement. Operational analytics ("which candidate was slow?") reads the
  event tables, which are the Data Plane's; financial analytics reads the
  ledger, which is the Control Plane's; an analysis that needs both joins the
  facts by `request_id` outside the serving path, not by crossing a boundary
  inside one.

There is no second metering pipeline: the usage event written at step 9 is
the single substrate for usage analytics.

## Future payment-provider integration — its own disjoint path

Payments (topping up PAYG balance, charging subscriptions at renewal) enter
exclusively as:

```text
payment provider webhook → (verified) → append `topup` / subscription lifecycle event
```

- Payment events append ledger entries and drive subscription lifecycle
  transitions; they never touch the request path. The request path's only
  dependency on payments is the capacity a top-up publishes to the runtime's
  projection — it reads that row, never the balance the payment moved.
- The ledger entry kinds (`topup`, `grant`) and the subscription states
  (`suspended` for payment failure, etc.) already exist in this model — the
  integration adds a **writer**, not new domain concepts.
- Until that integration exists, top-ups are recorded by the operator through
  whatever internal surface comes first; the accounting shape is identical.

This separation is what lets the gateway ship and operate before any payment
provider is chosen, without re-modelling when one is.
