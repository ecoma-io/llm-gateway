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
be the exception and are annotated where they appear: the `hold` legs of step 7
and the settlement of step 12 are written by the Control Plane, from the
runtime's facts, after the runtime's own transaction has committed. Nothing on
this path calls `console-api` or `dataplane-api`, reads the Control Plane's
database, or fails because either is unavailable.

## The runtime request path

```text
 API key authentication
        ↓
 account identification
        ↓
 request body scoping + digest
        ↓
 idempotency intake check (replay / in-flight / conflict)
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
earlier ones. Steps 1–2 read the runtime's own mirror of Identity; step 3
scopes the raw request body and computes its digest; step 4 checks the
relational intake record (`request_intake`) keyed by
`(account_id, idempotency_key)` — same digest returns the original outcome
(replay), in-flight returns `request_in_progress`, different digest returns
`idempotency_conflict`, and a digest matching an original the runtime
abandoned before any answer existed returns `request_abandoned`; steps 5–7
are the admission transaction, which reads
Catalog and writes the runtime's own rows — the request shell, the reservation
and its drawdown on the quota projection, and last the intake record; steps
8–10 are Execution; steps 11–12 close the reservation and hand the usage fact
on; step 13 returns. The contexts are unchanged; what changed is where their
rows sit, and the two steps that cross a plane are marked.

The intake check placement is deliberate: it runs **immediately after body
scoping, before alias resolution**, so the replay decision and its
deduplication are made against the raw bytes of the request, without any
downstream resolution that could vary. An in-flight request (same account and
key, earlier admission still open) returns `request_in_progress` with a short
fixed `Retry-After`; a conflicting digest returns `idempotency_conflict` with
no `Retry-After`; a matching digest returns the original request's id, its
terminal status and `replayed=true` — the answer is the record of what
happened, never a second execution of it. A matching digest whose original
the runtime abandoned before any answer existed is the one terminal shape
with no answer to re-serve: it returns `request_abandoned` — the record is
terminal, the key is spent, and no `Retry-After` is sent because nothing
about the condition clears.

| #   | Step                   | What happens                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 | On failure                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| --- | ---------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1   | API key authentication | Exactly one `Authorization: Bearer` header, parsed against the token grammar `gw_<uuid-v4>_<43-char base64url>`; a single statement resolves the runtime's **own** credential mirror — the key's digest, key state, revocation and the account's state in one joined read — and the presented secret is compared to the stored digest in constant time, always (a lookup miss burns the same comparison against a zero digest). No key semantics beyond identity (scoping is an open question, [overview](overview.md)).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     | Refuse `401 invalid_api_key` — **nothing is written at all**: no account context exists, and one fixed sentence answers all four causes (malformed header, malformed token, unknown key, wrong secret) so the response cannot tell a caller which one it was.                                                                                                                                                                                                                                                                                                                                                        |
| 2   | Account identification | The key's account must be `active`. The account is the billing subject for everything below. A credential that has arrived without its account's row is treated exactly like an unknown key, not as an active account: the two records arrive on independent feed entries, and a principal the mirror cannot fully state is a principal the runtime does not serve.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          | Refuse `403 account_suspended` / `account_closed` — a `rejected` request row is written (the account context exists from here on), in a transaction that holds no other work.                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| 3   | Request scoping        | The request body is read once, under a size cap, as **raw bytes**; its SHA-256 digest (lowercase hex) is the request's identity for every later comparison. The `Idempotency-Key` header is required (ADR 0004) and is itself grammar-checked (length and printable-ASCII, no whitespace). The body's _contents_ — whether it is JSON, the `model` field's grammar, the output ceiling — are deliberately not judged here: that judgment runs inside the admission unit, after the intake probe, so a body whose key is already spent answers from the record rather than as a fresh refusal.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                | The three refusals that precede any unit of work are the account gate, a key outside the grammar, and a body the transport could not deliver: each writes a `rejected` row and nothing else. No digest exists to key a replay record with, so there is nothing to replay — the caller fixes the transport problem and sends a fresh request.                                                                                                                                                                                                                                                                         |
| 4   | Idempotency intake     | One pre-transaction read of `request_intake` keyed `(account_id, idempotency_key)`, **before alias resolution**: a row whose digest matches is a replay — the original request id and its terminal status are re-answered with `replayed=true`; the original body is not replayed and the request is never re-executed (ADR 0004). A row whose digest differs is an explicit conflict. A row whose original failed `gateway_abandoned` — terminal without an answer, the one record a replay cannot re-serve — answers the spent key.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        | `409 idempotency_conflict` (different digest, no `Retry-After`, no new row — the original request's row answers), `409 request_in_progress` (same digest, original admission still open, with a fixed short `Retry-After` — no row is written), or `409 request_abandoned` (same digest, original abandoned before any answer — no `Retry-After`, the key is spent, the caller mints a fresh one).                                                                                                                                                                                                                   |
| 5   | Model alias resolution | Inside the admission transaction — and therefore after the intake probe, so a no-model body under a spent key answers 409 `idempotency_conflict`, not 400: the body is parsed once here, refusing an unparseable body, a missing `model`, and a model outside the alias-name grammar (a malformed name is 400 `invalid_request` with `model` in `param`, never a 404). Then alias lookup in the runtime's own alias and candidate rows, one consistent candidate snapshot (ADR 0001, rule 3); the alias must be `active`, and a retired one answers 404 exactly as an unknown name does. The request's canonical `max_output_tokens` is validated (present, positive, at most 2147483647, within the alias limit) and input tokens are counted by the canonical tokenizer B11 landed (`catalog/tokenize.go`): the whole request body's byte length, envelope included. The counting that began as the interim counter became canonical without its arithmetic moving a step — tokens are bytes, deliberately — so the hold formula it feeds never changed either. The admission-time price revision is resolved in the same transaction, and the hold is sized: `ceil((T_in·p_in + T_out·p_out) / 1_000_000)` in integer minor units per 1M tokens, one ceiling over the summed raw product. | `400 invalid_request` (an unparseable body, the `model` param, the output bound) or `404 model_not_found` (an unknown or retired alias) — each is recorded as the pair, the `rejected` request row and the intake record born terminal, so the key is spent: a corrected body under the same key answers 409 `idempotency_conflict`, and a retry must mint a fresh key. A missing price is a `500` that writes **nothing**: an unpriced alias is a configuration state the runtime refuses, never a price of zero. A hold above the alias's `reservation_cap` is `400 invalid_request` before any capacity is taken. |
| 6   | Admission decision     | The ADR 0003 waterfall runs against the runtime's **quota projections** at `transaction_timestamp()` — the database's own clock, never a node's. Matching grants are walked in the domain's total order and drawn by conditional update: one forward pass, the eligible rows read once and each taken once. A take that matches no row is a stale number, not a verdict — the walk passes that bucket by. If the hold is not secured when the pass ends, every take is given back unconditionally before the answer is decided. The only retries are whole-unit ones: a lost `(account, key)` unique race, or an engine abort (SQLSTATE `40001`, `40P01`, or a `08xxx` connection class), each of which re-runs the admission unit from its first statement — never a second pass of one walk.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               | Zero eligible capacity at all → every giveback, then `403 no_access`. Capacity present but the whole hold not secured → every giveback, then `429 insufficient_quota` (the reason the schema calls `insufficient_entitlement`, with the PAYG verdict — see [commerce](commerce.md)). Nothing is left drawn behind either refusal: the walk is all-or-nothing against the hold.                                                                                                                                                                                                                                       |
| 7   | Reservation            | Still the same transaction: insert the `Request` shell (`executing`), create the `Reservation` (`open`) priced at the admission revision with its allocation legs in waterfall order, and **insert the intake record LAST** — last so that the unique `(account_id, idempotency_key)` key is the race arbiter, and a racing duplicate loses at the constraint with the whole unit rolled back rather than half-committed. The `hold` legs are **not** written here — they are Control-Plane rows, derived from the committed reservation afterwards, idempotently by `request_id`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           | Transaction aborts atomically — no reservation, no drawdown, no intake row; the client sees a transient error and an idempotent replay re-admits from a clean state.                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| 8   | Candidate selection    | Only now does the request reach the router, which picks the first admissible candidate (candidate order + backend state; [routing](routing.md)). Nothing about a request that did not commit admission has reached this step; the router has no admission decision of its own to make.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       | Refuse `no_candidate` — the reservation is released (compensation, [below](#failure-and-compensation-summary)): the runtime returns the capacity to its projection and appends the `released` fact.                                                                                                                                                                                                                                                                                                                                                                                                                  |
| 9   | Attempt execution      | Adapter translates and calls the backend, enforcing the reservation's output ceiling; each upstream call appends an attempt row (retries included); no DB transaction is open here; the lease is committed state renewed in its own short write while the request runs.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      | Per-attempt failure classes feed step 10; provider-side cost is recorded, never client-billed.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| 10  | Fallback if allowed    | Pre-commitment failure (including `invalid_upstream_response`) → next candidate (repeat 8–10). After commitment: no fallback, ever (ADR 0002).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               | All candidates exhausted → reject `no_candidate_succeeded`; release the reservation.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| 11  | Usage capture          | From the committed attempt, by the delivery-boundary rules B11 settled in one place: **input** is the provider's report clamped down to the count admission priced the hold from — the admitted count itself when no report arrived, because the delivery boundary never touches input (the provider read the whole prompt either way); **output** follows the delivery boundary — delivered tokens for a stream that died or was left after commitment, the provider's report for a completed one bounded by the output basis the hold sized, delivered tokens when a completed answer carries no report, the reserved basis only where neither answers. The capture method attests the priced figures' provenance and nothing else (`reported` / `gateway_observed` / `reservation_floor` — see [accounting](accounting.md)). ADR 0004.                                                                                                                                                                                                                                                                                                                                                                                                                                                    | Unknowable usage still settles conservatively; a fact is never invented and never exceeds the hold — with both figures bounded by the hold's own bases, settled ≤ hold by construction.                                                                                                                                                                                                                                                                                                                                                                                                                              |
| 12  | Settlement             | **Crosses the plane, and is therefore two writes.** The runtime runs the close unit of work B11 landed — the CAS close claims the ending, the committed attempt joins it (probed, then inserted), the request and its replay record finalise, and the `UsageEvent` is appended **last**, its `settled_amount` bound to the fact's own figures: the domain re-derives the hold formula over them and refuses a fact that disagrees with its own arithmetic. Zero is a value — a hold that prices out to zero settles a header of record with no legs, and it is still a settlement; a model with one arm priced at zero books its legs normally, a zero unit price on a consume leg included. The Control Plane then creates the unique `Settlement`, appends the consume legs (split order preserved) and the release legs for the unconsumed tail, and updates its bucket projections — from that fact, idempotently by `request_id`. The runtime's half never waits for the Control Plane's, and the Control Plane's half follows the fact.                                                                                                                                                                                                                                                | Process death before settlement → lease dies, reaper expires the reservation (state `expired`). Settlement after expiry is forbidden; a proven orphaned completion is an `unbillable_orphaned` usage event, never a customer charge — under-accounting is possible, over-billing is not.                                                                                                                                                                                                                                                                                                                             |
| 13  | Response               | Success body / stream (started earlier for streaming — see below) with the request's final status recorded.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  | —                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |

### The wire answers the runtime gives

The status codes above are the runtime's decision; the bodies are a contract in
their own right, and one client's HTTP error is another's machine-readable
refusal. Every refusal the runtime produces is the OpenAI error envelope
(`{"error":{"message","type","param","code"}}`), the same `X-Request-Id` as any
other answer, and one of the values in
`api/openapi/runtime.yaml`'s `RuntimeError` — no internal reason vocabulary
leaks into it.

| Wire answer                                                                                                                                                                                                                                                    | `type`                  | `code`                                  | `param`                                                                | Headers                                                                                 |
| -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------- | --------------------------------------- | ---------------------------------------------------------------------- | --------------------------------------------------------------------------------------- |
| 401 on any authentication cause                                                                                                                                                                                                                                | `authentication_error`  | `invalid_api_key`                       | —                                                                      | `X-Request-Id`                                                                          |
| 403 `account_suspended` / `account_closed`                                                                                                                                                                                                                     | `permission_error`      | `account_suspended` \| `account_closed` | —                                                                      | `X-Request-Id`                                                                          |
| 403 `no_access` (no eligible grant, PAYG off)                                                                                                                                                                                                                  | `permission_error`      | `no_access`                             | —                                                                      | `X-Request-Id`                                                                          |
| 404 unknown or inactive alias                                                                                                                                                                                                                                  | `not_found_error`       | `model_not_found`                       | `model`                                                                | `X-Request-Id`                                                                          |
| 400 malformed key / body / output bound / hold over cap                                                                                                                                                                                                        | `invalid_request_error` | `invalid_request`                       | `idempotency_key` \| `max_tokens` \| `max_completion_tokens` \| `null` | `X-Request-Id`                                                                          |
| 400 `provider_rejected_request` — an upstream refused the request before any content                                                                                                                                                                           | `invalid_request_error` | `provider_rejected_request`             | — (null)                                                               | `X-Request-Id`                                                                          |
| 400 `context_too_large` — the upstream model cannot accept the request's context                                                                                                                                                                               | `invalid_request_error` | `context_too_large`                     | — (null)                                                               | `X-Request-Id`                                                                          |
| 500 `upstream_authentication` — the gateway's own upstream credential was refused                                                                                                                                                                              | `api_error`             | `upstream_authentication`               | — (null)                                                               | `X-Request-Id`                                                                          |
| 409 different digest on the same `(account, key)`                                                                                                                                                                                                              | `invalid_request_error` | `idempotency_conflict`                  | —                                                                      | `X-Request-Id`                                                                          |
| 409 same digest, original admission still open                                                                                                                                                                                                                 | `invalid_request_error` | `request_in_progress`                   | —                                                                      | `X-Request-Id`, fixed `Retry-After`                                                     |
| 409 same digest, original abandoned before any answer existed (the key is spent)                                                                                                                                                                               | `invalid_request_error` | `request_abandoned`                     | —                                                                      | `X-Request-Id`                                                                          |
| 429 capacity present but the whole hold not secured                                                                                                                                                                                                            | `insufficient_quota`    | `insufficient_quota`                    | —                                                                      | `X-Request-Id`                                                                          |
| 500 no price, or a persistence failure                                                                                                                                                                                                                         | `api_error`             | —                                       | —                                                                      | `X-Request-Id`                                                                          |
| 503 no admissible candidate at the handoff, every candidate tried and failed to serve (the release of step 8 or 10 has already run), or a hold reclaimed mid-flight by the lease reaper before commitment (the release's CAS loses to whoever closed the hold) | `overloaded_error`      | — (null)                                | —                                                                      | `X-Request-Id`, fixed `Retry-After`                                                     |
| 503 the same answer replayed from the record on a repeat of the same key and body                                                                                                                                                                              | `overloaded_error`      | — (null)                                | —                                                                      | `X-Request-Id`, `Idempotent-Replay: true`, `X-Original-Request-Id`, fixed `Retry-After` |

The three surfaced refusals are the pre-commitment endings whose answers are
ordinary HTTP: a candidate's provider named the fault before any content was
committed, the walk stopped there, and the hold was returned whole. A replay of
a request that ended in one of them re-answers byte-for-byte with the same
refusal, under the replay headers. The sentences never echo what the provider
said — a provider's own error text is its content, and content is exactly what
this surface does not relay; the class and the code are the whole answer.

The distinction `no_access` and `insufficient_quota` make is a real one and not a
restatement: `no_access` says the account has no capacity that may serve this
alias at all, and `insufficient_quota` says it has some and the request's hold
did not fit in it. The hold's own overflow or cap overflow is neither — it is
`invalid_request`, because it is a property of the request, not of the balance.

The vocabulary this table uses is landed code, not a glossary to be honoured
later: the terminal statuses and every rejection and failure reason are values
of `migrations/dataplane/000003_runtime_storage`'s CHECK constraints — widened
by `000007_routing_failure_vocabulary` with the three surfaced upstream
refusals (`provider_rejected_request`, `context_too_large`,
`upstream_authentication`), the pre-commitment endings that name no attempt —
and of the runtime's `execution` domain
(`apps/dataplane/internal/domain/execution`), which refuses in Go what the
database would refuse again in SQL. Three details in the
table are worth reading precisely because the landed schema fixes their shape.
The rows of step 4's replay decision live in `request_intake`, keyed
`(account_id, idempotency_key)` — the database's unique key on that pair is the
final idempotency guard, whatever admission checked first, and step 7's
insert-last discipline is what makes the constraint the arbiter of a race
rather than a surprise. Steps 8 and 10–12 are the routing stage's ending units,
and they run: the release (the compensation of step 8 and of an exhausted
step 10) and the settle (steps 11–12, delivery counted by the canonical byte
rule B11 landed) are whole transactions of the serving path, with the attempt
row probed then appended inside the settle unit and the usage fact appended
**last** — so "settled" and "in the feed" are one fact, not two events to
reconcile. Step 9's executor runs too
([ADR 0009](../adr/0009-provider-adapters-and-egress.md)): the routing
stage's registry is a snapshot of the catalog's callable backends, built by
the composition root and refreshed on a timer, and an admitted request whose
alias has a candidate on a callable backend reaches a real provider — one
upstream call per attempt, typed result back, the reply's sink as the
commitment point. The attempt rows of step 9 are appended
**as each upstream call finishes, never while it is in flight**, so no
transaction is ever open across a provider call and a crash mid-call leaves no
row at all; the one sanctioned later write to an attempt row is the
provider-usage report, and the report adapter that landed carries a COALESCE
discipline that never displaces a figure already recorded. That
ending composition is what the store's integration
suite proves against real PostgreSQL
(`TestIntegrationSettlementUnitIsAllOrNothing`).

### Streaming places step 13 earlier — the lifecycle does not change

For a streamed response, content begins flowing to the client between steps 9
and 10 (that moment **is** commitment; pre-content lifecycle frames are
buffered, [routing](routing.md)). The remaining semantics hold unchanged:

- commitment freezes the candidate (no fallback from step 10 on);
- the executor renews the reservation's **execution lease** for as long as
  the stream runs, so the hold cannot be expired under a live request;
- steps 11–12 run when the stream completes (or fails) — **asynchronous to
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

### The ending, end to end

Steps 11–12 are the **usage close**, and what it is reads best as one arc —
the flow B11 exists to make continuous, whatever the outcome it closes:

```text
B10 Provider/Egress
        ↓
