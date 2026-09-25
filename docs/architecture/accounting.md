# Accounting

Reference page for reservations, settlements, usage events, funding buckets,
and the ledger. The decisions are [ADR 0004](../adr/0004-reserve-and-settle-accounting.md)
— in particular the **ten invariants**, which this page assumes and does not
restate. Entity fields are in [overview](overview.md).

The model is two layers, and [ADR 0006](../adr/0006-control-plane-and-data-plane.md)
is why. **The ledger is the Control Plane's**: a funding bucket, the settlement
of record and its legs. **The capacity the runtime enforces is a quota
projection in the Data Plane**: a ceiling, not a balance, seeded from
Control-Plane grants and written only by the runtime. The two converge by
reconciliation, and only one of them is authoritative for money.

## The concepts, and who owns each

A page that blurs any two of these will mislead whoever builds from it. Six are
the Data Plane's vocabulary, three are the Control Plane's, and the line between
them is where money is allowed to be decided.

| Concept                    | Plane         | What it is                                                                                                               |
| -------------------------- | ------------- | ------------------------------------------------------------------------------------------------------------------------ |
| **Request**                | Data Plane    | the logical client request and the billing subject; created at admission, finalised once.                                |
| **Request Attempt**        | Data Plane    | one upstream call, retries included; never bills on its own.                                                             |
| **Reservation**            | Data Plane    | the pre-execution hold and hard execution ceiling. Opened, leased and closed by the runtime.                             |
| **Reservation Allocation** | Data Plane    | a reservation's leg against one funding bucket, with its waterfall ordinal — the `allocation legs` the ADRs name.        |
| **Quota Projection**       | Data Plane    | the runtime's lockable enforcement ceiling for one entitlement cycle or PAYG balance. Not a balance, and not the ledger. |
| **Usage Event**            | Data Plane    | the immutable fact the runtime writes when it closes a reservation; the authority a charge is derived from.              |
| **Funding Bucket**         | Control Plane | the ledger's bucket behind an entitlement or the PAYG balance. The source of truth for money.                            |
| **Settlement**             | Control Plane | the unique-per-request accounting header, written from the usage fact; the exactly-once boundary.                        |
| **Ledger Entry**           | Control Plane | one append-only bucket leg of a reservation or a settlement.                                                             |

Two pairs in that list get collapsed, and neither collapse is allowed:

- **Quota Projection ≠ Funding Bucket.** The projection is the runtime's
  enforcement ceiling — the row admission conditionally draws down with the
  Control Plane unreachable — while the bucket is financial authority: the
  ledger's record of what was granted, held, consumed and released. Only the
  bucket is authoritative for money, and only the projection can refuse a
  request at admission. They converge by reconciliation, and the projection is
  rebuildable from nothing except that convergence (ADR 0006's amendment to ADR
  0004).
- **A Reservation Allocation is not a Ledger Entry.** The allocation is the
  runtime's record of which buckets a hold was split across and in what order;
  the `hold`, `consume` and `release` legs are the ledger's, written in the
  Control Plane from the fact. The order is the same and the rows are not.

**Who writes what is the boundary.** The runtime never writes `ledger_entries`,
`funding_buckets` or `settlements`; the Control Plane never provides runtime
admission by synchronously reading them. Admission is allowed or refused against
the runtime's own projection — with the Control Plane down, if that is how the
deployment happens to be — and the ledger follows from the facts the runtime
observed (ADR 0006, sections 2 and 5).

**Settlement must be derivable from the fact alone.** It is derived from the
`UsageEvent`, idempotent by `request_id`, and it has to be possible **without a
synchronous callback into the Data Plane**: a settlement that had to ask the
runtime a question mid-transaction would be the synchronous cross-plane call the
split forbids. That is a requirement on the fact contract rather than on this
page — the payload carries the allocation and bucket identities and the
immutable reservation figures needed to derive the consume legs, the release
legs and the settled total (`api/openapi/shared/usage-facts.yaml`; the schema PR
fixes the columns).

## Reservation lifecycle

