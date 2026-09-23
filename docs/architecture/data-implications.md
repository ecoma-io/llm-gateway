# Data implications

Reference page mapping the domain model onto the intended storage split. The
decision is [ADR 0005](../adr/0005-relational-and-event-storage-split.md):
**one PostgreSQL cluster; mutable authoritative state is relational; immutable
time-series facts are Timescale-oriented event tables (hypertables) in that
same cluster.** This page creates no migrations — it is the constraint the
future schema PR derives from.

## The two families

```text
PostgreSQL relational tables                        Timescale-oriented event tables
────────────────────────────────                    ───────────────────────────────
accounts   users   api_keys                         requests
plans      subscriptions   entitlements             request_attempts
           funding_buckets                          usage_events
client_price_list_revisions   alias_group_versions
model_aliases (+ candidates)   backends
reservations (+ allocation legs)
settlements   ledger_entries
request_intake
```

### Relational family

| Table group            | Tables                                                                                            | Why relational (ADR 0005 placement rule)                                                                                     |
| ---------------------- | ------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------- |
| Identity               | `accounts`, `users`, `api_keys`                                                                   | Mutable state; hot-path lookups; revocation must be immediate.                                                               |
| Catalog                | `model_aliases` (+ candidates), `backends`, `alias_group_versions`, `client_price_list_revisions` | Configured state; alias+candidates one transaction (ADR 0001, rule 3); configuration activation is Catalog-local (ADR 0003). |
| Commerce               | `plans` (+ versions), `subscriptions`, `entitlements`, `funding_buckets`                          | Lifecycle and guarded capacity; admission and cycle-roll transactions write here.                                            |
| Accounting working set | `reservations` (+ allocation legs), `settlements`, `ledger_entries`                               | Must be transactional with bucket capacity and each other (invariants 3–4).                                                  |
| Intake                 | `request_intake`                                                                                  | Permanent idempotency replay — decided inside the admission transaction from relational state alone (ADR 0004).              |

Notes for the schema designer:

- `funding_buckets` — one lockable capacity row per entitlement cycle and per
  account PAYG balance (created at account creation for PAYG, at each roll
  for cycles), with cached `settled`/`held`/`available` maintained in
  the same transaction as the legs; rebuildable from `ledger_entries`, and
  wrong if it ever disagrees with them. The bucket row also allocates each
  leg's per-bucket `sequence`.
- `settlements` — one row per settled request (unique `request_id`); the
  double-settlement guard. `ledger_entries` reference their settlement (or
  reservation), bucket, kind, and carry positive amounts; the price snapshot
  (revision + unit prices) is required on `consume` legs, null on the rest.
- `request_intake` — one row per accepted intake, keyed
  `(account_id, idempotency_key)`; replay decisions read only this table.
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

### Event family (hypertables, same cluster)

| Table              | Grain                                   | Written                            | Notes                                                                                       |
| ------------------ | --------------------------------------- | ---------------------------------- | ------------------------------------------------------------------------------------------- |
| `requests`         | one per logical request                 | shell at admission; finalised once | Includes rejected requests (status + reason). Audit anchor for charges.                     |
| `request_attempts` | one per upstream call                   | as each call completes             | Candidate position, backend, provider request id, latency, error class, commitment outcome. |
| `usage_events`     | one (+ corrections) per settled request | in the settlement transaction      | Committed attempt, token counts, price snapshot, capture method; immutable (invariant 1).   |

Notes for the schema designer:

- Time-partitioned hypertables (plain time partitioning is the fallback when
  the extension is absent — the domain is indifferent, ADR 0005).
- `requests.id` is minted at admission; the shell-then-finalize shape is
  intentional (attempts and reservations reference it from birth).
- Cross-family references are by ID without enforced foreign keys (accepted,
  ADR 0005); within the relational family, foreign keys are enforced.

### Retention

| Table                                                           | Retention                            | Because                                                                                                          |
| --------------------------------------------------------------- | ------------------------------------ | ---------------------------------------------------------------------------------------------------------------- |
| `ledger_entries`, `settlements`, `usage_events`, `reservations` | forever                              | Billing/audit history; corrections are compensating rows.                                                        |
| `requests`                                                      | forever                              | One row per request — the anchor joining charges to usage and attempts; volume is already implied by the ledger. |
| `request_attempts`                                              | archivable (aggregate, then age out) | Provider telemetry, not customer billing; nothing references it for accounting.                                  |

## Transaction map

The four cross-context coordinated transactions (ADR 0001, rule 6) — and
nothing else spans contexts:

| Transaction | Tables written                                                                                                                                                               | Guarded invariants     |
| ----------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------- |
| Admission   | `request_intake` (insert), `requests` (shell), `reservations` + allocation legs (insert), `funding_buckets` (conditional capacity), `ledger_entries` (`hold` legs)           | 3, 6, 10               |
| Settlement  | `settlements` (insert), `usage_events` (append), `ledger_entries` (`consume`/`release` legs), `funding_buckets` (projections), `reservations` (close), `requests` (finalise) | 1, 2, 4, 6, 7, 8, 10   |
| Release     | `ledger_entries` (`release` legs), `funding_buckets` (projections), `reservations` (close, state-guarded), `requests` (finalise with failure reason)                         | 3, 6                   |
| Cycle roll  | `entitlements` + `funding_buckets` (create), `ledger_entries` (`grant` legs), `subscriptions` (cycle fields) — keyed `(subscription, cycle)`                                 | 3 (grant exactly once) |

Catalog configuration activation (aliases, group versions, price revisions)
is a coordinated transaction internal to the Catalog context (ADR 0003) —
Catalog-local, not a fifth cross-context transaction; it never spans
contexts, though it does coordinate more than one Catalog aggregate. Apart
from the four transactions above and Catalog activation, every transaction
is single-aggregate. The map is not exhaustive of every write: the
topup/adjustment ledger flows of ADR 0004 are capacity-funding writes
outside the four, each appending its own ledger legs under its own
uniqueness guards.

## What must be derivable from this page alone

A schema designer should be able to produce, without further business
decisions:

1. the table list above, with grain, family, and retention for each;
2. the uniqueness constraints named here;
3. the append-only discipline for `ledger_entries` and `usage_events`;
4. that no `current_plan` column exists anywhere;
5. that prices travel as snapshots (revision + unit prices), never as a live
   reference alone;
6. that settlement, its usage event, and its legs are one atomic transaction
   on one cluster — no outbox in the accounting path;
7. that replay decisions read only `request_intake` — never the event
   family — and that amounts are integer minor units in one platform
   currency.

Anything the schema needs that this page (or an ADR) does not supply is a
**missing decision**, to be raised as an issue — not filled in silently.
