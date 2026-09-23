#!/bin/bash
# The cluster's two databases, created once, on the first start of an empty
# volume.
#
# PostgreSQL's entrypoint creates exactly one database — the one `POSTGRES_DB`
# names — and this deployment serves two, one per plane (ADR 0006 §7). They
# are created here rather than left to a default because the row that matters
# is not "which database exists" but "which database each plane owns", and a
# script a reviewer reads states it while an image default does not.
#
# The template is the image's `template1`, which the timescaledb entrypoint
# has already extended: both databases therefore carry the extension before
# any migration runs, which is capability and not schema. What each plane's
# lane puts inside its own database is what `migrations/` decides.
#
# This runs only when the data directory is empty. An existing volume from
# before the split keeps its single `gateway` database and no `dataplane`:
# the fix is `docker compose -f deploy/postgres/compose.yaml down -v`, which
# is the reset lever deploy/postgres/README.md documents.
set -euo pipefail

for database in control dataplane; do
	createdb --username "$POSTGRES_USER" --owner "$POSTGRES_USER" "$database"
done
