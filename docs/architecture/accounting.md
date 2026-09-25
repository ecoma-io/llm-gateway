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
  Control Plane from the reservation's terminal fact. The order is the same
  and the rows are not.

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
   its consume legs, and `created_at`. A competing acknowledgement of the
   same request inserts nothing and reads the recorded settlement instead:
   the same total converges, and a different one is the conflict defect —
   one request cannot settle twice at two totals.
2. append **one consume leg per allocated bucket**, then the **release legs**
   for the unconsumed tail of each allocation — the amounts come from the
   fact, which carries what was held and what was delivered. Every leg lands
   with the bucket move it names, in the same unit of work: the guarded echo
   (balances, sequence, version) and the leg's insert are one statement pair
   under one savepoint, so a header without its legs cannot commit and a leg
   that loses its guard leaves no header behind.

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

| Kind         | Written when                                             | Bucket             | Meaning                                                                     |
| ------------ | -------------------------------------------------------- | ------------------ | --------------------------------------------------------------------------- |
| `grant`      | a subscription's grant cycle rolls                       | entitlement cycle  | capacity issued for the cycle                                               |
| `topup`      | operator today, payment provider later (webhook)         | PAYG               | funding added                                                               |
| `hold`       | from the reservation's terminal fact, never at admission | entitlement / PAYG | capacity the reservation held, carried by the fact's allocation tail        |
| `release`    | settlement tail, exhaustion, reaper expiry               | entitlement / PAYG | occupied capacity returned                                                  |
| `consume`    | settlement                                               | entitlement / PAYG | capacity actually spent                                                     |
| `adjustment` | explicit operator correction only                        | entitlement / PAYG | compensating pair with reason, original ref, and stated settled/held deltas |

**Formal projections** per funding bucket (G = grants/topups, C = consumes,
H = holds, R = releases, A = adjustment `(settled_delta, held_delta)` pairs):

```text
settled   = ΣG − ΣC + ΣA.settled_delta
held      = ΣH − ΣR − ΣC + ΣA.held_delta
available = settled − held        (must stay ≥ 0 — the hold guard refuses the overdraw)
```

**Where ΣH comes from, and what `held` therefore is.** A hold is never a
fact on the cross-plane feed: it is Data-Plane state — the reservation and its
allocation legs, drawn down against the runtime's quota projection at
admission — and the runtime writes none of this plane's rows. The feed
(`api/openapi/shared/usage-facts.yaml`) carries **terminal outcomes only** —
`settled`, `released`, `expired`, `unbillable_orphaned` — so no kind delivers
a hold leg, and no consumer of the feed can read a bucket's held amount at an
instant. What reaches this plane instead is each terminal fact's allocation
tail, whose per-bucket `amount` is the held amount that reservation booked
([cross-plane protocols](cross-plane-protocols.md)), and the `hold` leg is
booked here from that tail — before the `release`/`consume` legs it funds,
which the engine requires anyway (a release names a reservation that has a
hold leg on file, and a consume draws against held). Idempotency runs through
the fact: the applier keys on `request_id`, and a replayed leg collides on its
own uniqueness — `(reservation_id, funding_bucket_id, kind)` for a hold —
rather than moving money twice. B12's applier is the caller that will do it —
B6 shipped the primitive and no caller, and no other channel exists: the feed
is the only path by which a `dataplane` row becomes a `control` row ([data
implications](data-implications.md)), and the management surface contracts no
read of live reservations.

The consequence for the projection above is exact: a bucket's cached `held`
is algebra over holds whose terminal fact has already been applied, never the
runtime's in-flight total. The two differ by the reservations still open in
the Data Plane, plus the delivery latency of those reservations' facts — the
window between a committed reservation and its settlement that ADR 0006 names
as a bounded property, for reconciliation (B13) to converge — and the hold
guard's `available ≥ take` is judged against the bucket as the applied facts
left it, not as admission left it. The guard that refuses an overdraw at
admission is the runtime's projection, not this column; `held` is the ledger's
record of what was held and how it ended.

No kind can make any of the three negative. The non-adjustment kinds cannot
— consume's guard checks the take against held _and_ settled before either
moves — and an `adjustment` cannot either: its stated deltas are free values,
but a delta that would drive settled, held or available below zero is
refused, by the domain constructor before any statement runs and by the
guarded echo inside it. That is the **no-credit rule** (ADR 0004, as amended
below), and it is why the algebra separates adjustments: their deltas are
_stated_, not derivable from the kind — never because they may overdraw. A
correction fixes records; it never creates debt.

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
terminal fact, and the context-local `topup`/`adjustment` refills — while the Commerce
entitlement cycle or PAYG flag it projects references it by identifier. One
entitlement cycle and one account each own exactly one bucket (the owner
exclusivity is a schema `CHECK`, not a convention), and the bucket is never
re-pointed: the PAYG row's reference to it is write-once, so an account's
funded money has one home for its whole life — and the reference names only
its own account's bucket, a match the engine refuses to see broken (an
assignment naming another account's bucket is an error, not a silent no-op).
`adjustment` is an operator
correction that states its settled/held deltas explicitly, with the reason and
the original entry it corrects; it is never an automatic overdraw path (the
reservation ceiling means automatic debt cannot arise) and never a credit
mechanism (the no-credit rule above).

The legs carry their own provenance promises in the engine: a `release` names
a reservation this bucket actually booked a `hold` for, and an `adjustment`
cites an original entry that belongs to this bucket — a release or correction
naming someone else's history is refused, not booked. What the engine does
_not_ pin is the per-reservation hold ceiling: consume legs settle the
reservation without naming it, so "released ≤ held for this reservation" is
the runtime's admission state (ADR 0006 §7) — the ledger pins the part it can
derive, and the reservation's own figures travel with the reservation.

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

