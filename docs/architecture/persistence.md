# Persistence

Reference page for the persistence foundation: the two databases, the two
migration lanes, and the conventions every future schema change keeps. The
decisions behind it are
[ADR 0005](../adr/0005-relational-and-event-storage-split.md) — the two
storage families — and
[ADR 0006](../adr/0006-control-plane-and-data-plane.md) — the two databases,
one per plane; those records win over this page wherever the two disagree.
Which table lands in which database is
[data implications](data-implications.md)' mapping, with a reason per row in
[planes and ownership](planes.md); how an owning application reaches its
database in code is [ports and adapters](ports.md). This page carries what all
three assume: where the schema lives, who moves it forward, and what a table
must look like when it arrives.

## Two databases, one owner each

One TimescaleDB deployment, two databases, explicitly owned
([ADR 0006](../adr/0006-control-plane-and-data-plane.md) §7):

| Database    | Owner                                      | What it holds                                                                                                                                                                                                                  |
| ----------- | ------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `control`   | `apps/console-api` — the Control Plane API | the Control Plane's relational family and nothing else: identity, commerce, and the accounting working set ([data implications](data-implications.md))                                                                         |
| `dataplane` | `apps/dataplane` — the Data Plane runtime  | the runtime's own relational rows **and** the event family — the only database that carries the TimescaleDB extension (ADR 0006 §7), whose event tables landed plain rather than as the hypertables ADR 0005 first placed here |

Where the extension lives is not an implementation detail. The high-volume
time-series facts are the runtime's, so the extension belongs to the runtime's
database and the Control Plane's database stays
relational tables only (ADR 0006 §7); a migration that enabled `timescaledb`
in `control` would be applying a decision nobody made. The hypertables that
came with the extension in ADR 0005's original wording, though, never landed:
the runtime schema's tables are plain, by that ADR's B7 amendment.

`dataplane-api` is the third Go application and owns no database at all. It is
a management façade and a transport — no persistence adapter, no persistence
port, no state of its own — and its architecture tests hold that shape: the
only thing permitted under its outbound trees is the seam through which it
reaches the Data Plane (ADR 0006 §9;
[ports and adapters](ports.md)). A table behind the management surface would
make it a second persistence owner, which is the outcome §9 exists to prevent.

