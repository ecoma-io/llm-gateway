# ADR 0004: Accounting — reserve, execute, settle

- Status: Accepted
- Date: 2026-09-23
- Issue: [#5](https://github.com/ecoma-io/llm-gateway/issues/5)

## Context

The gateway has to hold finite customer funding before it calls a provider,
then account exactly once for what the customer was allowed to receive. The
model must survive concurrency, fallback, streaming, provider errors, process
death, price changes, and reservations split across several entitlement/PAYG
buckets. "A signed amount on an append-only ledger" alone is not a model: it
does not say whether a hold reduces settled balance, available balance, or
both, and it fails as soon as one settlement consumes two buckets.

## Decision

### Reservation, settlement, usage event, and ledger legs

```text
admission (one short DB transaction)        execution (no DB transaction)
────────────────────────────────────        ──────────────────────────
create Request shell + intake record         append RequestAttempt per upstream call
create Reservation + allocation legs         adapter enforces reservation token ceiling
append hold ledger legs                      router may fallback only before commitment
renew execution lease

settlement (one short DB transaction)
───────────────────────────────────
create the unique Settlement(request)
append UsageEvent
append one consume/release ledger leg per allocated bucket
close Reservation as settled
finalise the Request
```

- **RequestIntake** is the relational replay record: unique
  `(account_id, idempotency_key)`, the canonical request **digest**, the
  request ID, and a terminal status pointer. It is the only idempotency
  authority; event-family tables are never read for business decisions
  (ADR 0005). It is created in the admission transaction and immutable
  afterwards.
- **Reservation** is the pre-execution hold: `request_id`, immutable price
  revision, canonical input/max-output bounds, `reserved_amount`,
  `expires_at`, renewable execution lease, and **allocation legs** (one
  `(reservation, funding_bucket)` row per entitlement/PAYG bucket). State is
  `open → settled | released | expired`. A request has **exactly one
  reservation, ever** — uniqueness on `request_id` is total, not filtered by
  state — and only an `open` one can execute. The reservation references the
  request; the request row carries no reservation pointer.
- **Settlement** is an accounting header, unique by `request_id`; it is the
  idempotency boundary, not a vague verb. It carries `request_id`, a
  `settled_total` equal to the sum of its consume legs (written once), and
  `created_at`; nothing on it is ever mutated. It is created in the same
  transaction as its `UsageEvent` and ledger legs. A second finalizer hits
  the unique `settlements.request_id` constraint and reads the first result
  instead of charging again.
- **UsageEvent** is one immutable actual-usage fact per settlement:
  `(request, committed attempt, normalized input/output token counts, price
revision snapshot, capture method)`. Capture method is `reported` when the
  provider's terminal usage is trustworthy, `gateway_observed` when the
  gateway counts forwarded content, and `reservation_floor` only when a
  committed attempt ends before any reliable count. A proven post-expiry
  completion is written as `unbillable_orphaned` — a usage fact with no
  customer charge (below). Corrections are new events referencing the prior
  event; the original is never edited/deleted.
- **LedgerEntry** is a single immutable **bucket leg**, never an overloaded
  header. It carries `settlement_id` or `reservation_id` as applicable,
  `funding_bucket_id`, kind, positive `amount`, and a per-bucket `sequence`
  allocated by incrementing a counter on the bucket row inside the same
  transaction — each bucket's history is totally ordered without a global
  sequence bottleneck. The price snapshot (`revision_id` + copied unit
  prices) is **required on `consume` legs and usage events** and null on
  `hold`/`release`/`grant`/`topup`/`adjustment` legs, which move money
  without consuming tokens. A reservation/settlement may have many legs. A
  unique `(settlement_id, funding_bucket_id, kind)` permits a split
  settlement while preventing duplicate movement in a given bucket.
- **FundingBucket** is an authoritative, lockable capacity projection for
  exactly one entitlement cycle or one account PAYG balance. It carries a
  version and cached `settled_amount`, `held_amount`, and `available_amount`.
  The cache is maintained in the same transaction as ledger legs and is
  verified/rebuildable from those legs; it is a concurrency control projection,
  not an independently editable balance. Conditional updates such as
  `available_amount >= take` protect both entitlement and PAYG admission.

### Formal balance projections

For a funding bucket, let `G` be all `grant` and `topup` amounts, `C` normal
`consume` amounts, `H` `hold` amounts, `R` `release` amounts, and `A` the
`(settled_delta, held_delta)` pairs of `adjustment` legs. The projections are
formal, not illustrative:

```text
settled   = ΣG − ΣC + ΣA.settled_delta
held      = ΣH − ΣR − ΣC + ΣA.held_delta
available = settled − held
```

A valid normal ledger history — no adjustments — maintains `held ≥ 0` and
`available ≥ 0`, and the non-adjustment kinds can never make `settled`
negative because `ΣG ≥ ΣC` is enforced by the same bucket guards that admit
consumption. A negative `settled` is therefore reachable **only** through an
operator adjustment, which is exactly why the algebra separates them. The
transactional FundingBucket cache must equal these formulas after every
write. Thus a grant of 100, hold of 12, consume of 7, and release of 5 gives:

```text
settled   = 100 − 7 = 93
held      = 12 − 5 − 7 = 0
available = 93 − 0 = 93
```

`adjustment` is an explicit operator-authorised compensating leg with a
reason, an original-entry reference, and stated `settled_delta`/`held_delta`
— e.g. a goodwill credit appends `settled_delta = +5`; repairing a mistaken
charge appends `settled_delta = −7` against the original settlement. It must
preserve the same projection identities and is the only negative-settled
path; it is not an automatic debt/overdraw escape hatch.

### Admission and hard reservation bound

The client contract requires canonical `max_output_tokens`; ADR 0003 defines
validation and its alias-exact price snapshot. Admission inserts the request
shell and its intake record and, atomically:

1. creates the Reservation;
2. conditionally reserves FundingBucket capacity according to ADR 0003's
   complete waterfall;
3. inserts allocation rows and one `hold` LedgerEntry per allocation;
4. updates the corresponding bucket projections; and
5. begins the execution lease.

No provider call is inside that transaction. The adapter enforces the canonical
output ceiling, so client-billable actual usage is at most the reservation.
Actual usage often differs from the reservation by being lower; settlement
returns the unused portion. Provider-reported use beyond the enforced limit is
an upstream cost anomaly, not customer debt.

### Settlement of split holds

Settlement consumes reservation allocations in their stored waterfall order
(the allocation rows retain `ordinal`), then releases the unconsumed tail.
For a reservation S1: 20 + S2: 40 that settles at 30:

```text
Settlement S:
  consume leg  S1 20
  consume leg  S2 10
  release leg  S2 30
```

The three legs share `Settlement S`; its unique `request_id` enforces exactly
one settlement, while the per-bucket-leg unique constraint permits the required
split. There is no excess allocation path: the reservation ceiling makes it
unnecessary and rejects the unsecured-overdraw bug by construction.

### Commitment and billable delivery

The billing subject is the one `committed_attempt_id` on Request. Commitment
is defined by ADR 0002: first **content-bearing** byte forwarded (text or
streamed tool arguments; pre-content lifecycle frames are buffered).

- A pre-commitment failure gets no Settlement and releases the whole
  Reservation once candidates are exhausted. Failed attempts still exist as
  provider-cost telemetry; they do not bill the client.
- A committed attempt can never fallback. Its UsageEvent charges the
  **customer-visible delivery boundary**: content the gateway successfully
  forwarded before it observed the client disconnect or stream failure. The
  gateway cancels upstream on client disconnect where supported. Provider
  usage received later is recorded as provider-cost telemetry, not silently
  substituted as customer usage.
- If exact forwarded tokens are unavailable, `gateway_observed` counts gateway
  received/forwarded deltas; only when that too is unavailable does
  `reservation_floor` charge the conservative already-held amount. The
  capture method makes that confidence visible in data.
- An HTTP 200 followed by an SSE error payload is a failed stream, not success.
  Empty content, malformed non-streaming JSON, invalid framing, or EOF before
  commitment is a typed `invalid_upstream_response` failure; it may fallback
  according to ADR 0002 and cannot become a billing subject.

### Expiry, leases, and crashes

The live executor renews a reservation's execution lease; `expires_at` is
strictly longer than the maximum accepted request duration and a reaper expires
only `open` reservations with a dead/expired lease. The reaper writes release
legs and changes state to `expired` atomically.

After an expiry, customer settlement is intentionally **forbidden**: the
reservation's capacity has been released for future customers, so consuming it
later could double-spend it. When the **reaper** expires a reservation it
inspects the attempt telemetry; if it already proves an orphaned provider
completion, the reaper writes an immutable `UsageEvent` classified
`unbillable_orphaned` **in the same transaction as its release legs**. Proof
that only surfaces afterwards is appended by reconciliation on its own —
appending an unbillable fact late is safe; settling late is not. The orphaned
event exists for operations and provider-cost reconciliation; it carries no
customer `Settlement` and no consume leg. This is an explicit gateway loss /
under-accounting decision, never a hidden debt. A live stream should not enter
this path because its lease is renewed.

### Accounting invariants

1. **Usage history is immutable.** UsageEvent rows are insert-only; corrections
   are new linked facts.
2. **Ledger history is append-only.** LedgerEntry rows are insert-only;
   reversals are compensating legs, never updates/deletes.
3. **Entitlements cannot be double-spent.** Every FundingBucket allocation is
   a conditional, lock/version-guarded update inside one of the four
   coordinated transactions (ADR 0001, rule 6); no code path changes capacity
   outside them.
4. **Reservation and settlement are distinct.** Hold and consume are different
   ledger legs; one unique Settlement per request makes settlement exactly-once.
5. **Logical requests and provider attempts are distinct.** One Request owns
   many attempts (including retry calls); attempts never create customer ledger
   legs directly.
6. **Fallback cannot double-bill.** One Reservation persists across candidates;
   only the single committed attempt can create its request's Settlement.
7. **Streams cannot cross-provider fallback after commitment.** Commitment is
   the gate; a stream failure after it stays with that one attempt.
8. **Actual usage may differ from reservation.** It may be lower and releases
   the tail; it cannot be billably higher because the enforced canonical limit
   makes the reservation a hard cap.
9. **PAYG is a funding source, not a subscription.** Its bucket has no plan,
   cycle, or renewal and is funded/consumed by legs only.
10. **Pricing changes never rewrite history.** Reservation, UsageEvent, and
    LedgerEntry carry the admission PriceList revision and copied unit prices;
    an active alias missing a price is a configuration write failure, never a
    zero-price settlement.

### Intake and idempotency

The gateway receives a client-supplied idempotency key required for every
execution request; its uniqueness scope is `(account_id, idempotency_key)` and
is permanent, enforced by the relational `RequestIntake` record (above).
Retrying the same key with a different canonical request digest is rejected
(`idempotency_conflict`, no new row); retrying with the same digest returns
**outcome metadata** — the original request ID, its terminal status, and for
a settled request its usage facts — and never executes again. The response
body itself is not replayed; whether stored completions can be re-served as
bodies is an open question ([overview](../architecture/overview.md)).
Request ID is the internal key for Reservation, Settlement, UsageEvent, and
attempts. This is permanent rather than a 24-hour dedup window because billing
records are retained.

### Corrections move money only by compensation

A usage correction is a new `UsageEvent` referencing the original event,
**and** it appends compensating `adjustment` legs that reference the original
settlement. Neither the usage history nor the ledger is edited; balances move
because compensating legs move them (invariants 1–2).

## Consequences

- The schema needs `settlements`, `funding_buckets`, the `request_intake`
  replay table, and bucket-leg ledger entries in addition to the named
  top-level concepts; those are required to make split settlement, PAYG
  concurrency, and permanent idempotency enforceable, not implementation
  detail.
- Amounts are integer minor units in the single platform-wide settlement
  currency (ADR 0003); no per-row currency column exists.
- Analytics can reconcile bucket caches against append-only legs, while hot
  admission uses a short conditional update rather than summing a ledger under
  contention.
- Customer credit exposure is bounded by the reservation, even during streams
  and fallback. A provider can cost the gateway more than a capped client
  request, but it cannot create unbounded customer debt.

## Alternatives considered

- **Post-paid only**: rejected — cannot atomically reject insufficient
  concurrent spend.
- **Mutable balance as the only truth**: rejected — no audit/replay path.
- **One `(request, consume)` ledger row**: rejected — cannot represent split
  buckets; Settlement header + legs are necessary.
- **Soft holds with automatic PAYG debt at settlement**: rejected — concurrent
  streams turn prepaid service into unbounded unsecured credit.
- **Late charge after released/expired hold**: rejected — the released capacity
  may already be reserved by a new request; charging it is double-spend.
