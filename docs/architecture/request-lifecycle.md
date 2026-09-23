# Request lifecycle

Reference page for what happens to one request, step by step, and what is
deliberately **not** part of that path. Decisions live in
[ADR 0001](../adr/0001-bounded-contexts-and-aggregates.md) (transaction
boundaries) and [ADR 0004](../adr/0004-reserve-and-settle-accounting.md)
(reserve/settle); routing behaviour in [routing](routing.md).

## Synchronous request path

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
earlier ones. Steps 1–2 read Identity; step 3 reads Catalog; steps 4–5 write
Commerce+Accounting (the admission transaction); steps 6–8 are Execution; step
9–10 are the settlement transaction (Accounting); step 11 returns.

| #   | Step                   | What happens                                                                                                                                                                                                                                                                                                                                                             | On failure                                                                                                                                                                                                                                                                                                                                              |
| --- | ---------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1   | API key authentication | Key hash lookup; key must be `active`. No key semantics beyond identity (scoping is an open question, [overview](overview.md)).                                                                                                                                                                                                                                          | Reject `unauthenticated` — no account context, nothing written.                                                                                                                                                                                                                                                                                         |
| 2   | Account identification | Key → account; account must be `active`. The account is the billing subject for everything below.                                                                                                                                                                                                                                                                        | Reject `account_suspended` / `account_closed` — a `rejected` request row is written (the account context exists from here on).                                                                                                                                                                                                                          |
| 3   | Model alias resolution | Alias lookup, one consistent candidate snapshot (ADR 0001, rule 3); alias must be `active`. Also validates the request's canonical `max_output_tokens` (present, positive, within the alias limit) and counts input tokens with the gateway's canonical tokenizer.                                                                                                       | Reject `unknown_alias` or `invalid_request` (missing/over-limit output bound) — a `rejected` request row is written with the fields known so far.                                                                                                                                                                                                       |
| 4   | Admission decision     | Check the client-supplied idempotency key against the relational intake record (scope `(account, key)`; same digest → return the original outcome metadata, different digest → reject `idempotency_conflict`). Then resolve matching entitlements (ADR 0003 waterfall) and the admission-time price revision; decide servability: entitled capacity, or PAYG if enabled. | Reject `insufficient_entitlement` (no PAYG / available balance short), `no_access` (no entitlement, PAYG off), or `idempotency_conflict`. Every request that passes account identification — including rejections — gets a request row at admission with its reason; an `idempotency_conflict` reuses the original request's row and writes no new one. |
| 5   | Reservation            | The admission transaction: insert the `Request` shell and its intake record, create the `Reservation` (`open`) priced at the admission revision, conditionally reserve the waterfall buckets, append `hold` legs, start the execution lease.                                                                                                                             | Transaction aborts atomically — no hold exists; the client sees a transient error and an idempotent replay re-admits.                                                                                                                                                                                                                                   |
| 6   | Candidate selection    | Router picks the first admissible candidate (candidate order + backend state; [routing](routing.md)).                                                                                                                                                                                                                                                                    | Reject `no_candidate` — reservation is released (compensation) and release legs appended.                                                                                                                                                                                                                                                               |
| 7   | Attempt execution      | Adapter translates and calls the backend, enforcing the reservation's output ceiling; each upstream call appends an attempt row (retries included); no DB transaction is open here; the lease is committed state renewed in its own short write while the request runs.                                                                                                  | Per-attempt failure classes feed step 8; provider-side cost is recorded, never client-billed.                                                                                                                                                                                                                                                           |
| 8   | Fallback if allowed    | Pre-commitment failure (including `invalid_upstream_response`) → next candidate (repeat 6–8). After commitment: no fallback, ever (ADR 0002).                                                                                                                                                                                                                            | All candidates exhausted → reject `no_candidate_succeeded`; release the reservation.                                                                                                                                                                                                                                                                    |
| 9   | Usage capture          | From the committed attempt, at the **customer-visible delivery boundary**: provider-reported usage (`reported`), else forwarded-delta counting (`gateway_observed`), else the held amount (`reservation_floor`); ADR 0004.                                                                                                                                               | Unknowable usage still settles conservatively; a fact is never invented and never exceeds the hold.                                                                                                                                                                                                                                                     |
| 10  | Settlement             | The settlement transaction: create the unique `Settlement`, append the `UsageEvent`, append consume legs (split order preserved) and release legs for the unconsumed tail, update bucket projections, close the reservation (`settled`), finalise the request.                                                                                                           | Process death before settlement → lease dies, reaper expires the reservation (state `expired`). Settlement after expiry is forbidden; a proven orphaned completion is an `unbillable_orphaned` usage event, never a customer charge — under-accounting is possible, over-billing is not.                                                                |
| 11  | Response               | Success body / stream (started earlier for streaming — see below) with the request's final status recorded.                                                                                                                                                                                                                                                              | —                                                                                                                                                                                                                                                                                                                                                       |

### Streaming places step 11 earlier — the lifecycle does not change

For a streamed response, content begins flowing to the client between steps 7
and 8 (that moment **is** commitment; pre-content lifecycle frames are
buffered, [routing](routing.md)). The remaining semantics hold unchanged:

- commitment freezes the candidate (no fallback from step 8 on);
- the executor renews the reservation's **execution lease** for as long as
  the stream runs, so the hold cannot be expired under a live request;
- steps 9–10 run when the stream completes (or fails) — **asynchronous to
  the client's read, but direct database transactions**: no queue sits
  between the stream's end and the settlement write, so there is no window
  where a settlement is "pending delivery". If the process dies mid-stream,
  the lease dies with it and the reaper releases the hold;
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
  → **release** the reservation (release legs; state `released`).
- Process crash mid-flight → lease dies; the reservation **expires** via the
  reaper. A completion proven orphaned afterwards is an
  `unbillable_orphaned` usage event — under-accounting is possible,
  over-billing is not.
- Success → **settle** (consume per bucket, release the tail). Exactly once,
  by the settlement's uniqueness constraint.

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
  (ADR 0005).

There is no second metering pipeline: the usage event written at step 9 is
the single substrate for usage analytics.

## Future payment-provider integration — a third, disjoint path

Payments (topping up PAYG balance, charging subscriptions at renewal) enter
exclusively as:

```text
payment provider webhook → (verified) → append `topup` / subscription lifecycle event
```

- Payment events append ledger entries and drive subscription lifecycle
  transitions; they never touch the request path. The request path's only
  dependency on payments is reading the PAYG balance and subscription state
  they maintain.
- The ledger entry kinds (`topup`, `grant`) and the subscription states
  (`suspended` for payment failure, etc.) already exist in this model — the
  integration adds a **writer**, not new domain concepts.
- Until that integration exists, top-ups are recorded by the operator through
  whatever internal surface comes first; the accounting shape is identical.

This separation is what lets the gateway ship and operate before any payment
provider is chosen, without re-modelling when one is.