```text
        admission (one transaction)                  execution
              │                                            │
              ▼                                            │  lease renewed by the
        ┌──────────┐   close: usage fact; settlement      │  live executor while the
        │   open   │─────────────────────────────────▶   │  request runs
        └────┬─────┘   ┌───────────┐                     │
             │         │  settled  │                     │
             │         └───────────┘                     │
             │ all candidates exhausted,                 │
             │ pre-commitment only                       │
             ├───────────────────────────────▶ ┌───────────┐
             │                                │ released  │  full release legs
             │                                └───────────┘
             │ expires_at passed AND lease dead
             │ (process died / abandoned request)
             └───────────────────────────────▶ ┌───────────┐
                                               │  expired  │  reaper releases
                                               └───────────┘
```

- `open` — capacity is held, against the runtime's own quota projection, all
  of it in the Data Plane (ADR 0006); a request has exactly one reservation,
  ever (uniqueness on the request is total), and at most that one can be
  `open` (invariant 6).
- `settled` — closed by the runtime: one Data-Plane transaction writes the
  usage fact and flips the state, and the Control Plane's Settlement is
  derived from that fact afterwards, idempotently by `request_id` (ADR 0006).
- `released` — closed by explicit compensation (no candidate could serve), in
  the Data Plane; the Control Plane learns of it as a fact.
- `expired` — closed by the runtime's reaper because nothing closed it in
  time; the reaper acts only when `expires_at` has passed **and the execution
  lease is dead**. The lease is renewable and strictly outlives the maximum accepted
  request duration, so a live stream cannot be expired under itself.

Settlement after `expired` does not exist: the released capacity may already
be serving a newer request. An orphaned completion proven by attempt
telemetry is recorded as an `unbillable_orphaned` usage event — written by
the **reaper in the same Data-Plane transaction that closes the reservation**
when the proof exists at expiry (the Control Plane's release legs then follow
from that terminal state), or appended later by reconciliation when it
surfaces only afterwards (appending an unbillable fact late is safe; settling
late is not). It is a reconciliation fact, never a customer charge (ADR
0004).

### Reservation sizing — a hard ceiling

```text
reserved = price(revision_at_admission, canonical input tokens
                                     + canonical max_output_tokens)
```

`max_output_tokens` is a required, validated client field (ADR 0003); input
tokens are counted with the gateway's canonical tokenizer, the reservation is
priced once, at the admission-time price revision, and copied immutably onto
the reservation. A computed hold above the alias's **reservation cap** (a
maximum hold in minor units) rejects `invalid_request` before any capacity is
taken. Adapters enforce the output bound, so billable usage never exceeds the
hold — it can only come in under it. At settlement, provider-**reported**
usage is priced when present; otherwise delivered content is re-counted
canonically (the capture method records which).

## Settlement

Settlement is **two transactions in two databases**, and the plane boundary is
why it can no longer be one: these rows do not all live in the same database,
so no transaction can write them all (ADR 0006).

**In the Data Plane**, the runtime closes the reservation — one transaction
here, and it is the half that observes what happened:

1. close the reservation (`settled`), state-guarded so exactly one writer
   closes it — this CAS decides who appends a fact at all, and the loser
   reads false and appends nothing;
2. append the `UsageEvent` (immutable fact: committed attempt, normalized
   token counts, price revision snapshot, capture method) as the unit of
   work's **last** statement. The fact is the **authority the charge is
   derived from**, not a side effect of it.

Both statements are guarded twice over in the landed schema: the close is a
`WHERE state = 'open'` update, and the fact's append sequence is allocated
from the stream row whose lock is held to commit
— which is why the append is written **last** in the unit of work, after the
close has decided the writer. The ordering guarantee the fact feed sells
([cross-plane protocols](cross-plane-protocols.md)) is minted by that same
discipline:
settlement order is visibility order because the statement that takes a
position in it is the one the transaction commits behind.

**In the Control Plane**, settlement runs from that fact, idempotently by
`request_id`, in this order:

1. create the **Settlement** — unique by `request_id`; the exactly-once
   boundary. It carries `request_id`, a `settled_total` equal to the sum of
   its consume legs, and `created_at`. A competing finalizer hits the unique
   constraint and stops.
2. append **one consume leg per allocated bucket**, then the **release legs**
   for the unconsumed tail of each allocation — the amounts come from the
   fact, which carries what was held and what was delivered;
3. update each affected funding bucket's projections.

The fact's carrying the held and delivered amounts is what lets the Control
Plane settle without so much as a reference to the runtime's reservation rows:
it knows the request by ID and nothing else (ADR 0006 §5). Exactly-once
survives twice over — the runtime's close is state-guarded locally, and the
Control Plane's unique settlement per request is unchanged — while the
atomicity between the charge and the fact it derives from is gone,
deliberately: what replaces it is a fact that already exists, a retry that is
safe, and a convergence property that is named (ADR 0006).

