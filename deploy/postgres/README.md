# Local PostgreSQL / TimescaleDB

The gateway's authoritative primary store, as a developer runs it: one
compose file, pinned images, one command for everything. What a contributor
runs locally and what an integration lane runs are this same file and this
same suite — there is no second definition of green.

The store is PostgreSQL with the
[TimescaleDB](https://docs.timescale.com/) extension: transactional tables
for the OLTP domains, event tables for the time-series workloads. Which
tables belong to which family is not decided here —
[ADR 0005](../../docs/adr/0005-relational-and-event-storage-split.md) set
the placement rule, and every future migration applies it. No table is a
hypertable today: the runtime schema's tables landed as plain tables, by
that ADR's B7 amendment. The extension belongs to the
`dataplane` database, because the high-volume time-series facts are the
runtime's; the Control Plane's database is relational tables only
([ADR 0006 §7](../../docs/adr/0006-control-plane-and-data-plane.md)).

**One cluster, two databases.** `control` belongs to the Control Plane API
(`apps/console-api`) and `dataplane` to the runtime (`apps/dataplane`) — the
split [ADR 0006 §7](../../docs/adr/0006-control-plane-and-data-plane.md)
decides. It is not filing: it is a **database ownership boundary** — which
tables exist where, and which process may read them, is decided by which
database they are in — and that is a different thing from a **security /
credential boundary**, which this fixture does not have. What two databases do
and do not give, stated once so nothing below has to imply more:

```text
Two databases give:  separate namespaces · separate connection targets ·
                     independent transactions · independent migration history ·
                     no ordinary cross-database SQL reference

They do NOT give:    separate credentials · no privileged cross-database access ·
                     no FDW/dblink-style access · production-grade least privilege
```

The two migration lanes mirror the two databases exactly, and the lane
directory is the database name — `migrations/control/`, `migrations/dataplane/`
— so a file's directory is also the deployment decision about where it runs;
`migrations/README.md` states the rule that ties the two together, and why the
Control Plane's lane opens on a foundation migration — its ownership
namespace — rather than a business table, with the identity foundation
(000002) as the first business schema inside it. Both databases are created by
[`initdb/`](initdb/10-create-plane-databases.sh), on the first start of an
empty volume.

## Decision record

Pinned as of 2026-09-23:

| What             | Pin                                                                                                                    | Why this one                                                                               |
| ---------------- | ---------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------ |
| PostgreSQL       | **18** via `timescale/timescaledb:2.30.1-pg18@sha256:9dede0e3ccc071cf71935b17f76bf243331df0b1575338c8ac294640fcf12a36` | TimescaleDB 2.30.1 (2026-09-17) supports PG 16/17/18; 18 is the newest. Community edition. |
| Migration tool   | **golang-migrate** `migrate/migrate:v4.20.1@sha256:76cc2074cb6642631f34a898ced71e6aeaa6b1a4d78c4daa743275a22e0c5be7`   | See the comparison below.                                                                  |
| Migration format | Sequential pairs under [`migrations/<plane>/`](../../migrations): `000001_name.up.sql` + `000001_name.down.sql`        | Authored, reviewable, one file per direction; one lane per database.                       |

The tag is readable and the digest is what actually resolves: a re-pull
cannot silently move either image. Both are multi-arch manifest lists, so an
arm64 workstation and the amd64 CI runners pull the same pin.

### Why golang-migrate, not Atlas (and not goose or dbmate)

Evaluated on the dimensions this foundation actually has to survive —
versioned migrations, integrity validation, transaction behavior, locking,
destructive-change safety, CI validation, PostgreSQL/Timescale support, local
developer experience:

| Dimension                  | golang-migrate v4.20.1                                                                               | Atlas v1.3.x                                                                                                         |
| -------------------------- | ---------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------- |
| Versioned migrations       | Paired `.up.sql`/`.down.sql`, sequential versions, `schema_migrations(version, dirty)`               | Timestamped files + `atlas.sum` + revision table                                                                     |
| Integrity validation       | **None** — no checksums (see the accepted gap below)                                                 | Directory checksums + `atlas migrate validate`; applied-file editing detected only if `atlas.sum` is not regenerated |
| Transaction behavior       | Whole file = one PostgreSQL implicit transaction by default; `dirty` flag written outside it         | `--tx-mode=file` default (one tx per file), `all`/`none` available                                                   |
| Locking                    | `pg_advisory_lock` keyed on database/schema/table, 15 s CLI timeout                                  | Session advisory lock, 10 s default, configurable                                                                    |
| Destructive-change safety  | None built in — review policy + this suite                                                           | `atlas migrate lint` flags drops/rewrites — **but has required an Atlas Pro license since its introduction**         |
| CI validation              | Apply + assert + roll back against a real database (this suite)                                      | `validate`/`status`/`dry-run` (lint is Pro)                                                                          |
| Down migrations            | **First-class, authored** `.down.sql` per migration; `down -all`, `goto`                             | Primarily **computed** from a dev database; authored downs need txtar sections                                       |
| Local developer experience | One binary or one image; the verbs this repository uses (`create`, `up`, `down`, `version`, `force`) | Powerful but a larger surface: dev databases, `atlas.sum` hygiene, HCL optionality                                   |

golang-migrate is the smaller machine that does everything this repository
has committed to: authored SQL pairs a human reviews, an explicit dirty
state, advisory locking, and rollback that is _authored and tested_ rather
than computed. Atlas lost on the same facts that make it impressive — its
down flow plans reversals dynamically instead of shipping the file pair the
reviewer read, its destructive-change lint moved behind a Pro license, and
its newest version was distributed to Docker Hub and the binary endpoint
without a matching public GitHub release — provenance this repository's
pinning discipline cannot verify. `pressly/goose` (v3.28.0, active) and
`amacneil/dbmate` (v2.36.0, active) are credible one-file-format
alternatives; the paired-file model and the verified PostgreSQL dirty/lock
behaviour kept golang-migrate ahead.

### The migration safety model

- **Atomicity.** By default each migration file reaches PostgreSQL as one
  simple query — one implicit transaction: a failed migration leaves no
  half-applied schema. `x-multi-statement=true` would break that property
  and is never to be enabled here. The suite executes the property: it runs
  a migration whose first statement succeeds and whose second fails, and
  proves the first left nothing behind.
- **Dirty state.** The tool writes `dirty=true` before running a migration
  and `dirty=false` after; the write is outside the migration's transaction,
  so a failure leaves the version recorded with `dirty=true` and every later
  migration refuses to run — both facts proven by the same suite step.
  Recovery is deliberate, never automatic:
  reconcile the real schema, then `migrate force <the version that is
actually applied>` — for a transactionally rolled-back failure that is the
  _previous_ version, so the repaired migration runs again.
- **Concurrent migrators.** Two `up`s cannot interleave: the PostgreSQL
  driver holds an advisory lock for the duration and waits (up to the 15 s
  CLI timeout) rather than racing.
- **Immutability.** An applied migration is immutable history. Editing,
  renaming or deleting one is never the fix — a corrective migration is. The
  tool records versions, not contents, so nothing else would catch a silent
  edit.
- **Rollback is authored and proven.** Every `.up.sql` ships with a
  `.down.sql`, and `verify.sh` applies and fully rolls the schema back on a
  real database — a down file that was never executed against PostgreSQL is
  not a rollback, it is a guess.
- **Accepted gap.** No checksum verification: nothing in the tool compares
  applied history against the files that produced it. The mitigations are
  the immutability rule above, review, and this suite — whose opening step
  proves each lane's shape (versions numbered without a gap or duplicate,
  one pair per version, nothing else in the directory), which is the drift a
  directory listing can catch; the content of an applied file stays outside
  every check. If silent-edit detection or destructive-change linting ever
  becomes a requirement, re-evaluate Atlas (checksummed directories,
  Pro-licensed lint) — that is a deliberate escalation, not a default.

