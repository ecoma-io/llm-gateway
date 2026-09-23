# Accounting

Reference page for reservations, settlements, usage events, funding buckets,
and the ledger. The decisions are [ADR 0004](../adr/0004-reserve-and-settle-accounting.md)
— in particular the **ten invariants**, which this page assumes and does not
restate. Entity fields are in [overview](overview.md).

## Reservation lifecycle

```text
        admission (one transaction)                  execution
              │                                            │
              ▼                                            │  lease renewed by the
        ┌──────────┐   settle: Settlement + legs          │  live executor while the
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

- `open` — capacity is held; a request has exactly one reservation, ever
  (uniqueness on the request is total), and at most that one can be `open`
  (invariant 6).
- `settled` — closed by the settlement transaction with actual usage.
- `released` — closed by explicit compensation (no candidate could serve).
- `expired` — closed by the reaper because nothing closed it in time; the
  reaper acts only when `expires_at` has passed **and the execution lease is
  dead**. The lease is renewable and strictly outlives the maximum accepted
  request duration, so a live stream cannot be expired under itself.

Settlement after `expired` does not exist: the released capacity may already
be serving a newer request. An orphaned completion proven by attempt
telemetry is recorded as an `unbillable_orphaned` usage event — written by
the **reaper in the same transaction as its release legs** when the proof
exists at expiry, or appended later by reconciliation when it surfaces only
afterwards (appending an unbillable fact late is safe; settling late is
not). It is a reconciliation fact, never a customer charge (ADR 0004).

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

Settlement is one transaction that does, in this order:

1. create the **Settlement** — unique by `request_id`; the exactly-once
   boundary. It carries `request_id`, a `settled_total` equal to the sum of
   its consume legs, and `created_at`. A competing finalizer hits the unique
   constraint and stops.
2. append the `UsageEvent` (immutable fact: committed attempt, normalized
   token counts, price revision snapshot, capture method);
3. append **one consume leg per allocated bucket**, drawing the
   reservation's allocation legs in their stored waterfall order, then the
   **release legs** for the unconsumed tail of each allocation;
4. update each affected funding bucket's projections;
5. close the reservation (`settled`).

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

The method travels on the usage event, so analytics can price confidence.

## Ledger legs and balance projections

`LedgerEntry` rows are **bucket legs** — one funding bucket, one kind, one
positive amount, a per-bucket `sequence` (allocated by incrementing a counter
on the bucket row inside the same transaction, giving each bucket's history a
total order), and a price snapshot **required on `consume` legs** (null
elsewhere — the other kinds move money without consuming tokens),
referencing their reservation or settlement:

| Kind         | Written when                                         | Bucket             | Meaning                                                                     |
| ------------ | ---------------------------------------------------- | ------------------ | --------------------------------------------------------------------------- |
| `grant`      | a subscription's grant cycle rolls                   | entitlement cycle  | capacity issued for the cycle                                               |
| `topup`      | operator today, payment provider later (webhook)     | PAYG               | funding added                                                               |
| `hold`       | admission                                            | entitlement / PAYG | capacity occupied by a reservation                                          |
| `release`    | settlement tail, candidate exhaustion, reaper expiry | entitlement / PAYG | occupied capacity returned                                                  |
| `consume`    | settlement                                           | entitlement / PAYG | capacity actually spent                                                     |
| `adjustment` | explicit operator correction only                    | entitlement / PAYG | compensating pair with reason, original ref, and stated settled/held deltas |

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

Each bucket's funding-bucket row caches these three numbers **inside the
same transaction** that writes legs; the cache exists for contention control
(conditional updates) and is rebuildable from legs — if cache and legs ever
disagree, the legs win and the cache is wrong. `adjustment` is an operator
correction that states its settled/held deltas explicitly; it is never an
automatic overdraw path (the reservation ceiling means automatic debt cannot
arise).

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
  settlement, release/compensation, and cycle roll — the four cross-context
  coordinated transactions (ADR 0001, rule 6) — are short transactions; the
  in-flight hold is data plus a renewable lease, not a lock.
- **Bucket guards**: every capacity drawdown is a conditional update on the
  funding bucket (`available >= take`) inside one of those four coordinated
  transactions — the same mechanism for entitlement buckets and the PAYG
  bucket, so concurrent admissions cannot jointly overdraw either. Refills
  are ledger-backed writes outside the admission path: grant legs from the
  cycle roll, and `topup`/`adjustment` legs (ADR 0004), each carrying its
  own uniqueness guards.
- **Time**: cycle membership and price-revision selection use the database
  clock (`transaction_timestamp()` at admission), never gateway node clocks;
  admission and the cycle roll serialise on the subscription row, so a
  boundary-straddling request deterministically lands in one cycle (ADR
  0003).
- **Reaper idempotence**: state transitions guard the reaper; a reaper racing
  a settlement performs at most one release, because one of them wins the
  reservation's state transition and the loser no-ops.
