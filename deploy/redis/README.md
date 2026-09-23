# Local Redis-compatible service

This directory is a **disposable local-development and integration-test
fixture**, not a production deployment topology and not part of an application
compose stack. It runs the Valkey server selected below.

The service has no authentication because it is bound to loopback only. Never
copy that posture into a remotely reachable deployment: production credentials
are supplied at process wiring time through `VALKEY_USERNAME` and
`VALKEY_PASSWORD`, never committed in source.

Valkey persistence is deliberately disabled (`--save "" --appendonly no`): it
is an implementation boundary for the policy that Redis-compatible state is
never authoritative. Two PostgreSQL databases in one TimescaleDB cluster own
the durable state instead, and they own it separately: `control` holds the
Control Plane's users, keys, subscriptions, entitlements, wallets, ledgers and
payments, and `dataplane` holds the runtime's catalog, routing and provider
configuration, its quota and key projections, and its request and usage
history (ADR 0006 §7). Neither
application reads the other's database.

## Decision record

This PR selects **Valkey 9.1.2** rather than Redis 8.10.2. Valkey is
BSD-3-Clause licensed, Linux Foundation-governed, and compatible with the
Redis OSS 7.2 client protocol. Redis 8 offers RSALv2, SSPLv1 or AGPLv3 instead;
Valkey avoids that licensing friction for the same core RESP commands this
foundation uses (`PING`, `AUTH`, `SELECT`, `GET`, `SET`, `DEL`).

Each Go application that talks to it uses exactly one client:
`github.com/valkey-io/valkey-go` v1.0.78 — Apache-2.0, actively maintained,
and the only runtime requirement in the two modules that have one,
`apps/console-api/go.mod` and `apps/dataplane/go.mod`. It is the official
Valkey client and its CI exercises Valkey plus Redis 5, Redis 8 and Redis
Stack. The standard library has no RESP client; this is the
smallest reasonable dependency. Vendor imports stay inside each application's
own adapter — `apps/console-api/internal/adapters/outbound/valkey` and
`apps/dataplane/internal/adapters/outbound/valkey` — and consumers see only
the named `cache.Cache` port. Its ordinary commands use four multiplexed
connections by default; the separately bounded pool for future blocking
commands defaults to one connection rather than the library's 1024.

Redis-compatible state is restricted to cache, rate limiting, short-lived
coordination, distributed locks/leases, ephemeral state and optional event
streams. It is never source-of-truth storage for the PostgreSQL/TimescaleDB
data named above. Every key is physically namespaced by the application that
wrote it — `console-api:cache:`, `dataplane:cache:` — because one server may
serve both planes and a key written by one must never be readable as the
other's. A future subsystem defines and documents its own prefix before it
writes data; the application name is stated in it so that two planes sharing a
database cannot collide by accident.

A distributed lease/lock deliberately does **not** ship in this foundation.
Correct lock behavior needs a real consumer to define fencing, renewal,
network-partition and stale-holder semantics; a token + compare-and-delete
script protects a key, not the consumer's critical section. It will arrive
with that consumer and its tests.

Sources checked 2026-09-23: [Valkey releases](https://api.github.com/repos/valkey-io/valkey/releases),
[Valkey BSD-3-Clause license](https://raw.githubusercontent.com/valkey-io/valkey/9.1.2/COPYING),
[Valkey governance and compatibility](https://valkey.io/),
[Redis 8.10.2 license](https://raw.githubusercontent.com/redis/redis/8.10.2/LICENSE.txt),
and [valkey-go v1.0.78 test matrix](https://raw.githubusercontent.com/valkey-io/valkey-go/v1.0.78/docker-compose.yml).

## Start and inspect

```bash
# Start the pinned image in the background.
docker compose -f deploy/redis/docker-compose.yml up -d

# Wait until the service reports healthy.
docker compose -f deploy/redis/docker-compose.yml ps

# Run the integration suites against it — the `test-integration` Moon target of
# each module that owns an adapter. Both refuse to run without VALKEY_ADDRESS
# and fail if the tagged suite would run nothing (the same targets the CI
# verify-go job runs):
VALKEY_ADDRESS=127.0.0.1:6379 pnpm exec moon run console-api:test-integration
VALKEY_ADDRESS=127.0.0.1:6379 pnpm exec moon run dataplane:test-integration
```

The compose file pins a readable Valkey version **and** an immutable manifest
list digest. When upgrading it, verify the new tag's digest before changing the
file, then update the Decision record in the same pull request **and** the
matching `image:` pin on the `verify-go` job in
`.github/workflows/ci.yml` — the workflow asserts the two pins agree and
fails until they do.

## The same suite in CI

This is not only a local fixture: the `verify-go` job in
`.github/workflows/ci.yml` starts a service container from the image pinned
above and runs these exact suites through the `test-integration` Moon target
of each module that declares one (`apps/console-api/moon.yml`,
`apps/dataplane/moon.yml`), with `VALKEY_ADDRESS=127.0.0.1:6379` — the same
address as the local command, so the two invocations are one run in two
places. The tagged suites are therefore part of every `ci-gate` run, not an
opt-in extra.

That job also re-derives this directory's pin and its own at run time and
fails when they diverge, so CI can never quietly upgrade to a server version
no developer has run. The enforcement lives in the workflow; this file records
the contract.

## Stop and remove

```bash
# Stop and remove the disposable container. There is no volume or persisted data.
docker compose -f deploy/redis/docker-compose.yml down
```