The tool runs from its pinned image as a compose service — never a
host-installed binary — so a contributor needs Docker and nothing else, and
the version that migrates a database is the version written in
`compose.yaml`.

## Requirements

Docker, with the compose plugin. That is the whole list.

## The commands

Run from the repository root. Nothing in this directory is a secret: the
credentials are local-development defaults for a database bound to this
machine, and the environment overrides them without touching any file —
`GATEWAY_POSTGRES_PASSWORD` (the role's password), `GATEWAY_POSTGRES_PORT`
(the published host port), `GATEWAY_POSTGRES_HOST` (the published host
interface; the default `127.0.0.1` keeps a development database off the
network), and `GATEWAY_POSTGRES_PROJECT` (the compose project name; set it
when a second checkout of this repository runs beside the first).

One caveat on the password: `POSTGRES_PASSWORD` seeds the cluster only
when its volume is first created. Overriding `GATEWAY_POSTGRES_PASSWORD`
on an existing volume does not change the role's password — reset the
volume with `down -v` (or `ALTER ROLE` in place) after changing it.

The same is true of the two databases, and it is the one thing a volume
created before the plane split gets wrong: `control` and `dataplane` are
created by [`initdb/`](initdb/10-create-plane-databases.sh) on the first
start of an empty volume, so an older volume holds a single database named
`gateway` and neither of these. `down -v` is the fix, and the one-time cost
is the local data in it.

### Start, stop, reset

```bash
# Start the database and wait until it accepts connections.
docker compose -f deploy/postgres/compose.yaml up -d --wait

# Stop it; the named volume keeps the data.
docker compose -f deploy/postgres/compose.yaml down

# Stop it AND delete the data — the reset lever. The next `up` is a fresh
# volume, and migrations apply from zero.
docker compose -f deploy/postgres/compose.yaml down -v
```

### Migrations

The connection string lives in the compose file — the command is the verb
and nothing else. The lane is one variable, `GATEWAY_MIGRATE_LANE`, and it
names both the directory the runner reads and the database it writes:

```bash
# Apply every pending migration. The default lane is `dataplane`, so this
# one command never touches the Control Plane's database.
docker compose -f deploy/postgres/compose.yaml run --rm migrate up

# The same verb for the Control Plane: its lane applies `migrations/control/`
# to the `control` database and to no other (`migrations/README.md`).
GATEWAY_MIGRATE_LANE=control \
  docker compose -f deploy/postgres/compose.yaml run --rm migrate up

# Show the applied version and whether it is clean.
docker compose -f deploy/postgres/compose.yaml run --rm migrate version

# Roll back the most recent migration.
docker compose -f deploy/postgres/compose.yaml run --rm migrate down 1

# Roll every migration back (the verify suite does this and proves it clean).
docker compose -f deploy/postgres/compose.yaml run --rm migrate down -all

# After a failure left the state dirty: set the version that is ACTUALLY
# applied — the last SUCCESSFUL migration, usually the one before the
# failure — to clear the flag, then fix and re-apply. Here the database
# applied 000001 cleanly, so 1 is that version; 0 would un-apply real
# history.
docker compose -f deploy/postgres/compose.yaml run --rm migrate force 1
```

Migration files live under [`migrations/<plane>/`](../../migrations) at the
repository root — ordered, reviewed SQL, never an ad-hoc edit and never an
ORM's auto-migration. A new migration is a hand-authored pair placed in the
lane that owns the schema (the runner mounts `migrations/` read-only on
purpose: migrations are written where every file in the repository is
written — in an editor, as a reviewed diff):

```text
migrations/dataplane/000002_<name>.up.sql    # what this change applies
migrations/dataplane/000002_<name>.down.sql  # its exact inverse — proven
```

Sequential, zero-padded to six digits, one more than the highest pair in
that lane — each lane numbers from `000001`, because the version the runner
records lives in the database it migrated. The down file is not optional and
not ceremonial: `verify.sh` executes every down file against the real
database.

### Connect

```bash
# The Control Plane's database.
docker compose -f deploy/postgres/compose.yaml exec postgres \
  psql -U gateway -d control

# The Data Plane's.
docker compose -f deploy/postgres/compose.yaml exec postgres \
  psql -U gateway -d dataplane
```

Both commands reach the same role, and that is the fixture's convenience rather
than a security property: these are two ownership boundaries, not two
credential boundaries — ["What is deliberately not here"](#what-is-deliberately-not-here)
states the production privilege contract.

The database also listens on `127.0.0.1:5432` (`GATEWAY_POSTGRES_HOST` and
`GATEWAY_POSTGRES_PORT` to move either) for a local psql or a GUI client.

### Verify everything

```bash
bash deploy/postgres/verify.sh
```

Runs the whole integration suite against the real database: both lanes'
directories proven to hold exactly what the runner will read — versions
numbered 1..N with no gap and no duplicate, one `.up.sql` and one `.down.sql`
per version, nothing else in either lane, and the failing-migration fixture
in neither; startup with both plane databases answering; the pinned image
proving it ships the timescaledb extension; each lane applying into its own
database, its recorded version validated against the lane's newest file and
proven clean, and re-application proven a no-op; the two lanes proven to be
two databases — the Control Plane's foundation migration building its
ownership namespace and nothing else, no history and no schema crossing the
boundary in either direction, and one fixture credential opening both;
transaction behaviour; a migration that fails mid-file (proved to roll back
whole, record itself dirty, refuse further runs, and recover through
`force`); a full down roll of both lanes, proved clean — and both databases
left migrated and running. CI runs this exact
command: the `Verify (persistence)` job in
[`.github/workflows/ci.yml`](../../.github/workflows/ci.yml) executes it on
a runner whose preinstalled Docker and compose plugin meet the
[requirements](#requirements) above, and `ci-gate` — the required check a
branch ruleset enforces — fails unless that job passed. One script, one
definition of green, for a contributor and for the pipeline alike.

## What is deliberately not here

- No schema beyond what the landed domains own. Tables arrive with the
  domains that own them, as migrations in the lane that owns them; the Data
  Plane's bootstrap migration enables the `timescaledb` extension and nothing
  else, and the Control Plane's lane holds its ownership namespace, the
  identity foundation (accounts, users, api_keys) and the credential
  projection's foundation — the change log, the materialized mirrors and the
  revision counter ([ADR 0007](../../docs/adr/0007-control-to-data-projection.md)).
  The Data Plane's lane carries that pipeline's consumer half beside its
  catalog foundation: the mirror tables and the position singleton.
- No per-plane roles, and no claim that this fixture demonstrates credential
  isolation — it does not. One convenient role (`gateway`) owns and opens both
  databases, which is a local-development fixture and not a security property:
  the database ownership boundary above is real, the credential boundary is
  absent. `verify.sh` asserts that the one role opens both databases, so the
  absence is a fact the suite proves rather than prose that can drift back into
  a stronger claim. Production is a different contract, and this is the whole
  of it — per plane database, two roles, stated as required properties; the
  names are a suggestion in the shape `<plane>_app` / `<plane>_migrator`, not
  a fixed convention:

  - The application role. CONNECT on its own plane's database; USAGE on its
    namespace; SELECT, INSERT, UPDATE and DELETE on that plane's tables. No
    CREATE, no DDL, no superuser, no cross-plane grant.
  - The migration role. Ownership of the lane's schema objects; CREATE on the
    namespace; the only role that runs golang-migrate — invoked by CI or an
    operator through the pinned runner image, never by an application:
    `console-api`, `dataplane` and `dataplane-api` do not run migrations at
    startup or anywhere else. `dataplane-api` holds no database role at all,
    because it owns no database (ADR 0006 §7).

  The local fixture provisions neither role and keeps the single `gateway`
  one. Which roles exist in a given deployment, and what they are called, is
  a production decision this fixture deliberately does not pre-make.

- No production deployment. This compose project is a development database;
  how the store runs in production is a deployment decision that has not
  been made yet, and this file will not pre-make it.
- No host-installed tools. Everything runs from pinned images, so the only
  version that matters is the one written in `compose.yaml`.
