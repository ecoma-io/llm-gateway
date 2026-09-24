# Migrations

Schema for this repository's two databases, as ordered, reviewed
`.up.sql`/`.down.sql` pairs applied by golang-migrate. There is no ORM here
and no auto-migration: a schema change is a diff a human reads. How the
runner works, why this tool was chosen, and what the safety model guarantees
are in [`deploy/postgres/README.md`](../deploy/postgres/README.md).

## One directory per plane

The split is the database split (ADR 0006 §7), not a filing convention:

| Lane                    | Database    | Owner                                      |
| ----------------------- | ----------- | ------------------------------------------ |
| `migrations/control/`   | `control`   | `apps/console-api` — the Control Plane API |
| `migrations/dataplane/` | `dataplane` | `apps/dataplane` — the Data Plane runtime  |

Both lanes exist. The Control Plane's first pair establishes its ownership
namespace, and nothing else: ADR 0006 §5 writes the Control Plane's future
position table as `control.usage_ingestion_cursor` — schema-qualified — so
Control Plane migrations keep their domain tables in the `control` schema,
while the ADRs name the Data Plane's tables bare (`usage_events`,
`request_intake`) and its lane stays on the default namespace. The namespace
migrates first, on purpose: the schema change that lands the first Control
Plane table should arrive on a lane the suite has already exercised end to
end — applied, re-applied, rolled back — rather than ask an unexercised
pipeline to prove itself while carrying real schema. (Until that first pair
landed, the lane was absent rather than empty: golang-migrate refuses a lane
with no files (`first .: file does not exist`), so an empty
`migrations/control/` would have read as a working lane that failed the
moment anyone used it.)

A migration belongs to exactly one lane, and its lane decides the database it
is applied to: the runner's lane argument points `-path` and `-database` at
the same name (`deploy/postgres/compose.yaml`), so a file cannot be applied
to the other plane's database by editing a path. What that gives is a
**database ownership boundary** — separate namespaces, separate connection
targets, independent transactions and migration history, and no ordinary SQL
statement spanning the two. It is not a **security / credential boundary**:
one role owns both databases today, which is a fixture convenience and not a
security property, and
[`deploy/postgres/README.md`](../deploy/postgres/README.md) states the
production privilege contract this fixture deliberately does not provision.

The consequence to remember when a schema change spans both planes: there
isn't one. A fact one plane owns is written by that plane, and the other
learns it as data — a cross-plane `JOIN` is not an available answer.

## Adding a migration

Author a pair in the lane that owns the schema — creating the lane directory
if this is the plane's first file — named `NNNNNN_<snake_case_name>` with the
number one past the highest pair in **that lane** (golang-migrate records
versions in the database it migrates, so the two lanes number independently,
each from `000001`):

```text
migrations/dataplane/000002_<name>.up.sql    # what this change applies
migrations/dataplane/000002_<name>.down.sql  # its exact inverse — proven
```

The down file is not optional and not ceremonial: the suite executes every
down file against the real database, and an untested rollback is a guess.
Migrations are append-only — an applied migration is immutable history, and
a correction is a new migration, never an edit to a file the runner has
already recorded.

Run the suite after adding one — `bash deploy/postgres/verify.sh` — which
applies, asserts, rolls back, and recovers from a deliberately failed
migration, against the pinned TimescaleDB.
