# Local PostgreSQL / TimescaleDB

The gateway's authoritative primary store, as a developer runs it: one
compose file, pinned images, one command for everything. What a contributor
runs locally and what an integration lane runs are this same file and this
same suite — there is no second definition of green.

The store is PostgreSQL with the
[TimescaleDB](https://docs.timescale.com/) extension: transactional tables
for the OLTP domains, hypertables for the time-series workloads, one
database. Which tables belong to which family is not decided here —
[ADR 0005](../../docs/adr/0005-relational-and-event-storage-split.md) set
the placement rule, and every future migration applies it. Not every table
becomes a hypertable.

## Decision record

Pinned as of 2026-09-23:

| What             | Pin                                                                                                                    | Why this one                                                                               |
| ---------------- | ---------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------ |
| PostgreSQL       | **18** via `timescale/timescaledb:2.30.1-pg18@sha256:9dede0e3ccc071cf71935b17f76bf243331df0b1575338c8ac294640fcf12a36` | TimescaleDB 2.30.1 (2026-09-17) supports PG 16/17/18; 18 is the newest. Community edition. |
| Migration tool   | **golang-migrate** `migrate/migrate:v4.20.1@sha256:76cc2074cb6642631f34a898ced71e6aeaa6b1a4d78c4daa743275a22e0c5be7`   | See the comparison below.                                                                  |
| Migration format | Sequential pairs under [`migrations/`](../../migrations): `000001_name.up.sql` + `000001_name.down.sql`                | Authored, reviewable, one file per direction.                                              |

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
  the immutability rule above, review, and this suite. If silent-edit
  detection or destructive-change linting ever becomes a requirement,
  re-evaluate Atlas (checksummed directories, Pro-licensed lint) — that is a
  deliberate escalation, not a default.

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

One caveat on the password: `POSTGRES_PASSWORD` seeds the database only
when its volume is first created. Overriding `GATEWAY_POSTGRES_PASSWORD`
on an existing volume does not change the role's password — reset the
volume with `down -v` (or `ALTER ROLE` in place) after changing it.

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
and nothing else:

```bash
# Apply every pending migration.
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

Migration files live in [`migrations/`](../../migrations) at the repository
root — ordered, reviewed SQL, never an ad-hoc edit and never an ORM's
auto-migration. A new migration is a hand-authored pair (the runner mounts
`migrations/` read-only on purpose: migrations are written where every file
in the repository is written — in an editor, as a reviewed diff):

```text
migrations/000002_<name>.up.sql    # what this change applies
migrations/000002_<name>.down.sql  # its exact inverse — proven by the suite
```

Sequential, zero-padded to six digits, one more than the highest pair that
exists. The down file is not optional and not ceremonial: `verify.sh`
executes every down file against the real database.

### Connect

```bash
docker compose -f deploy/postgres/compose.yaml exec postgres \
  psql -U gateway -d gateway
```

The database also listens on `127.0.0.1:5432` (`GATEWAY_POSTGRES_HOST` and
`GATEWAY_POSTGRES_PORT` to move either) for a local psql or a GUI client.

### Verify everything

```bash
bash deploy/postgres/verify.sh
```

Runs the whole integration suite — startup, connectivity, migration
application, validation, transaction behaviour, a migration that fails
mid-file (proved to roll back whole, record itself dirty, refuse further
runs, and recover through `force`), clean rollback — against the real
database, and leaves the database migrated and running. CI's persistence
lane, when the workflow-owning change wires it in, runs exactly this
script.

## What is deliberately not here

- No business schema. Tables arrive with the domains that own them, as
  migrations under `migrations/`; the bootstrap migration enables the
  `timescaledb` extension and nothing else.
- No production deployment. This compose project is a development database;
  how the store runs in production is a deployment decision that has not
  been made yet, and this file will not pre-make it.
- No CI workflow changes. The suite is written to be that lane verbatim
  (`bash deploy/postgres/verify.sh`), but `.github/workflows/` is owned by
  another change — wiring it in is one step, documented here, not done here.
- No host-installed tools. Everything runs from pinned images, so the only
  version that matters is the one written in `compose.yaml`.