Concretely, a hold split S1: 20 + S2: 40 (S1 expires sooner) settling at 30
writes: `Settlement S` → consume S1: 20, consume S2: 10, release S2: 30 —
three legs, one settlement, one usage event. The per-bucket uniqueness on
`(settlement, bucket, kind)` permits the split while blocking duplicate
movement; the settlement's own `request_id` uniqueness blocks double
settlement (invariant 4).

### Billable delivery and capture methods

The billing subject is the request's single `committed_attempt_id` (ADR
0002's commitment gate). Billable usage is the **customer-visible delivery
boundary**: content the gateway actually forwarded before the stream ended —
including ending by client disconnect, where the gateway cancels upstream and
captures what was already delivered. Provider-reported usage that arrives
after a disconnect or post-commitment failure is provider-cost telemetry on
the attempt record; it is never silently substituted as customer usage.

| Capture method      | When it is used                                                                                      |
| ------------------- | ---------------------------------------------------------------------------------------------------- |
| `reported`          | The provider's terminal usage is present and the delivery is complete.                               |
| `gateway_observed`  | Delivery was partial (disconnect / post-commitment failure); the gateway counts forwarded deltas.    |
| `reservation_floor` | No reliable count exists for a committed attempt; the already-held amount is charged conservatively. |

The method travels on the usage event — it is the `usage_events.capture_method`
column since B7's runtime storage landed, one of exactly these three values by
schema CHECK, so analytics can price confidence from the row itself.

## Ledger legs and balance projections