What the two databases give — separate namespaces, separate connection
targets, independent transactions and independent migration history, and no
ordinary cross-database SQL reference — and what they do not — separate
credentials, a stop against a privileged role — is stated once in ADR 0006 §7.
It is why this page has a [privilege model](#the-privilege-model) section at
all: the split is an ownership boundary, and least privilege is granted, never
inherited from it.

## Two lanes, one per database

`migrations/<plane>/` is the layout, and the lane directory **is** the
database: the runner's lane argument points `-path` and `-database` at the
same name, so a file cannot be applied to the other plane's database by
editing a path ([migrations/README.md](../../migrations/README.md)). A
migration is a hand-authored pair — `NNNNNN_name.up.sql` beside
`NNNNNN_name.down.sql` — numbered one past the highest pair in **that lane**,
because the version the runner records lives in the database it migrated, so
each lane numbers from `000001` independently. It is append-only: an applied
migration is immutable history, and a correction is a new migration, never an
edit to a file the runner has already recorded.

Both lanes exist, and neither is empty: the Control Plane's first pair
establishes its ownership namespace and nothing else, so the schema change
that lands the plane's first table arrives on a lane the suite has already
exercised end to end — applied, re-applied, rolled back
([migrations/README.md](../../migrations/README.md)).

One namespace decision is already made, and a future migration is the place it
would be quietly lost, so it is written here. Within the Control Plane's
database, tables keep to the `control` schema — the namespace
[ADR 0006](../adr/0006-control-plane-and-data-plane.md) §5 already spells in
`control.usage_ingestion_cursor`. The Data Plane's database stays on the
default namespace, which is where its bootstrap pair puts the extension and
where the verify suite asserts that lane's applied history lands
(`deploy/postgres/verify.sh`).

## Where migrations run

The runner is golang-migrate, from its pinned image, as a compose service in
`deploy/postgres/compose.yaml` — never a host-installed binary, so a
contributor needs Docker and nothing else, and the version that migrates a
database is the version written in that file
([deploy/postgres/README.md](../../deploy/postgres/README.md)). One variable,
`GATEWAY_MIGRATE_LANE`, names both the directory the runner reads and the
database it writes; that is what makes "a migration is applied to the database
its plane owns" a property of the compose file rather than a property of
whoever typed the command.

What the runner guarantees — one file delivered as one transaction, an
explicit dirty state, an advisory lock against two concurrent migrators, and a
rollback that is authored and then proven — is the safety model that
directory's README records, and the verify suite executes it rather than
asserting it: apply, validate, roll back, and recover from a deliberately
failed migration, against the real database. Contributors run
`bash deploy/postgres/verify.sh` and CI runs the same command; there is no
second definition of green.

## Applications never migrate

None of the three Go applications runs migrations, at startup or ever.
Migration is a deployment act with its own role: the runner brings the schema
forward, and the role that owns schema is a deployment role
([the privilege model](#the-privilege-model)), not a process that happens to
need a table. What an application contributes to that order is its startup
binding below — it validates its connection and refuses to start unless its
database answers as its own — so a half-deployed release fails loudly at
startup instead of serving against whatever it finds.

## Connection ownership (startup binding)

Each owning application binds to its plane's database at startup, and the
binding is strict in three directions:

- **The configuration names its plane's database, and is checked.** An owning
  application validates its configuration against its plane's database at
  load time; a DSN naming the other plane's database is a startup failure
  that states the ownership boundary, not a connection that fails later,
  mid-request. The adapter that opens the pool holds the same rule — it is
  the last place a foreign DSN could pass through — so the binding is
  enforced at load and again at open, and neither copy can be removed
  without the other noticing.
- **The pool is opened and pinged once, at startup.** An owning application
  refuses to start when its database does not answer
  ([ports and adapters](ports.md) puts that lifecycle where it belongs — the
  composition root). A process that came up without its database and degraded
  request by request would turn one deployment mistake into client-visible
  failures.
- **The pool closes after the servers drain.** The database connection is
  process infrastructure exactly as the listeners are: opened before they
  serve, closed after they drain.

The asymmetry between the planes is the point of the rule. The runtime's hot
path never depends on the Control Plane or on `dataplane-api` (ADR 0006 §4),
and its own database is what makes intake, reservations and usage durable —
which is why the runtime also refuses to start without it. A runtime that
started databaseless would not be degraded; it would have nowhere to record
the requests it served.

## The transaction boundary

Per plane, per pool. Each owning application holds its own pool and its own
`persistence.Store` — the port's two copies are deliberately two copies, one
per module, each wired to its own plane's database ([ports and
adapters](ports.md); ADR 0006 §7). `WithinTx` scopes a unit of work to the
pool that opened it: a nested scope on the same store joins the transaction
already in flight, and a store backed by a different pool never joins one of
another pool's units of work — it begins, owns and commits a transaction of
its own. That is what keeps a transaction and the database it writes from ever
being decided apart, and it is why "which store did this query land on" is not
a question review has to ask.

No transaction spans the two databases, and nothing in this repository needs
to enforce that, because the engine already does: a cross-database statement
is not one PostgreSQL parses (ADR 0006 §7). What holds the planes together is
the convergence contract instead — idempotent facts keyed by ID, `request_id`
for the facts the Control Plane settles from, the entity's own identifier for
the commands the Data Plane applies — never a distributed commit
([cross-plane protocols](cross-plane-protocols.md); ADR 0006 §5).

## The privilege model

Per plane, two roles. An **application role** that can connect to its plane's
database and read and write that plane's data — and no DDL — and a
**migration role** that owns the schema and is the only writer of migrations.
No application role holds migration rights, and `dataplane-api` holds no
database role at all. The split is the point, and it is the reason the model
has to be stated rather than assumed: two databases are an ownership boundary
and **not** a credential boundary (ADR 0006 §7). The boundary the engine draws
stops the ordinary query and nothing else — it does not stop a privileged role
from reaching both databases, it does not stop an FDW or dblink-style path,
and it produces no least privilege by itself. Roles are what make least
privilege real, because least privilege is granted, never inherited from a
split.

The local fixture does not model this, and says so: one convenient role owns
and opens both databases, and the verify suite asserts that fact, so the
documentation cannot drift back into a stronger claim.
[deploy/postgres/README.md](../../deploy/postgres/README.md) is the exact
contract — the fixture's one role, the production guidance, and the privileged
cross-plane mechanism it rules out — and this page does not restate it.

## Adding a migration for a future domain

One pair, in its plane's lane, numbered one past that lane's highest pair.
Which lane is not a judgement call: the lane is the database, and the database
is the plane that owns the rows ([planes and ownership](planes.md)). Ordering
follows the aggregate dependencies
[ADR 0005](../adr/0005-relational-and-event-storage-split.md) names —
identity → catalog → commerce → accounting → events — so a table another
references arrives before the one that references it, within a lane as well as
across the model. The down file is part of the deliverable rather than an
afterthought: the suite executes every down file against the real database,
and a rollback that has never run is a guess.

One co-edit belongs to the same diff: the verify suite pins the exact set of
tables a lane's database holds — the control-database assertions in
`deploy/postgres/verify.sh` count and name them — so a migration that adds a
table updates those assertions in the same reviewed change. The suite's own
comment says it and this page repeats it: replaced, never silenced.

## Conventions for future schema work

The conventions later phases follow. Each is grounded in an ADR where an ADR
decides it, and named as this foundation's decision where none does. The
uniqueness constraints the model requires are the ones
[data implications](data-implications.md) already names; they are not repeated
here, and none is invented here.

| Convention         | Rule                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| ------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Names              | snake_case identifiers and plural table names; a deviation states its reason in the migration that makes it                                                                                                                                                                                                                                                                                                                                             |
| Primary keys       | UUID version 7 for any row whose identity crosses a plane or reaches a client; deviations require a stated reason. The stated deviation: the Control Plane's API-key id is UUIDv4, because the id is the token's prefix material and the token grammar pins the v4 form (`identity`'s `validateUUIDForm`; the `api_keys_prefix_shape` CHECK enforces it in the schema) — the one exception, and every later plane-crossing or client-facing id mints v7 |
| Lifecycle          | explicit state machines carrying the ADRs' exact state vocabulary — `pending` \| `active` \| `suspended` \| `cancelled` \| `expired` for a subscription, `open` \| `settled` \| `released` \| `expired` for a reservation — and no soft-delete columns: deletion policy is the retention policy (ADR 0005)                                                                                                                                              |
| Append-only tables | `ledger_entries` and `usage_events` have no `UPDATE` or `DELETE` path (ADR 0004, invariants 1–2)                                                                                                                                                                                                                                                                                                                                                        |
| JSONB              | opaque provider telemetry only — never a load-bearing, queryable field; everything the model requires to be queryable is a column. One sanctioned second category: per-candidate provider parameter overrides — opaque to the gateway, never queried, validated by the domain and passed through to the provider at execution                                                                                                                           |
| Enumerated states  | `text` plus a `CHECK` constraint — migration-friendly where an `ENUM` type is not — carrying the value sets the ADRs name                                                                                                                                                                                                                                                                                                                               |
| Foreign keys       | within one database only, and within it up to the family boundary in both directions, as [ADR 0005](../adr/0005-relational-and-event-storage-split.md) states; none across planes in either direction (ADR 0006 §7)                                                                                                                                                                                                                                     |
| Constraint names   | `<table>_<columns>_idx`, `<table>_<columns>_key`, `<table>_<column>_fkey` — PostgreSQL's own defaults, kept rather than overridden; a CHECK names `<table>_<what>_<rule>` in the identity migration's vocabulary (`_state_valid`, `_length`, `_shape`, `_consistency`), and a qualifying infix is kept when it carries the rule's one subtlety (the `live` of `users_account_live_email_key`, the live-only partial index)                              |

And the rule that closes the list, because it is what keeps the list honest:
anything the schema needs that the pages above do not supply — a column type
nobody named, a state the ADRs do not carry, a constraint the model does not
require — is a **missing decision**, raised as an issue rather than filled in
silently ([data implications](data-implications.md)). These conventions decide
shape, never semantics; a migration that had to invent a semantic to compile
is a migration that made a decision nobody accepted.