execution outcome           the walk's typed result, commitment drawn where it belongs
        ↓
B11 Usage Close             one Data-Plane unit of work — the settle, the release, or the reaper's expiry
        ↓
final Request/Attempt state
        ↓
immutable Usage Fact        appended LAST — "terminal" and "in the feed" are one fact
        ↓
B12 Settlement              the Control Plane derives it from the fact — landed on the far side of the seam (ADR 0010)
```

Each stage's contract, in the vocabulary the rest of this page already uses:

- **Terminal states.** A request ends in exactly one of `succeeded`,
  `failed_after_commitment`, `rejected` (with its reason), or the
  `gateway_abandoned` failure — and its replay record takes the matching
  terminal pointer in the same unit of work. An attempt ends in `succeeded`,
  `failed`, or `failed_after_commitment`; only the committed attempt is
  named by the ending's fact.
- **Commitment.** The answer's first forwarded content freezes the candidate
  and the ending's shape ([routing](routing.md)). A stream commits at its
  first content-bearing chunk — or at the flush of a preamble buffer grown
  to its cap, a cap-sized preamble being content for commitment purposes —
  and a buffered answer commits when its completed body first reaches the
  client. Before commitment, failure means fallback or a whole release;
  after it, failure is delivered inside the answer and settles on what was
  delivered.
- **Partial stream.** A stream that dies — or a client that leaves — after
  commitment settles on the delivered bytes (capture `gateway_observed`);
  the provider's usage for the unread remainder is attempt telemetry only
  (ADR 0004).
- **Usage availability.** Billable usage becomes visible outside the runtime
  at exactly one instant: the commit of the close's fact. The wire answer
  never waits for it, and no consumer can see it before that commit — the
  fact feed's ordering is commit ordering
  ([data implications](data-implications.md)).
- **Idempotency.** The ending is claimed by the reservation close's
  compare-and-set, so exactly one unit appends the settlement-relevant fact;
  the committed attempt joins by probe-then-insert; the replay record's
  terminal pointer is once-only; and the feed's dedup is kind-classed — a
  settlement fact and an orphan fact of one request are different facts,
  each exactly once.
- **Reservation close.** `settled`, `released`, `expired` all pass through
  the same state-guarded close, all with the fact appended last — and a
  close whose request row or replay record would not take the ending rolls
  the whole unit back, hold still open, for a whole retry or the reaper
  ([accounting](accounting.md)).
- **Crash recovery.** The close runs detached from the caller's context with
  three whole-unit retries; a crash rolls it back whole — never a terminal
  row without its fact. A hold whose process died is the reaper's `expired`
  close; a settle that loses a close whose telemetry it already earned does
  not walk away — it records the `unbillable_orphaned` orphan tail.
- **Replay.** Both kinds are read-only answers: a client replay re-answers
  the original outcome from the intake record (step 4), and a feed replay
  re-delivers a fact the Control Plane has already applied — a no-op keyed
  by the fact's `(request_id, kind class)`, the same key the applier settles
  on ([accounting](accounting.md); [ADR 0010](../adr/0010-usage-fact-ingestion-and-settlement.md)).
- **Attribution.** The single committed attempt is the billing subject; its
  figures are clamped to the bases the hold was priced from; failed attempts
  bill nothing and carry provider-side cost as telemetry only.
- **Accounting boundary.** The close writes Data-Plane rows only: no
  Control-Plane database call, no ledger write, no funding-bucket read
  stands inside its transaction ([accounting](accounting.md)). The money
  half of the arc is another plane's derivation from the fact.

And the boundary's negative space, stated once because every conversation
about the close eventually reaches for one of these words:

> **The usage close is not billing** — nothing is owed, sent or collected;
> the fact states usage, and the ledger prices it later. **It is not
> settlement** — the `Settlement` of record is the Control Plane's
> derivation from the fact, the stage of the arc above the B12 consumer
> performs ([ADR 0010](../adr/0010-usage-fact-ingestion-and-settlement.md)).
> **It is not the
> ledger** — the runtime writes no ledger row at all. **And it is not
> pricing** — the snapshot it prices with was copied at admission; the close
> computes an amount with it and never selects, revises or re-quotes a
> price.

### Failure and compensation summary

- Rejection from account identification onward but before reservation → the
  request row is written with `rejected` status and its reason. A
  `401` request writes nothing — there is no account to attach a
  row to. An `idempotency_conflict` writes no new row either; the original
  request's record answers.
- Failure after reservation but before any attempt could serve (steps 8–10)
  → **release** the reservation: the runtime returns the capacity to its
  projection, records the terminal state (`released`) and appends the
  `released` usage fact in the same unit of work — the fact the Control Plane
  derives its release legs from. The return is unconditional once the close's
  compare-and-set has won it: running it outside the winning close would mint
  capacity, never give one back.
- Process crash mid-flight → lease dies; the reservation **expires** via the
  reaper, on the same terms. A completion proven orphaned afterwards is an
  `unbillable_orphaned` usage event — under-accounting is possible,
  over-billing is not.
- Success → **settle** (consume per bucket, release the tail). Exactly once,
  by the settlement's uniqueness constraint — which is a Control-Plane
  constraint, and the reason a redelivered usage fact costs nothing.

## What admission does not promise

The steps above are commitments the landed pieces are already shaped for;
there are things a reader might infer from them that the design deliberately
does not claim, and naming them is cheaper than letting a future change
assume them silently.

- **The quota projection is not promised fresh.** A projection row is as new
  as its last delivery, and admission makes no claim about how long ago that
  was: a grant that has not yet arrived reads as zero capacity, a grant that
  has been revoked but not yet converged may still read as available. The
  runtime enforces the ceiling it can see; the ledger, not the projection, is
  the source of truth for money. No staleness bound is promised — not a number,
  not a "recently".
- **There is no epoch fence between a projection row's generations.** A
  redelivered or stale publication updates the Control-Plane-owned columns and
  never resurrects spent capacity, and a refill carries its own identity — but
  a projection row that survived a Control-Plane-side re-keying is matched by
  its funding-bucket identity alone. If that identity is ever re-minted for
  the same logical bucket, the runtime treats it as a different row. Nothing
  in the landed schema or the admission path detects or prevents that; it is
  a re-keying discipline, not a runtime guarantee.
- **The reaper is not on the synchronous path, and nothing here promises a
  request safety from it.** An executing request keeps its hold alive by
  renewing its lease, and the bound that protects it is the one the process
  validates at start: the execution duration sits strictly below the
  reservation hold window — the horizon the reaper sweeps against, not the
  lease, which renewal keeps alive for as long as the call runs. A lease
  that is not renewed dies, and the reaper expires the reservation on
  the same terms as any other dead lease; no step above claims otherwise.
- **The seed producer does not exist yet.** The quota projection's rows are
  consumed by admission, but the Control-Plane side that publishes them — the
  cycle-roll delivery of [commerce](commerce.md)'s projection seed contract,
  and the PAYG balance seeding behind it — is an unlanded seam. A row in
  `quota_projections` today is one a test or an operator put there, not one a
  landed producer guarantees; nothing above describes a publisher that runs.
- **The credential mirror is a projection too.** The same caveats as the
  quota projection apply one step earlier in the path: a key revoked or an
  account suspended on the Control Plane keeps authenticating until the
  mirror converges, and no bound is promised on how long that takes. The
  mirror is what makes authentication survivable with the Control Plane down
  (step 1); the price of that independence is exactly this window, and it is
  the Control Plane's reconciliation, not the runtime, that closes it.
- **No admitted request is promised an answer.** A request that committed
  admission has secured its hold, not its response: step 8 can find no
  admissible candidate and answer 503 with the hold fully returned, step 10
  can exhaust every candidate, and a mid-stream death settles on what was
  delivered. Admission is a capacity contract with the ledger, never a
  success contract with the provider.

## Post-request fact delivery — a separate path

The runtime's journey ends at step 13. What happens to the usage fact afterwards
is a different path with a different schedule, and it is not a step 14: nothing
on the list above waits for it, and a Control Plane that is down when the fact
commits changes nothing about the request that produced it.

```text
UsageEvent committed durably (step 12)
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
  A page is acknowledged only after its required envelope and event fields have
  been validated — a malformed page is rejected before anything is applied and
  before the position moves, so the same range is read again rather than
  crossed.