This section is the Control Plane's half of the picture, and the half that
decides what money is owed. `LedgerEntry` rows are **bucket legs** — one
funding bucket, one kind, one positive amount, a per-bucket `sequence`
(allocated by incrementing a counter on the bucket row inside the same
transaction, giving each bucket's history a total order), and a price snapshot
**required on `consume` legs** (null elsewhere — the other kinds move money
without consuming tokens), referencing their settlement or, across the plane
boundary, their reservation **by ID alone** — no foreign key exists in either
direction (ADR 0006 §7):

| Kind         | Written when                                     | Bucket             | Meaning                                                                     |
| ------------ | ------------------------------------------------ | ------------------ | --------------------------------------------------------------------------- |
| `grant`      | a subscription's grant cycle rolls               | entitlement cycle  | capacity issued for the cycle                                               |
| `topup`      | operator today, payment provider later (webhook) | PAYG               | funding added                                                               |
| `hold`       | after admission, from the reservation as a fact  | entitlement / PAYG | capacity occupied by a reservation                                          |
| `release`    | settlement tail, exhaustion, reaper expiry       | entitlement / PAYG | occupied capacity returned                                                  |
| `consume`    | settlement                                       | entitlement / PAYG | capacity actually spent                                                     |
| `adjustment` | explicit operator correction only                | entitlement / PAYG | compensating pair with reason, original ref, and stated settled/held deltas |

**Formal projections** per funding bucket (G = grants/topups, C = consumes,
H = holds, R = releases, A = adjustment `(settled_delta, held_delta)` pairs):

```text
settled   = ΣG − ΣC + ΣA.settled_delta
held      = ΣH − ΣR − ΣC + ΣA.held_delta
available = settled − held        (must stay ≥ 0; admission enforces it)
```

Without adjustments the non-adjustment kinds cannot make `settled` negative
(bucket guards keep `ΣG ≥ ΣC`); a negative settled balance is reachable only
through an operator `adjustment` — that is why the algebra separates it.

Example — grant 100, hold 12, consume 7, release 5:

```text
settled   = 100 − 7        = 93
held      = 12 − 5 − 7     =  0
available = 93 − 0         = 93
```

Each funding-bucket row in the Control Plane caches these three numbers
**inside the same transaction** that writes its legs; the cache exists for
contention control (conditional updates) and is rebuildable from legs — if
cache and legs ever disagree, the legs win and the cache is wrong. The funding
bucket is an **Accounting aggregate** (ADR 0001) and it lives in the Control
Plane: its row is written only by the accounting flows of that plane — the
Control-Plane halves of the four coordinated transactions (settlement, and the
cycle roll's grant), the `hold`/`release` legs derived from the reservation's
facts, and the context-local `topup`/`adjustment` refills — while the Commerce
entitlement cycle or PAYG flag it projects references it by identifier. `adjustment` is an operator correction that states its
settled/held deltas explicitly; it is never an automatic overdraw path (the
reservation ceiling means automatic debt cannot arise).

**The Data Plane holds no copy of this row.** What the runtime conditionally
draws down is the _quota projection_: the lockable capacity row for one
entitlement cycle or PAYG balance, seeded from a Control-Plane grant, updated
only by the runtime, and converged to the ledger by reconciliation. It is not
a balance and not a second ledger — it is the ceiling admission enforces, and
the ledger, never the projection, is the source of truth for money (ADR
0006).

**A grant is eligible only for the request it may fund.** The drawdown walks
the waterfall in the domain's order — entitlement cycles strictly before the
PAYG balance, then named scope before `*`, earliest period end, oldest
subscription — but only among grants that are eligible for _this_ request: the
grant's stored scope must contain the requesting alias (the wildcard `*`
version contains every alias; a named version exactly the aliases its snapshot
lists; a version id nothing answers for contains nothing, a treat-as-zero and
never an error), and the grant's cycle must not have ended at the
transaction's own clock. Eligibility is re-asserted under the row lock by the
conditional take itself — the same predicate, deliberately the same text — so
a grant whose scope or cycle moved between the walk and the take is never
drawn on a stale read's word. A grant that fails eligibility is passed by, not
fatal: only a final shortfall surfaces, as the usual refusal with nothing
drawn.

### Worked ledger sequences

**Success, no fallback, usage below the hold** — entitlement bucket granted
100 this cycle:

```text
admission   hold     S1  12          → bucket: held 12
settle      consume  S1   7
            release  S1   5          → bucket: held 0, settled 100 − 7 = 93
```

**Fallback** — candidate 1 fails before commitment, candidate 2 serves. One
reservation; only the committed attempt settles (invariant 6):

```text
admission   hold     S1  12
attempt×2   (attempt rows only — failed attempts never create legs)
settle      consume  S1  11
            release  S1   1
```

**Stream dies after commitment, client received part** — capture method
`gateway_observed`, observed delivery prices to 20 against a hold of 30:

```text
admission   hold     S1  30
settle      consume  S1  20
            release  S1  10
```

The hold always covers the charge (hard ceiling), so `release` is never
negative; usage above the hold is impossible by construction, not by luck.

**Client disconnects mid-stream** — same shape as the previous case, with
capture method `gateway_observed`; provider's late usage report for the
cancelled remainder lands on the attempt row as provider-cost telemetry only.

**Crash before settlement** — lease dies, reaper closes an abandoned hold:

```text
admission   hold     S1  12
reaper      release  S1  12         (state: expired)
```

No settlement exists; if attempt telemetry proves the orphaned completion,
an `unbillable_orphaned` usage event is appended for reconciliation — with
the reaper's release when the proof is already there, otherwise later
(ADR 0004). Under-accounting is possible
in exactly this crash case; over-billing is not. The asymmetry is deliberate.

## Concurrency and time

- **No locks across provider calls** (ADR 0001, rule 7). Admission,
  settlement, release/compensation and cycle roll — the four cross-context
  coordinated transactions (ADR 0001, rule 6) — are short transactions, and
  each is now local to one plane, settlement being two of them rather than one
  (ADR 0006). The in-flight hold is data plus a renewable lease, not a lock.
- **Capacity guards**: every drawdown is a conditional update
  (`available >= take`) on the capacity row it guards, inside its plane's own
  transaction — the runtime's quota projection at admission, the Control
  Plane's funding bucket at settlement and at the cycle roll. It is the same
  mechanism for entitlement capacity and the PAYG balance, so concurrent
  admissions cannot jointly overdraw either. Refills are ledger-backed writes
  outside the admission path: grant legs from the cycle roll, and
  `topup`/`adjustment` legs (ADR 0004), each carrying its own uniqueness
  guards.
- **Time**: cycle membership and price-revision selection use the database
  clock (`transaction_timestamp()` at admission), never gateway node clocks;
  admission and the cycle roll serialise on the subscription row, so a
  boundary-straddling request deterministically lands in one cycle (ADR
  0003).
- **Reaper idempotence**: state transitions guard the reaper; a reaper racing
  a settlement performs at most one release, because one of them wins the
  reservation's state transition and the loser no-ops.
