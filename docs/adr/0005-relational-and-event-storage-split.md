# ADR 0005: Storage — PostgreSQL relational state and Timescale-oriented event history

- Status: Accepted
- Date: 2026-09-23
- Issue: [#5](https://github.com/ecoma-io/llm-gateway/issues/5)
- Amended by: [ADR 0006](0006-control-plane-and-data-plane.md) — the two families now sit in two databases, one per plane

## Context

The gateway's data has two different shapes: mutable, low-volume,
consistency-critical state (is this key revoked? how much capacity remains in
this cycle?) and immutable, high-volume, time-ordered history (every request,
every provider attempt, every usage fact). One access pattern for both either
starves analytics or compromises transactional guarantees. The split must be
decided at the domain level so schema and service boundaries derive from it
rather than being discovered under load.

The accounting model (ADR 0004) adds a hard constraint on the answer: the
settlement transaction must write a settlement, its usage event, its ledger
legs, and bucket projections **atomically**. A split across two databases
would force a transactional outbox into the accounting path — extra tables,
a delivery contract, dedup keys, and a window where a charge exists without
its usage fact.

> **Amended by [ADR 0006](0006-control-plane-and-data-plane.md).** That
> constraint is answered rather than ignored, and the split it rejected is one
> this ADR now makes — along a different line. The event family (`requests`,
> `request_attempts`, `usage_events`) and the runtime's own relational rows live
> in the Data Plane's database; the Control Plane's relational working set lives
> in its own. What replaces the outbox is not a delivery mechanism but an
> inversion: the usage fact is not downstream of the charge, it is the
> **authority the charge is derived from**, written by the process that observed
> it, and the Control Plane settles from it idempotently by `request_id`. The
> window this Context called disqualifying still exists — it is now a named,
> bounded convergence property instead of a transactional guarantee (ADR 0006).
> Between planes, rows are referenced by ID with no foreign key in either
> direction.

## Decision

One PostgreSQL cluster, two table families, decided by one rule: **mutable
authoritative state is relational; immutable time-series facts are
Timescale-oriented event tables.** The event tables are hypertables in that
same cluster (TimescaleDB extension); "family" separates access patterns,
partitioning, and retention — not databases. Extraction to a separate
analytics store, if it ever happens, moves **reads** (replicas, aggregates),
never the accounting write path.

> **Amended by [ADR 0006](0006-control-plane-and-data-plane.md): family and
> database are now two different lines.** They were the same line when this ADR
> was written. They are not any more, and the separation is worth stating
> plainly because everything below reads differently once it holds:
>
> - The **Control Plane's database** holds a relational family and no event
>   family: identity and access, commerce, and the accounting working set
>   (`funding_buckets`, `settlements`, `ledger_entries`).
> - The **Data Plane's database** holds both families: the runtime's own
>   relational rows — Catalog configuration, `request_intake`, `reservations`
>   and their allocation legs, API-key records — and the event family
>   (`requests`, `request_attempts`, `usage_events`), with the TimescaleDB
>   extension and the hypertables.
>
> So a table's **family** is still decided by the placement rule below, and its
> **database** is decided by which plane owns it — which is a different question,
> with a different answer. `reservations` is the row that makes the difference
> visible: mutable, low-volume, lockable, unambiguously relational by the rule,
> and yet the runtime's, because the hot path draws it down and must be able to
> do that with the Control Plane unreachable. The table-by-table matrix with a
> reason per row is in
> [../architecture/planes.md](../architecture/planes.md).

> **Amended (B7, runtime storage): the event family ships as plain tables, not
> hypertables.** The runtime's seven tables landed in
> `migrations/dataplane/000002_runtime_storage` unpartitioned, because three
> constraints the money path leans on cannot be expressed on a hypertable: the
> partitioning-column rule forbids `requests(id)` as primary key, the attempts'
> business key, and the usage-fact dedup partial uniques on
> `usage_events(request_id)`; the engine refuses a foreign key that references a
> hypertable, which would take the intra-family guarantees (attempts and facts
> referencing their billing subject; the request's committed-attempt pairing)
> with it; and a later conversion would have to rebuild exactly those
> constraints. What the deviation gives up: columnstore compression on
> `request_attempts`, chunk-based retention pruning, and continuous aggregates
> over `usage_events` until an equivalent is built. The family rule above —
> what is relational, what is an event table — is unchanged; this amendment is
> about the physical placement of the second family, which follows the same
> logic the rule always did: the guards the money path needs outrank the
> operational conveniences of partitioning.

### Relational family

```text
accounts        users        api_keys        request_intake
plans           subscriptions    entitlements    funding_buckets
client_price_list_revisions     alias_group_versions
model_aliases (+ candidates)    backends
reservations (+ allocation legs)    settlements (+ ledger legs)
```

- These answer **"what is true now, or was decided"**: identity and access,
  catalog configuration, commerce lifecycle, and the accounting working set
  (funding-bucket capacity, open reservations, the append-only ledger legs).
- `funding_buckets` exists because admission needs a **lockable capacity
  row** per entitlement cycle and per PAYG balance (ADR 0004). Without it,
  concurrent PAYG admission would have nothing to serialise on. The bucket
  is Accounting-owned (ADR 0001); the entitlement cycle or PAYG balance it
  projects is Commerce's, referenced by identifier — which is why the table
  sits with the accounting working set it must be transactional with, not
  beside the tables of the context that projects onto it.
- `request_intake` exists because idempotent replay must be decidable inside
  the admission transaction **from relational state alone** — an event-family
  row is never consulted for a business decision (below).
- `settlements` exists because the double-settlement guard must be a single
  unique row per request; `ledger_entries` are its legs (per bucket), which is
  what makes split settlement expressible.
- `client_price_list_revisions`, `alias_group_versions`, and immutable plan
  versions exist so that a past admission can be reconstructed without asking
  what today's configuration says (invariant 10).
- The ledger lives here, not in the event family: append-only is a write
  pattern, but the placement rule follows the **consistency requirement** —
  it must be transactional with the bucket capacity it moves. Its volume is
  request-rate, not token-rate. (Before the plane split this clause read "with
  bucket capacity and reservations"; the requirement is unchanged, but
  `reservations` are the runtime's rows and now sit in the other database, so
  what the ledger is transactional with is the bucket it draws down
  — ADR 0006, and the amendments below.)
- `reservations` are mutable lifecycle rows (`open → settled | released |
expired`) with a renewable lease.
- The list above splits across the two databases, because the plane that writes
  a row is what decides its address. The **Control Plane's** are `accounts`,
  `users`, `plans`, `subscriptions`, `entitlements`, and the accounting working
  set — `funding_buckets`, `settlements` with their ledger legs. The **Data
  Plane's** are `model_aliases` with their candidates, `backends`,
  `alias_group_versions`, `client_price_list_revisions` (Catalog configuration,
  which the runtime prices from — ADR 0003), and the runtime's own
  `request_intake` and `reservations` with their allocation legs. `api_keys`
  appears on both sides, and deliberately: the Control Plane owns which
  account a key belongs to and who may manage it, and the Data Plane owns the
  record the hot path authenticates against (ADR 0006, section 8).
  `funding_buckets` is the entry whose ownership
  the split changed rather than only its address — ADR 0004 now defines it as
  two rows in two planes, the Accounting bucket here and the runtime's quota
  projection there.

### Event family (hypertables)

```text
requests        request_attempts        usage_events
```

- These answer **"what happened, when"**: the logical request (created as a
  shell at admission and finalised once), one row per upstream call, and the
  immutable usage facts.
- `requests` rows are created at admission — not only at finalization — so
  attempts and the reservation can reference a request that exists from its
  first moment. The row is finalised exactly once (status, timing, the
  committed attempt).
- Events are never part of an admission or settlement **read** dependency;
  they are facts written by the runtime's transactions (usage events, in the
  Data Plane) or by execution (attempts, request finalisation), all of them in
  the one cluster the plane they belong to owns.
- Analytical queries and continuous aggregates read these tables, never the
  relational working set.

### Placement rule for anything new

_Do rows of this table change after insert, or must they be read in a
coordinated transaction with mutable state?_ Yes → relational. No (append-only
fact, time-ordered, analytics-oriented) → event table. A table that needs both
is two tables, and the design is wrong, not the rule.

### Retention

- `ledger_entries`, `settlements`, `usage_events`, `reservations`: **retained
  forever** — billing and audit history; corrections are compensating rows,
  never deletions.
- `requests`: **retained forever** too, deliberately. It is one row per
  request — the same order of magnitude as the ledger legs that already
  reference it — and it is the audit anchor joining a charge to its
  usage event and its attempts. Deleting it would leave ledger references
  dangling for no meaningful saving.
- `request_attempts`: the genuinely high-volume, provider-telemetry table.
  It may carry a retention/compression policy (aggregated into analytics
  before ageing out); attempts are operational telemetry, not customer
  billing records, and no ledger row references them.

### Synchronous path vs analytics

- The synchronous path writes relational state (admission, settlement) and
  appends event rows; settlement writes both families in **one transaction on
  one cluster**, which is the whole reason the families share a database.
  Amended by ADR 0006: within the **Control Plane** the settlement writes its
  relational rows in one transaction, and the usage fact it settles is written
  in the **Data Plane** and referenced by ID. No transaction writes both
  families, because no transaction spans the two databases they now occupy.
- Asynchronous analytics read the event tables and continuous aggregates and
  never write the relational working set. There is no "job that fixes up
  balances": a wrong number is corrected by compensating ledger legs
  (invariants 1–2, ADR 0004). Reconciliation across the plane boundary is not
  that job and must not become it — it derives Control-Plane rows from
  Data-Plane facts and corrects through compensating legs like any other
  correction, never by editing a balance to match.

## Consequences

- The future schema PR has its table list, families, consistency boundaries,
  and retention policy pre-decided; migration ordering follows aggregate
  dependencies (identity → catalog → commerce → accounting → events).
- TimescaleDB becomes a deployment choice with a stated default (same
  cluster); if it is unavailable, the event tables are plain time-partitioned
  tables and nothing in the domain model changes.
- Analytics scale independently of the working set — a slow dashboard query
  cannot contend with admission — while accounting stays single-transaction
  **per plane**: the runtime's close is one Data-Plane transaction, and the
  Control Plane's settlement from that fact is another (ADR 0006).
- Cross-family references are by ID within the same database — in **both**
  directions: event rows point at relational state (an attempt names its
  candidate's backend), and relational rows point at event rows (a settlement
  names its request). Enforced foreign keys stop at the family boundary in
  both directions; that is safe because every **load-bearing** guard — the
  unique settlement, the bucket conditional updates, the intake uniqueness —
  lives on the relational side, which is exactly what the family split
  guarantees. Amended by ADR 0006: the Data Plane's database holds both
  families, so this bullet describes it; the Control Plane's database holds one,
  and the references from its rows to the runtime's — a settlement naming the
  request it settles, a ledger leg naming the reservation it holds against —
  cross a database boundary and therefore carry no foreign key at all. They are
  ID references that nothing in the engine can enforce, which is why the guards
  that matter are all reachable inside the transaction that writes them and why
  the reconciliation path exists to notice a reference that never arrives.

## Alternatives considered

- **Everything in one flat table family**: rejected — request/attempt volume
  grows without bound while analytics wants partitioning and compression, and
  retrofitting hypertables onto transactional tables later is a migration
  nightmare.
- **Event tables in a separate analytics database**: rejected _for the
  accounting path_ — it forces a transactional outbox between a charge and
  its usage fact. Analytics reads may be extracted later; the writes stay
  together. ADR 0006 parts the event tables from the ledger's tables anyway, and
  this objection is the one it had to answer first: its answer is that the fact
  becomes the authority the charge derives from, so what would have been an
  outbox carrying a charge to its fact becomes a fact the charge is computed
  from, delivered by a retry that is safe.
- **Event sourcing for the relational state**: rejected — conditional
  capacity updates and settlement uniqueness are natural in SQL and painful
  as replayed projections.
- **Ledger in the event family** (it is append-only, after all): rejected —
  placement follows the consistency requirement, not the write pattern; a
  ledger outside the settlement transaction cannot enforce invariants 3–4.