- **Redelivery is the norm, not an error.** There is no acknowledgement, no
  consume-and-delete and no completion signal: the same page may be read any
  number of times, and applying a fact twice is a no-op because `request_id` is
  the idempotency key. That is what makes a retry free and a crash between
  applying and advancing safe.
- **The client never sees any of it.** The response ended at step 13. The
  Control Plane's settlement is a second write, and it is invisible from the
  outside: the console reads the ledger, and the runtime does not wait for it.

The protocol, its ordering guarantee and its failure model are
[cross-plane protocols](cross-plane-protocols.md); the layering that carries it
is [ports and adapters](ports.md). The delivery model is ADR 0006 §5, and it is
why step 12 could be split across the plane boundary without the request path
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
  spoken today ([above](#post-request-fact-delivery--a-separate-path)), and
  the management half now has its first spoken operation: the alias-group
  current-version read, called through the `CatalogReader` port by the
  commerce due-work lanes to resolve grant-definition scopes before their
  units of work open ([commerce](commerce.md)). The interactive operations
  the port is shaped for still have no use case behind it.
- **It runs at a different tempo.** This path is human-paced and low-volume;
  the runtime's is latency-bound and the product's availability. Neither
  shares a process, a deploy or a database with the other.

There is deliberately no normative step list for this path yet. The commerce
domain's application half is built — the plan, subscription and PAYG use cases
with their persistence ([commerce](commerce.md)) — but no transport serves
them: the console client calls none of them yet, and sign-in and configuration
are not built at all. A numbered lifecycle for the path would still be a
design the ADRs have not made. What is decided is that the path exists, that
it is disjoint from the runtime's, and that its only way to reach the Data
Plane is the management port.

## Asynchronous analytics — a separate path

Everything the business wants to know later — per-candidate latency, retry
counts, error-class frequencies, provider-side cost of failed attempts,
capture-method ratios — is derived from the event tables
(`requests`, `request_attempts`, `usage_events`) and the append-only ledger,
in queries/jobs that:

- never write to the relational working set (balances are never "fixed up" by
  a job — corrections are compensating ledger entries, invariant 2);
- never sit in the synchronous path's latency budget;
- read rollups over the event tables when volume demands — continuous
  aggregates were ADR 0005's vehicle here, and the B7 amendment it carries
  gives them up until an equivalent is built;
- read either database from one side of the boundary, never both in one
  statement. Operational analytics ("which candidate was slow?") reads the
  event tables, which are the Data Plane's; financial analytics reads the
  ledger, which is the Control Plane's; an analysis that needs both joins the
  facts by `request_id` outside the serving path, not by crossing a boundary
  inside one.

There is no second metering pipeline: the usage event written at step 12 is
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
