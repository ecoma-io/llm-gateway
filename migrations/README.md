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

Only the Data Plane's lane exists so far, and that is the honest state of a
scaffold whose Control Plane schema has not been designed: the Control
Plane's database is created and empty, and its lane appears with the first
Control Plane domain that needs a table. An empty lane directory would be
worse than a missing one — golang-migrate refuses a lane with no files
(`first .: file does not exist`), so an empty `migrations/control/` would
read as a working lane that fails the moment anyone used it.

A migration belongs to exactly one lane, and its lane decides the database it
is applied to: the runner's lane argument points `-path` and `-database` at
the same name (`deploy/postgres/compose.yaml`), so a file cannot be applied
to the other plane's database by editing a path. That is what makes
"no application reads the other plane's tables" a property of the deployment
rather than a promise: PostgreSQL refuses a query that spans databases, so
there is no query to write.

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