The left column names the **Data-Plane event** a leg is derived from, not when
the Control Plane books it. Every leg of a request's money — its `hold` and the
`release`/`consume` legs that end it — is booked in this plane when that
request's terminal fact is applied, because that is the only thing the runtime
publishes (see **Where ΣH comes from**, above). A bucket's committed `held`
therefore never tracks a reservation still being served.

**Success, no fallback, usage below the hold** — entitlement bucket granted
100 this cycle:

```text
reserved     hold     S1  12
settled      consume  S1   7
             release  S1   5          → bucket: held 0, settled 100 − 7 = 93
```

**Fallback** — candidate 1 fails before commitment, candidate 2 serves. One
reservation; only the committed attempt settles (invariant 6):

```text
reserved     hold     S1  12
attempt×2    (attempt rows only — failed attempts never create legs)
settled      consume  S1  11
             release  S1   1
```

**Stream dies after commitment, client received part** — capture method
`gateway_observed`, observed delivery prices to 20 against a hold of 30:

```text
reserved     hold     S1  30
settled      consume  S1  20
             release  S1  10
```

The hold always covers the charge (hard ceiling), so `release` is never
negative; usage above the hold is impossible by construction, not by luck.

**Client disconnects mid-stream** — same shape as the previous case, with
capture method `gateway_observed`; provider's late usage report for the
cancelled remainder lands on the attempt row as provider-cost telemetry only.

**Crash before settlement** — lease dies, reaper closes an abandoned hold:

```text
reserved     hold     S1  12
expired      release  S1  12         (state: expired)
```

No settlement exists; if attempt telemetry proves the orphaned completion,
an `unbillable_orphaned` usage event is appended for reconciliation — with
the reaper's release when the proof is already there, otherwise later
(ADR 0004). Under-accounting is possible
in exactly this crash case; over-billing is not. The asymmetry is deliberate.

### The engine is the last line of defense

Everything above reads as this plane's discipline, and the foundation made it
structural: the PostgreSQL schema states the same rules in its own grammar, so
a leg or a row this page would refuse is one the database refuses too — with
nothing depending on this process being the writer, which is what makes the
ledger safe to read from any process and its projections safe to rebuild.

- `funding_buckets_balance_projection` CHECKs the projection identity
  (`available = settled − held`) with `held ≥ 0` and `available ≥ 0`, which
  makes `settled ≥ 0` transitive — the no-credit rule as arithmetic, not
  sentiment. `funding_buckets_owner_xor` keeps a bucket exactly one owner's
  (an entitlement cycle or an account, never both, never neither).
- `ledger_entries_leg_algebra` pins every kind's deltas to the table above;
  `ledger_entries_price_snapshot` keeps the price provenance on `consume`
  legs and off every other kind; `ledger_entries_reference_shape`,
  `ledger_entries_adjustment_shape` and `ledger_entries_command_key_scope`
  hold the reference grammar each kind carries — a hold names its
  reservation, a consume names its settlement, an adjustment names its
  reason, original and operator, and only a keyed command carries a command
  key.
- The idempotency keys are indexes, not conventions: a partial unique on
  `(funding_bucket_id, command_key)` where a key exists, on
  `(settlement_id, funding_bucket_id, kind)` where a settlement does, and on
  `(reservation_id, funding_bucket_id, kind)` where a reservation does —
  plus the total order itself, `UNIQUE (funding_bucket_id, sequence)`. A
  replayed command or movement collides before it can move money twice.
- Append-only is a trigger, not a promise: `UPDATE` or `DELETE` on
  `ledger_entries` or `settlements` raises with the correction path named in
  the message, and the PAYG row's `funding_bucket_id` is write-once the same
  way — re-pointing and deleting the reference are both refused.

For the promises the engine carries outright — append-only, write-once,
provenance, owner-match — the adapter's WHERE-clause guards and its sentinel
classification are a translation of verdicts the engine guarantees before
the statement even runs, not the last line of defense wearing a Go
interface. The funder-ownership rule and the sufficiency each leg's own
deltas demand are the other kind: no single row can hold both sides, so the
domain states them and the echo's WHERE clause states them again — the same
guard in two vocabularies, and the classification names which one refused.
A writer that bypasses both has only the engine's words to answer to, which
is exactly why the derivable promises were made engine promises at all.

### What the foundation carries, and what waits

B6 lands the money authority itself: the funding bucket aggregate, the six
leg kinds and their algebra, the guarded single-statement echo that moves a
bucket and its leg together, the settlement of record with its exactly-once
edge, the keyed `topup` and stated `adjustment` flows, the reconciliation
read that keeps the cache honest, and the entitlement-funding and
account-funding choreographies commerce calls. The flows that will drive it
arrive later, and none is stubbed here:

- **Admission (B8)** — the runtime's reservation waterfall against its quota
  projection; admission writes no fact — the feed carries terminal outcomes
  only — so this plane's `hold` legs wait for the terminal fact's allocation
  tail, as **Where ΣH comes from** above records.
- **Usage → pricing → settlement wiring (B12)** — the consumer loop that
  reads the fact feed and calls Settle; what that feed must carry is the
  open defect of
  [issue #63](https://github.com/ecoma-io/llm-gateway/issues/63).
- **Reconciliation worker (B13)** — the scheduled convergence between the
  Data Plane's quota projections and the ledger; `ReconcileBucket` is the
  verdict it will lean on.
- **Payment processor (B15)** — the provider whose webhooks land as topups;
  an operator's keyed topup is the same door it will use.

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
