# ADR 0005: Storage — PostgreSQL relational state and Timescale-oriented event history

- Status: Accepted
- Date: 2026-09-23
- Issue: [#5](https://github.com/ecoma-io/llm-gateway/issues/5)

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

## Decision

One PostgreSQL cluster, two table families, decided by one rule: **mutable
authoritative state is relational; immutable time-series facts are
Timescale-oriented event tables.** The event tables are hypertables in that
same cluster (TimescaleDB extension); "family" separates access patterns,
partitioning, and retention — not databases. Extraction to a separate
analytics store, if it ever happens, moves **reads** (replicas, aggregates),
never the accounting write path.

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
  concurrent PAYG admission would have nothing to serialise on.
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
  it must be transactional with bucket capacity and reservations. Its volume
  is request-rate, not token-rate.
- `reservations` are mutable lifecycle rows (`open → settled | released |
expired`) with a renewable lease.

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
  they are facts written by those transactions (usage events) or by execution
  (attempts, request finalisation) in the same cluster.
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
- Asynchronous analytics read the event tables and continuous aggregates and
  never write the relational working set. There is no "job that fixes up
  balances": a wrong number is corrected by compensating ledger legs
  (invariants 1–2, ADR 0004).

## Consequences

- The future schema PR has its table list, families, consistency boundaries,
  and retention policy pre-decided; migration ordering follows aggregate
  dependencies (identity → catalog → commerce → accounting → events).
- TimescaleDB becomes a deployment choice with a stated default (same
  cluster); if it is unavailable, the event tables are plain time-partitioned
  tables and nothing in the domain model changes.
- Analytics scale independently of the working set — a slow dashboard query
  cannot contend with admission — while accounting stays single-transaction.
- Cross-family references are by ID within the same database — in **both**
  directions: event rows point at relational state (an attempt names its
  candidate's backend), and relational rows point at event rows (a settlement
  names its request). Enforced foreign keys stop at the family boundary in
  both directions; that is safe because every **load-bearing** guard — the
  unique settlement, the bucket conditional updates, the intake uniqueness —
  lives on the relational side, which is exactly what the family split
  guarantees.

## Alternatives considered

- **Everything in one flat table family**: rejected — request/attempt volume
  grows without bound while analytics wants partitioning and compression, and
  retrofitting hypertables onto transactional tables later is a migration
  nightmare.
- **Event tables in a separate analytics database**: rejected _for the
  accounting path_ — it forces a transactional outbox between a charge and
  its usage fact. Analytics reads may be extracted later; the writes stay
  together.
- **Event sourcing for the relational state**: rejected — conditional
  capacity updates and settlement uniqueness are natural in SQL and painful
  as replayed projections.
- **Ledger in the event family** (it is append-only, after all): rejected —
  placement follows the consistency requirement, not the write pattern; a
  ledger outside the settlement transaction cannot enforce invariants 3–4.
