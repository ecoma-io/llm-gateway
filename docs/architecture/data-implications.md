# Data implications

Reference page mapping the domain model onto the intended storage split. The
decision is [ADR 0005](../adr/0005-relational-and-event-storage-split.md):
**one PostgreSQL cluster; mutable authoritative state is relational; immutable
time-series facts are Timescale-oriented event tables in that same cluster**
(hypertables in the ADR's original wording — the landed schema ships them as
plain tables, [below](#event-family-in-dataplane)).
[ADR 0006 §7](../adr/0006-control-plane-and-data-plane.md)
splits that cluster into **two databases, one per plane** — `control` for the
Control Plane API, `dataplane` for the runtime, one migration lane each — so
the family a table belongs to and the database it lands in are two separate
answers, and every table below states both. The runtime's half of this page has
a landed schema (`migrations/dataplane/000003_runtime_storage`, the B7 runtime
storage foundation); the Control Plane's tables are still constraints here, not
DDL. Where a landed schema and this page describe the same row, the migration —
and the ADR amendment it records — is what shipped; this page says which.

## Three kinds of record, and the database that holds each

Every table below is one of three things, and the kind decides both its
database and who may write it:

| Kind of record                         | Authority                                                                       | Database    | Examples                                                                              |
| -------------------------------------- | ------------------------------------------------------------------------------- | ----------- | ------------------------------------------------------------------------------------- |
| Runtime-authoritative fact             | the Data Plane observed it, and nothing else can state it                       | `dataplane` | `requests`, `request_attempts`, `usage_events`, `reservations` (+ allocation legs)    |
| Runtime projection                     | the Control Plane decided it; the Data Plane holds the copy it enforces against | `dataplane` | the API-key credential, the quota projection                                          |
| Control-authoritative financial record | the Control Plane decides and records it                                        | `control`   | `funding_buckets`, `settlements`, `ledger_entries`, and the commerce rows behind them |

The three behave differently under failure, which is why the distinction is
worth stating before any table:

- **A fact is not a copy.** Nothing else holds it, so it cannot be stale and it
  cannot disagree with an authority — it _is_ the authority for what happened.
  The Control Plane derives its records from facts and never rewrites one; a
  correction is a later fact, and the same fact delivered twice is a no-op.
- **A projection is a copy, and it is the only kind that can be wrong without
  the other side knowing.** The runtime reads it on the hot path with the
  Control Plane unreachable, so it is deliberately stale by a bounded window and
  is converged to the ledger by reconciliation (ADR 0006 §8;
  [accounting](accounting.md)).
- **A financial record is derived, not observed.** It is written in the Control
  Plane from the facts the Data Plane published, idempotently by the fact's
  `(request_id, kind class)`, and it is the source of truth for money: the
  runtime's quota projection is not a balance and never becomes one.

The facts reach the Control Plane as a durable pull rather than a push, with an
opaque cursor and a consumer-owned position ([cross-plane
protocols](cross-plane-protocols.md); the wire shape is
`api/openapi/shared/usage-facts.yaml`). That is the only path by which a
`dataplane` row becomes a `control` row.

## The two families, in two databases

```text
timescaledb (one cluster)
│
├── control   ← console-api     the Control Plane's relational rows
│     accounts   users   API-key ownership records
│     plans (+ versions, grant definitions)   subscriptions   entitlements
│     account_payg
│     funding_buckets   settlements   ledger_entries
│     applied_facts   quarantined_facts   ingestion_cursor
│
└── dataplane ← the runtime     the runtime's own rows
      model_aliases (+ candidates)   backends
      alias_group_versions   client_price_list_revisions (+ entries)
      API-key authentication records
      reservations (+ allocation legs)   request_intake
      the quota projection, seeded from Control-Plane grants
      ──── event family (plain tables; see below) ────
      requests   request_attempts   usage_events
```

Each family keeps the shape ADR 0005 gave it; what ADR 0006 adds is that each
table has one owning plane, and that no ordinary query reads across the line. A
plane reads what it holds — the runtime resolves aliases, authenticates keys and
draws down capacity from its own database, never by reaching into the Control
Plane's (ADR 0006 §4, §7).

### Relational family

| Table group                   | Database    | Tables                                                                                                        | Why relational (ADR 0005 placement rule)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| ----------------------------- | ----------- | ------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Identity                      | `control`   | `accounts`, `users`, the API-key ownership records                                                            | Mutable state owned by Identity: who an account is, who may sign in, and which account a key belongs to, who created and revoked it and who may manage it (ADR 0006 §3, §8).                                                                                                                                                                                                                                                                                                                                                                                                            |
| API-key authentication        | `dataplane` | the key's hash record, its runtime active/revoked state and scopes                                            | Mutable state, looked up on every request: the other half of the two-record boundary (ADR 0006 §8). Authentication happens on the hot path, which must resolve a key with the Control Plane unreachable, so the record it authenticates against is the runtime's own.                                                                                                                                                                                                                                                                                                                   |
| Catalog                       | `dataplane` | `model_aliases` (+ candidates), `backends`, `alias_group_versions`, `client_price_list_revisions` (+ entries) | Configured serving state; alias+candidates one transaction (ADR 0001, rule 3); activation is Catalog-local (ADR 0003). The runtime resolves the alias and prices the admission from these rows on every request, and the console's operator reaches them through `dataplane-api` (ADR 0006 §3, §5).                                                                                                                                                                                                                                                                                     |
| Commerce                      | `control`   | `plans` (+ versions + grant definitions), `subscriptions`, `entitlements`, `account_payg`                     | Commercial lifecycle; the due-work lanes write here — the cycle-roll transaction among them — and the capacity a cycle grants reaches the runtime as a published projection (ADR 0006). `account_payg` is the PAYG enablement flag plus its write-once funding-bucket reference; the bucket row itself is the ledger family's. `plans` are unique by operator-facing name (`plans_name_key`) and immutable after creation: renaming is not defined, a new plan is created instead — a Commerce decision, not an ADR 0003 sentence, recorded here so the constraint is not an invention. |
| Accounting ledger             | `control`   | `funding_buckets`, `settlements`, `ledger_entries`                                                            | One consistency unit — bucket capacity and its legs must be written together (invariants 3–4); the bucket is an Accounting aggregate (ADR 0001) and the ledger, with the settlement of record, is the Control Plane's (ADR 0006).                                                                                                                                                                                                                                                                                                                                                       |
| Fact ingestion                | `control`   | `applied_facts`, `quarantined_facts`, `ingestion_cursor`                                                      | The consumer's side of the pull-with-replay seam (ADR 0006 §5, ADR 0010): the applied record is the idempotency key — the fact's `(request_id, kind class)` — that makes a replayed page free; the quarantine is a refusal kept verbatim for an operator; the cursor is the one thing about the feed the Data Plane never learns. Written in the same unit of work as the settlement effects they record and by nothing else.                                                                                                                                                           |
| Runtime reservation and quota | `dataplane` | `reservations` (+ allocation legs), the quota projection                                                      | The runtime's own working set: admission writes the reservation and conditionally draws down the projection in one Data-Plane transaction (ADR 0006).                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| Intake                        | `dataplane` | `request_intake`                                                                                              | Permanent idempotency replay — decided inside the runtime's admission transaction from its own relational state alone, with the Control Plane switched off (ADR 0004, ADR 0006 §4).                                                                                                                                                                                                                                                                                                                                                                                                     |

Notes for the schema designer:

- `funding_buckets` — in `control`, and an Accounting aggregate (ADR 0001):
  one lockable capacity row per entitlement cycle and per
  account PAYG balance (created at account creation for PAYG, at each roll
  for cycles), with cached `settled`/`held`/`available` maintained in
  the same transaction as the legs; rebuildable from `ledger_entries`, and
  wrong if it ever disagrees with them. The bucket row also allocates each
  leg's per-bucket `sequence`. The Commerce entitlement cycle it projects
  references it by identifier, never the reverse. The row the runtime draws
  down at admission is not this one: it is the quota projection in
  `dataplane`, seeded from these grants, written only by the runtime and
  converged to this ledger by reconciliation (ADR 0006).
- `settlements` — one row per settled request (unique `request_id`); the
  double-settlement guard. `ledger_entries` reference their settlement (or
  reservation), bucket, kind, and carry positive amounts; the price snapshot
  (revision + unit prices) is required on `consume` legs, null on the rest.
- `request_intake` — in `dataplane`, because the runtime writes it inside
  its own admission transaction: the row is one per accepted intake, keyed
  `(account_id, idempotency_key)`, and replay decisions read only this table.
  Two write-ordering rules make it the race arbiter rather than a race
  participant. On the **way in**, it is inserted **last**: the unique
  `(account_id, idempotency_key)` key is what admits exactly one in-flight
  intake, so a racing duplicate loses at the constraint and the whole unit of
  work rolls back rather than half-committing. On the **way out**, the row's
  `final_status` pointer is finalised by whichever unit of work closes the
  request — the settlement close, the release compensation, or the reaper's
  expiry — and a NULL pointer means the original is still executing, so a
  replay arriving then is refused with retry-later semantics rather than
  queued behind it. The replay **identity** is engine-immutable: a trigger
  refuses any UPDATE that rewrites `account_id`, `idempotency_key`,
  `request_digest` or `request_id`, and the reason columns exist only under
  the status that names them. Intake rows exist from the admission
  transaction onward, including for requests that transaction itself refuses
  — an unknown alias, an invalid bound, a hold the waterfall cannot secure —
  which are born terminal with `final_status = 'rejected'` and their reason,
  so a replay of the same bytes re-answers the same refusal instead of
  re-running admission. Refusals before that transaction — the account's
  state, the key's grammar, a body the transport could not deliver — write a
  `rejected` request row and no intake row: no digest exists to key a record
  with, so the cheap refusals stay cheap, and the caller simply sends a fresh
  request. A body that is _not JSON_ is not one of them: it is judged inside
  the admission transaction, recorded as the pair, and its key is spent — a
  corrected body under the same key is a different digest and answers 409
  `idempotency_conflict`, so a caller that fixes its request mints a fresh
  key.
  The crash window between admission's COMMIT and the client's answer leaves
  exactly the state the NULL pointer describes — intake row in flight, request
  row `executing`, hold drawn, no candidate ever chosen. That window is not
  papered over: the retrying client is told to retry later for as long as it
  lasts, and when the lease dies the reaper's `expired` close finalises both
  rows, so the next replay re-answers the expiry instead of re-admitting. The
  capacity is never lost by it — a `still-open` reservation the process can no
  longer renew is precisely what the reaper exists to reclaim — and it is
  never double-spent by it, because the second admission of the same bytes is
  the replay the pointer refuses.
  The `request_digest` column is the SHA-256 of the request's **raw** body
  bytes as received, not of any re-serialised or parsed form — two requests
  are the same request iff their bytes are the same bytes, and a field order
  or a whitespace difference is therefore a different digest and an explicit
  conflict, not a silent replay.
- The serving configuration is the runtime's own row rather than a copy of a
  Control-Plane one: the catalogue, the price revisions it prices with and the
  key records it authenticates against all live in `dataplane` and are read
  there on every request (ADR 0006 §3, §8). What does cross as a _projection_
  is the quota capacity the Control Plane grants — seeded from a grant,
  updated only by the runtime, and converged to the ledger by reconciliation.
  A projection is a delivered copy: it joins to nothing in `control`, and the
  engine gives it no way to (ADR 0006 §5, §7).
- Uniqueness constraints this model requires: exactly one reservation per
  request (total, not state-filtered); `settlements.request_id` unique;
  `(settlement, bucket, kind)` unique on ledger legs (split settlement legal,
  duplicate movement not); `(account, idempotency_key)` unique on
  `request_intake`; `(bucket, source, key)` unique on topups/adjustments (a
  redelivered webhook cannot fund twice); `(subscription, cycle,
grant_definition)` unique on entitlements; alias identities unique across
  active and retired (names are never reused).
- Append-only discipline: `ledger_entries` and `usage_events` have no
  UPDATE/DELETE path.
- Amounts are integer minor units in the single platform-wide settlement
  currency (ADR 0003); no per-row currency column exists.
- There is **no `current_plan` column anywhere**, and prices travel as copied
  snapshots (revision + unit prices) on reservations, usage events, and legs —
  never as a live reference alone.

### Event family (in `dataplane`)

All three live in the Data Plane's database. ADR 0005 placed this family on
TimescaleDB hypertables; the landed schema deviates: B7's
`000003_runtime_storage` ships all nine of the runtime schema's tables as
**plain (unpartitioned) tables** — the three below, plus `request_intake`,
`reservations` (+ `reservation_allocations`), the quota projection
(`quota_projections`, `quota_refills`) and `usage_events_stream`. The reasons
and the price of that deviation are the [amendment recorded in ADR
0005](../adr/0005-relational-and-event-storage-split.md); what this page adds
is where the deviation landed. The extension stays installed
(`000001_timescaledb_bootstrap`); its fate is decided with the
retention/compression milestone, not here.

| Table              | Database    | Grain                                   | Written                               | Notes                                                                                                                                                  |
| ------------------ | ----------- | --------------------------------------- | ------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `requests`         | `dataplane` | one per logical request                 | shell at admission; finalised once    | Includes rejected requests (status + reason). Audit anchor for charges.                                                                                |
| `request_attempts` | `dataplane` | one per upstream call                   | as each call completes                | Candidate position, backend, provider request id, latency, error class, commitment outcome.                                                            |
| `usage_events`     | `dataplane` | one (+ corrections) per settled request | as the runtime closes the reservation | Committed attempt, token counts, price snapshot, capture method, a settled amount the domain binds to the fact's own figures; immutable (invariant 1). |

Notes for the schema designer:

- ~~Time-partitioned hypertables~~ **plain tables** — the placement deviation
  above is the decision; hypertable conversion would mean rebuilding the
  uniqueness and foreign-key constraints the money path leans on (ADR 0005's
  amendment).
- `requests.id` is minted at admission; the shell-then-finalize shape is
  intentional (attempts and reservations reference it from birth).
- **Fact ordering is commit ordering** (`usage_events.append_seq`): the
  sequence is allocated from the single `usage_events_stream` row under its
  row lock held to commit, so the order facts become visible is the order
  their appends were serialised in — the feed's ordering guarantee needs no
  timestamp to be trustworthy, and `occurred_at` is telemetry, never order.
  The stream's `epoch` is server-minted at the first append and never by a
  migration, so a database that lost its rows cannot be mistaken for the same
  feed ([cross-plane protocols](cross-plane-protocols.md)).
- **The fact payload is the allocation tail** — the waterfall legs this ending
  closes out, serialised as the `v1` envelope
  `{"allocations":[{"funding_bucket_id",…,"amount",…,"ordinal",…}]}` by
  `apps/dataplane/internal/domain/accounting` (`payloadV1`). Opaque to every
  consumer as a matter of contract — the shape belongs to the writer package —
  but it is not provider telemetry. It is one of the schema's two sanctioned
  jsonb columns (the other is `request_attempts.provider_error`), capped at
  32768 octets by the schema's CHECK and at half that by the writer's domain
  guard. It carries no request or response body, no key, no
  prompt material and no secret; a Control-Plane settlement derives
  consume/release legs from the legs it carries. Anything settlement needs
  beyond the typed columns must be an explicit envelope field, never a
  convention hidden in untyped bytes. The no-bodies rule has exactly one
  sanctioned exception, and it is the other jsonb: `provider_error`
  preserves opaque provider telemetry for debugging, and the executor that
  will populate it (a future milestone) is responsible for redacting request
  material before it reaches the column — B7's guard on it is shape and size
  only.
- A cross-plane reference is by ID, with **no foreign key and no join in
  either direction**. The ordinary cross-plane query is not something review
  has to keep out — PostgreSQL cannot query across databases in one statement,
  so there is no statement to write that would join `usage_events` to the
  settlement it produced or `reservations` to the bucket they drew down
  (ADR 0006 §7). That is an ownership boundary and not a credential one:
  separate databases do not carry separate credentials, they do not stop a
  privileged role from reaching both, and least privilege is not something the
  split produces by itself. Within one database foreign keys are enforced as
  before, and within `control` that includes the legs' reference to their
  bucket and settlement.

### Retention

| Table                                | Database    | Retention                            | Because                                                                                                                                                                              |
| ------------------------------------ | ----------- | ------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `ledger_entries`, `settlements`      | `control`   | forever                              | Billing/audit history; corrections are compensating rows.                                                                                                                            |
| `applied_facts`, `quarantined_facts` | `control`   | forever                              | The idempotency ledger and its refusals: a replayed fact is answered from the applied row, and a quarantine is a record an operator resolves — neither is a state anything ages out. |
| `usage_events`, `reservations`       | `dataplane` | forever                              | The facts the Control Plane settles from and the holds they came from — the same audit value, retained for the same reason.                                                          |
| `requests`                           | `dataplane` | forever                              | One row per request — the anchor joining charges to usage and attempts; volume is already implied by the ledger.                                                                     |
| `request_attempts`                   | `dataplane` | archivable (aggregate, then age out) | Provider telemetry, not customer billing; nothing references it for accounting.                                                                                                      |

"Forever" is a statement about policy rather than a promise about every value a
read can carry. Nothing in this design ages a fact out, and the ingestion cursor
is expected to keep up with the append sequence rather than lag behind it — but
`cursor_expired` (410) exists all the same, because retention is only one of the
ways a stored position can become unplaceable. A position written by another
deployment, one held across a restore from an older backup, or one whose
encoding a future Data Plane has changed all fail the same way, and the answer is
the same in every case: refuse, and make a human re-establish a position rather
than skip forward. Retention being indefinite is what keeps that refusal rare; it
is not what makes it impossible
([cross-plane protocols](cross-plane-protocols.md)).

## Transaction map

The four cross-context coordinated transactions (ADR 0001, rule 6), each now
local to a single plane — settlement being two rows, one per side, because it
is the one the split divides — and nothing else spans contexts. **No
transaction spans the two databases**: each row below writes one database
only, and the planes converge through idempotent facts keyed by ID, not
through a distributed commit (ADR 0006 §5, §7).

| Transaction                                             | Plane                 | Tables written                                                                                                                                                                                                                                                                                                                         | Guarded invariants     |
| ------------------------------------------------------- | --------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------- |
| Admission                                               | Data Plane (local)    | `requests` (shell), `reservations` + allocation legs (insert), the quota projection (conditional drawdown), `request_intake` (insert **last**)                                                                                                                                                                                         | 3, 6, 10               |
| Settlement — the runtime's close                        | Data Plane (local)    | `usage_events` (append), `reservations` (close, state-guarded), `request_attempts` (probe, then insert), `requests` (finalise), `request_intake` (terminal pointer)                                                                                                                                                                    | 1, 6, 7                |
| Settlement — settlement from the fact                   | Control Plane (local) | `applied_facts`/`quarantined_facts` (one row per fact — an interpretable fact records its application, a refusal quarantines verbatim), `settlements` (insert), `ledger_entries` (`hold`/`consume`/`release` legs), `funding_buckets` (projections), `ingestion_cursor` (advance **last**)                                             | 2, 4, 8, 10            |
| Release (explicit compensation, or the reaper's expiry) | Data Plane (local)    | `reservations` (close, state-guarded), the quota projection (capacity returned, leg by leg), `requests` (finalise with failure reason), `request_intake` (terminal pointer), `usage_events` (append, the `released`/`expired` fact — **last**)                                                                                         | 3, 6                   |
| Cycle roll                                              | Control Plane (local) | `entitlements` (create) + `subscriptions` (cycle fields) — keyed `(subscription, cycle)`; `funding_buckets` (create) and the `ledger_entries` `grant` legs join the same unit of work at settlement (B6) — the commerce foundation ships the Commerce half. The new capacity then reaches the runtime's projection as a published fact | 3 (grant exactly once) |

The release row is one unit of work, and its internal order is not a matter of
taste. It **enters** through the reservation's close — a compare-and-set on
the open state, so exactly one of the releaser and the settler ever wins a
given reservation — and the capacity return is **unconditional** once that
CAS has won it: running the giveback outside the winning close would mint
capacity, never return it, so the return belongs to the winner and to nobody
else. From there the legs are returned in their stored waterfall order, the
request and its intake pointer are finalised, and the fact — already
described below as the last statement — closes the unit. The reaper's
`expired` close is the same unit with a different trigger and failure reason:
one composition, two doors into it.

The two settlement rows are one logical settlement split by the plane that
owns each row, not one transaction failing: the runtime closes its reservation
and writes the usage fact in the Data Plane, and the Control Plane reads that
fact and writes the settlement, its ledger legs and its bucket projections
from it — together with the applied record that makes a redelivery a no-op,
keyed by the fact's `(request_id, kind class)` — while the cursor's advance
is the same unit's last write, so the position never claims work that did not
commit (ADR 0010). The window between the two is a named,
bounded property, and the unique settlement per request is unchanged — the
customer is charged exactly once (ADR 0006). Admission and release are
Data-Plane transactions for the same reason, and their `hold` and `release`
legs are Control-Plane rows written from the reservation as a fact: the
runtime writes no ledger row at all (ADR 0006 §3, §7). The release row's fact
append is the crash-consistency rule made concrete: a compensation and the
fact that reports it commit or vanish together, and the pairing is a property
of the callers' unit-of-work discipline, not of a constraint — the append is
the **last** statement of the close's unit of work
([accounting](accounting.md)), so a crash in the window rolls the whole unit
back and leaves the reservation still open, never a terminal state without its
fact. A fact with no reservation behind it is refused by that same discipline
— the store refuses an append whose context carries no unit of work at all,
because a sequence allocated outside the unit it belongs to is a fact that
commits before the close it reports — and the pairing is caught by the
integration suite (`TestIntegrationSettlementUnitIsAllOrNothing`), not by a
schema guard — no constraint enforces the fact↔reservation pairing, because
`usage_events` references `requests`, not `reservations`. The reaper's sweep
is held to the same posture from its own side: `ExpireLapsedLeases` refuses a
context that carries no unit of work, because a bare sweep's closes would
commit one by one under autocommit, and a crash before the caller's unit
opened would leave closed holds the feed never hears about.

Catalog configuration activation (aliases, group versions, price revisions)
is a coordinated transaction internal to the Catalog context (ADR 0003) —
Catalog-local, not a fifth cross-context transaction; it never spans
contexts, though it does coordinate more than one Catalog aggregate, and its
rows are the Data Plane's, so it is local to that database as well (ADR 0006
§3, §7). The
same is true of the capacity-funding and correction flows of ADR 0004
(`topup`; `adjustment`; a usage correction's compensating legs): with
funding buckets Accounting-owned (ADR 0001) they coordinate the bucket and
the ledger inside one context — a correction that also supersedes a usage
event references it by ID rather than writing it, because the event is the
Data Plane's row (ADR 0006) — so they too are context-local rather than
members of the cross-context set. Apart from the four transactions above and
those named context-local coordinated transactions, every transaction is
single-aggregate — and nothing else crosses a context boundary at all:
account creation, for instance, is a choreography over IDs, not a
coordinated transaction (the account row is an Identity write, and the
zero-balance PAYG bucket it promises (ADR 0003) is its own Accounting
write in the same workflow, both of them `control` writes). The map is not
exhaustive of every write: the topup/adjustment flows each append their own
ledger legs under their own uniqueness guards.

## What must be derivable from this page alone

A schema designer should be able to produce, without further business
decisions:

1. the table list above, with database, grain, family, and retention for
   each;
2. the uniqueness constraints named here;
3. the append-only discipline for `ledger_entries` and `usage_events`;
4. that no `current_plan` column exists anywhere;
5. that prices travel as snapshots (revision + unit prices), never as a live
   reference alone;
6. that settlement is **two transactions in two databases, not one**: the
   runtime writes the usage fact and closes its reservation in the Data
   Plane, and the Control Plane writes the settlement, its ledger legs and
   its bucket projections from that fact, idempotently by `request_id`. The
   window between a committed runtime reservation and its Control-Plane
   settlement is a **named, bounded property**, not an impossibility, and the
   ledger — never the runtime's quota projection — is the source of truth for
   money (ADR 0006). There is no outbox either: the usage fact is the
   authority the charge is derived from, written by the process that observed
   it, and it reaches the Control Plane over the replayed feed rather than by a
   push ([cross-plane protocols](cross-plane-protocols.md)). The fact must
   carry the allocation and bucket identities and the immutable reservation
   figures needed to derive the consume legs, the release legs and the settled
   total, so that a settlement needs no synchronous call back into the Data
   Plane;
7. that replay decisions read only `request_intake` — never the event
   family — and that amounts are integer minor units in one platform
   currency.

Anything the schema needs that this page (or an ADR) does not supply is a
**missing decision**, to be raised as an issue — not filled in silently.
