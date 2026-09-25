# ADR 0004: Accounting — reserve, execute, settle

- Status: Accepted
- Date: 2026-09-23
- Issue: [#5](https://github.com/ecoma-io/llm-gateway/issues/5)
- Amended by: [ADR 0006](0006-control-plane-and-data-plane.md) — the transactions below are plane-scoped, and settlement is split across the plane boundary
- Amended by: B6, the accounting foundation (2026-09-25) — the guards this record states as discipline are schema CHECKs, indexes and triggers now, and there is no negative-settled path any more (see the amendment under “Formal balance projections”)

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

> **Amended by [ADR 0006](0006-control-plane-and-data-plane.md): the blocks are
> plane-scoped, and settlement is no longer one transaction.** The rows above do
> not all live in one database, so no transaction can write them all:
>
> | Record                                                                                            | Database                        |
> | ------------------------------------------------------------------------------------------------- | ------------------------------- |
> | `Settlement`, `LedgerEntry`, `FundingBucket`                                                      | `control` — the Control Plane's |
> | `RequestIntake`, `Request`, `RequestAttempt`, `UsageEvent`, `Reservation` and its allocation legs | `dataplane` — the Data Plane's  |
>
> Every one of these rows is written by the transaction that the diagram
> attributes it to, and that transaction is now plane-local — which is what
> decides the database, not the shape of the row. Admission is therefore
> **entirely the Data Plane's**: one transaction writing the shell, the intake
> record, the reservation with its allocation legs and the execution lease,
> against the runtime's own capacity projection. The hold legs are written in
> the Control Plane, from the reservation as a fact — see the note under
> admission below. Settlement splits along the table: the runtime writes the usage
> fact and closes its reservation locally, and the Control Plane writes the
> `Settlement`, its ledger legs and its bucket projections from that fact,
> idempotently by `request_id`. Everything this ADR says about exactly-once
> settlement still holds — the uniqueness is on `settlements.request_id`, and
> one request still yields one settlement — but the atomicity between the
> charge and the fact it derives from is gone, replaced by a fact that is the
> authority the charge derives from.

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
  `created_at`; nothing on it is ever mutated. It is written in the Control
  Plane in the same transaction as its ledger legs, from the usage fact the
  Data Plane recorded (amended by ADR 0006; see the table above). A second
  finalizer hits the unique `settlements.request_id` constraint and reads the
  first result instead of charging again.
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
  exactly one entitlement cycle or one account PAYG balance. It is an
  **Accounting aggregate** (ADR 0001): every write to the row happens inside
  the accounting flows this ADR defines — admission, settlement, release,
  the cycle roll's bucket creation, and the topup/adjustment refills — and
  the Commerce entitlement cycle or PAYG flag it projects references it by
  identifier, never the reverse. It carries a version and cached
  `settled_amount`, `held_amount`, and `available_amount`.
  The cache is maintained in the same transaction as ledger legs and is
  verified/rebuildable from those legs; it is a concurrency control projection,
  not an independently editable balance. Conditional updates such as
  `available_amount >= take` protect both entitlement and PAYG admission.

> **Amended by [ADR 0006](0006-control-plane-and-data-plane.md): this one
> bucket becomes two rows in two planes.** Within the **Control Plane**, a
> funding bucket's cached settled/held/available values are still maintained in
> the same transaction as its ledger legs and remain rebuildable from them, and
> everything above about the formal projections, the guards and the adjustment
> algebra still describes that row exactly. Separately, the **Data Plane** holds
> a _quota projection_ — the lockable capacity row the runtime's admission
> conditionally updates in the same transaction as its reservation and
> allocation legs. The projection is not a balance: it is the enforcement
> ceiling for one entitlement cycle or PAYG balance, seeded from Control-Plane
> grants and updated only by the runtime. Settlement of record happens in the
> Control Plane from the runtime's usage facts. The projection converges to the
> ledger by reconciliation, and **the ledger, never the projection, is the
> source of truth for money**. The projection exists because the runtime must
> be able to refuse an over-budget request with the Control Plane switched off;
> it is the price of that, paid in a convergence step that did not previously
> exist.

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

> **Amended by B6 (the accounting foundation, 2026-09-25): there is no
> negative-settled path any more — the no-credit rule is structural.** The
> two claims above — that a negative `settled` is reachable through an
> adjustment, and that the adjustment is therefore "the only negative-settled
> path" — were written when that freedom was discipline the operator was
> trusted to keep. The landed schema states it as arithmetic instead:
> `funding_buckets_balance_projection` CHECKs `available = settled − held`
> with `held ≥ 0` and `available ≥ 0`, which makes `settled ≥ 0` transitive,
> and the guarded echo that lands an adjustment re-states the same
> predicates in its WHERE clause — a stated delta that would drive settled,
> held or available below zero updates zero rows and is refused in the
> domain's words before that. The examples stand inside that bound: a
> goodwill credit (`+5`) still appends; repairing a mistaken `−7` charge
> still appends while settled covers it, and is refused against a settled
> balance of 5. Corrections fix records; they never create debt.

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

> **Amended by [ADR 0006](0006-control-plane-and-data-plane.md): three of those
> five steps stay in the admission transaction and two move.** Steps 3 and 4
> write Control-Plane rows, so they cannot be in a Data-Plane transaction. What
> the runtime does atomically at admission is: create the request shell and the
> intake record, create the reservation and its allocation legs, conditionally
> draw the allocation down from its **quota projection**, and begin the lease.
> The `hold` legs and the bucket projections of steps 3 and 4 are written in the
> Control Plane from the reservation as a fact, idempotently by `request_id`.
> The waterfall order and the guards are unchanged; what changes is that the
> runtime enforces them against a row it owns, and the ledger records them
> afterwards. That is deliberate: the runtime must not hold ledger write
> authority (ADR 0006, section 2), and the alternative — the runtime writing
> money from the hot path — is the blast radius the split exists to prevent.

No provider call is inside that transaction (ADR 0001, rule 7). The adapter
enforces the canonical output ceiling, so client-billable actual usage is at
most the reservation.
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
only `open` reservations with a dead/expired lease. The reaper returns the
capacity to the runtime's projection and changes state to `expired` atomically;
its release legs are written in the Control Plane from that state change
(ADR 0006 — the reaper is the runtime, and the ledger is not its to write).

After an expiry, customer settlement is intentionally **forbidden**: the
reservation's capacity has been released for future customers, so consuming it
later could double-spend it. When the **reaper** expires a reservation it
inspects the attempt telemetry; if it already proves an orphaned provider
completion, the reaper writes an immutable `UsageEvent` classified
`unbillable_orphaned` **in the same transaction as its release legs**.

> **Amended by [ADR 0006](0006-control-plane-and-data-plane.md): the reaper is
> the runtime, so those two rows are in different databases.** The reaper
> writes the `unbillable_orphaned` usage event and closes the reservation in
> one Data-Plane transaction; the Control Plane writes the release legs from
> that fact, idempotently. What the sentence was protecting survives — the fact
> and the release cannot disagree, because the release is derived from the
> fact — and it is now the ordering this ADR uses everywhere else: the fact is
> written where it was observed, and the ledger follows it.

Proof that only surfaces afterwards is appended by reconciliation on its own —
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
3. **Entitlements cannot be double-spent.** Every capacity drawdown is a
   conditional, lock/version-guarded FundingBucket update
   (`available >= take`) inside one of the four cross-context coordinated
   transactions (ADR 0001, rule 6, plane-scoped by ADR 0006); refills are
   ledger-backed writes outside the admission path — the grant-cycle roll, and
   the topup/adjustment flows, each with its own ledger legs and uniqueness
   guards. Under the plane split the invariant is held twice over rather than
   once: the runtime's admission keeps the same guard on its quota projection,
   in the same transaction as its reservation, while the Control Plane's bucket
   guard is unchanged. The runtime's guard is the load-bearing one — it is the
   only thing standing between a request and an unpriced provider call when the
   Control Plane is unreachable, which is precisely why the projection exists.
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
- **The guards are structural, and B6 is what made that literal.** Every rule
  this ADR states as a discipline is now also a statement the database itself
  refuses to violate: the balance-projection `CHECK` (whose `available = settled
− held` with both non-negativity predicates makes non-negative `settled`
  transitive), the owner exclusivity, the leg-algebra and reference-shape
  `CHECK`s, the price-provenance `CHECK`, the partial unique indexes that make
  command keys, reservation movements and settlement movements
  exactly-once, the total order on `(bucket, sequence)`, and the append-only
  and write-once triggers. The adapter's guarded statements re-state the same
  predicates in their WHERE clauses and classify the verdicts into the domain's
  sentinels, so a guard-miss reads as a refusal rather than a surprise — but
  the schema is what makes the refusal true for any writer, including one that
  arrives with a hand-written statement.
- Amounts are integer minor units in the single platform-wide settlement
  currency (ADR 0003); no per-row currency column exists.
- Analytics can reconcile bucket caches against append-only legs, while hot
  admission uses a short conditional update rather than summing a ledger under
  contention.
- Customer credit exposure is bounded by the reservation, even during streams
  and fallback. A provider can cost the gateway more than a capped client
  request, but it cannot create unbounded customer debt. The bound is now
  enforced by the runtime's own projection rather than by a lock on an
  Accounting row, so the guarantee survives the Control Plane being down — and
  is bought with the reconciliation step named in ADR 0006. On the ledger
  side the same bound is arithmetic: no balance may go negative, so there is
  no path by which the system records money it does not hold, and corrections
  never widen the exposure they repair.

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
