# ADR 0006: Control Plane and Data Plane

- Status: Accepted
- Date: 2026-09-23
- Issue: [#35](https://github.com/ecoma-io/llm-gateway/issues/35)

## Context

ADRs 0001–0005 design five bounded contexts, four coordinated transactions, a
reserve-execute-settle accounting model and a two-family storage layout — and
never say how many processes carry them. At the time of this decision the
repository shipped one Go service (`apps/api`), one Vue console and one OpenAPI
document, so the implicit answer was "one process, one schema, one contract" —
which is the answer this record replaces.

That answer is wrong for what is being built, for reasons that have nothing to
do with team size:

- **The two workloads have opposite profiles.** An LLM request path is
  connection-heavy, latency-bound, horizontally scaled, machine-authenticated,
  and its availability is the product. The console's backend is request/response
  CRUD over identity, commerce and configuration, human-paced, low-volume, and
  it can be down for a maintenance window without a single completion failing.
  Scaling them together necessarily scales the wrong one.
- **They hold different credentials.** The runtime holds provider credentials
  and customer API-key material; the console's backend holds sessions and
  billing authority. A runtime that can read the ledger is a runtime whose
  compromise is a billing compromise. The Data Plane should hold the fewest
  secrets and the least authority that still lets it serve.
- **They change at different speeds.** Routing, provider adapters and egress
  are the fastest-moving parts of the product. Commerce and identity are the
  slowest. One deployable makes every pricing-page change a runtime release.

The tension this ADR must resolve is real and is recorded, not papered over:
ADR 0001 rule 6 makes admission and settlement single database transactions
spanning contexts, and ADR 0005's Context rejects splitting the event tables
out of the accounting database because it "forces a transactional outbox
between a charge and its usage fact". A plane boundary makes the first claim
false as written and re-opens the second. Both are addressed in
[Amendments](#amendments-to-adrs-0001-0003-0004-and-0005).

The cost of not deciding now: the first schema PR and the first product
endpoint decide the deployment shape silently, and
[overview.md](../architecture/overview.md),
[request-lifecycle.md](../architecture/request-lifecycle.md) and ADR 0005
already read as if there were one serving process.

## Decision

### 1. Four deployable applications

```text
                         ┌─────────────────────┐
                         │       console       │  Vue 3 + Loom — presentation only
                         └──────────┬──────────┘
                                    │ user-facing API (console.yaml)
                                    ▼
                         ┌─────────────────────┐
                         │     console-api     │  Control Plane / BFF
                         │  identity · commerce │
                         └──────────┬──────────┘
                                    │ management contract (dataplane.yaml)
                                    ▼
                         ┌─────────────────────┐
                         │    dataplane-api    │  Data Plane management surface
                         └──────────┬──────────┘
                                    │ internal service
                                    ▼
                         ┌─────────────────────┐
LLM clients ────────────►│      dataplane      │  Data Plane runtime
        (runtime.yaml)   │  /v1/* · routing     │
                         │  egress · usage      │
                         └──────────┬──────────┘
                                    │
                          providers / egress
```

One repository, one release unit, four deployables. The console and the three
Go binaries ship together; they are separate processes for the reasons above,
not separate products.

### 2. Why the split exists

Three reasons, in order of weight: **blast radius** (the runtime must not be
able to read or write money), **failure and scaling independence** (completions
must not depend on, or contend with, the console's backend), and **release
cadence** (the hot path changes far more often than commerce).

It is explicitly _not_ an organisational boundary: the contexts of ADR 0001 are
unchanged and are still not services. Nothing here justifies a fifth
deployable, and the next section says what each one must not do so that the
boundary does not creep.

### 3. Responsibilities

| App             | Plane                 | Owns                                                                                                                                                                                                                                                             | Never does                                                                                              |
| --------------- | --------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------- |
| `console`       | Presentation          | Vue 3 shell, routing, presentation state, the generated console client                                                                                                                                                                                           | Knows Data Plane internals; calls `dataplane-api`; holds policy that belongs to a backend               |
| `console-api`   | Control Plane         | Identity (account, user, session, RBAC, ownership), commerce (plan, subscription, entitlement), billing-facing workflows, console orchestration                                                                                                                  | Serves `/v1/*`; executes provider requests; routes or selects egress; reads Data Plane tables           |
| `dataplane-api` | Data Plane management | Administrative configuration of the Data Plane: providers, model aliases and candidates, routing and egress policy, runtime key state, quota configuration, operational status                                                                                   | Exposes console concepts (`/me`, sessions, subscriptions, billing); re-implements the Data Plane domain |
| `dataplane`     | Data Plane runtime    | `/v1/*`; API-key authentication; alias resolution; admission and quota enforcement; routing and candidate selection; pre-commitment fallback; provider adapters; egress; streaming; usage capture; request history; runtime reservation and settlement mechanics | Calls `console-api`; requires `dataplane-api` to serve a request; holds billing authority               |

### 4. Runtime hot-path isolation

The invariant, stated once and enforced by tests:

```text
LLM request → dataplane → provider / egress
```

The normal `/v1/*` path must not traverse `console-api`, must not depend on the
Control Plane being available, and must not require `dataplane-api` to answer.
Runtime configuration is read from the Data Plane's own persistence and cache,
never fetched per request from a management API.

The enforcement is mechanical, not aspirational: each backend is its own Go
module, so another app's `internal/` packages do not compile into it, and
architecture tests assert the rest (no cross-module `require`, no management
client port in the runtime, no management route literals in the runtime).

### 5. Communication rules

| Direction      | What crosses                                                                                                  | Keyed by                                 |
| -------------- | ------------------------------------------------------------------------------------------------------------- | ---------------------------------------- |
| Control → Data | **Configuration and projections**: aliases, backends, price revisions, entitlements, key projections          | the entity's own identifier — idempotent |
| Data → Control | **Facts and observations**: usage facts, request-outcome facts, quota-consumption facts, reconciliation state | `request_id` — idempotent                |

The directions have names, and using them is part of the rule: Control → Data
carries **configuration** and **projections**, Data → Control carries **facts**
and **observations**. Neither direction is "data synchronization", because
nothing is synchronised — one side states what the Data Plane should hold, the
other states what it observed. The operational protocol behind the second
direction is [../architecture/cross-plane-protocols.md](../architecture/cross-plane-protocols.md);
the wire shape of the feed is `api/openapi/shared/usage-facts.yaml`.

The first row crosses in two different senses, and the difference matters.
Catalog configuration — aliases, candidates, backends, price revisions — is
**stored where the runtime reads it**, in the Data Plane's own database, and
crosses as an instruction written through the management surface (section 3).
Entitlements and key projections cross as records, because the Control Plane is
their authority. The key projection's protocol — its durable log, its
revision ordering, its snapshot bootstrap and its replay — is
[ADR 0007](0007-control-to-data-projection.md); the rules of this section are
its skeleton, that record is its body. The row states a direction, not where a
record lives; the record-by-record answer is the matrix in
[../architecture/planes.md](../architecture/planes.md).

Rules that follow:

- **No synchronous cross-plane call on the runtime path.** Management
  operations are request/response over the management contract; usage flows in
  the other direction as facts.
- **No shared table and no cross-plane transaction.** Each plane writes its own
  database (section 7).
- **Every cross-plane message carries a key**, so redelivery is a no-op and no
  distributed transaction is needed. Where coordination is required — a key
  being minted, capacity being granted — it is explicit request/response with a
  defined failure behaviour, never a two-phase commit.
- **The console never talks to `dataplane-api`.** When the console needs a Data
  Plane operation, the call is:
  `console-api application → dataplane.Management → HTTP adapter → dataplane-api`.
  The application layer depends on the port; the port is where the plane
  boundary is legible in code.

#### Control → Data: an idempotent command

A management call states what the Data Plane should hold — an alias, a
candidate order, a price revision, a key's runtime state, a capacity grant —
and it is keyed by the entity's own identifier, so the Data Plane may apply it
twice without the second application changing anything. It is
request/response, it is not on the LLM request path, and the runtime does not
need it to serve (section 4). Nothing in this direction is a push of state the
Data Plane already owns: the Control Plane is the authority for entitlements
and key ownership, and the Data Plane is the authority for everything it
serves.

#### Data → Control: a durable pull with replay

The runtime never pushes, never calls back, and the Control Plane never asks
the Data Plane to mark anything consumed. The canonical flow:

```text
LLM → dataplane → UsageEvent committed durably
                        │
                        ▼
                 dataplane private listener        (internal, not the runtime surface)
                        ▲
                 dataplane-api management façade   (transport only, owns no state)
                        ▲
                 console-api poller                (owns its cursor)
                        │
                        ▼
                 Control Plane settlement
```

Three rules follow, and they are the whole protocol:

- **The cursor is opaque.** It names a position in the Data Plane's fact order.
  No consumer may parse it, synthesise it, compare it or order by it, and
  nothing outside the Data Plane constructs one.
- **Replay is normal.** The same range may be requested any number of times;
  repeated delivery is expected, not an error. There is no acknowledgement
  endpoint, no consume-and-delete, and no way for a consumer to tell the Data
  Plane it has finished with anything.
- **The consumer advances its own position, after applying.** It stores the
  `next_cursor` it has acted on, and only after the facts up to it are durably
  recorded. A crash between the two replays; that is safe, because applying a
  fact twice is a no-op.

**Ordering** is a single monotonic, gap-aware append sequence assigned by the
Data Plane at the moment a fact commits — so an aborted transaction leaves a
hole that is skipped rather than a reordering. It is deliberately not
`occurred_at`, not physical row order and not an unordered identifier: replay
needs a total order that survives a concurrent writer, and only an append
sequence gives one.

**Idempotency.** `request_id` is the fact's immutable business identity and the
logical idempotency key for everything derived from it. It is **not** the HTTP
`X-Request-Id`, which correlates a call and means nothing across a retry. For
`request_id` to be sufficient, the feed carries at most one settlement-relevant
terminal fact per runtime request; a future multi-fact audit feed needs its own
key and is explicitly out of scope here.

**Crash safety** is one local transaction covering applier and cursor:

```text
fetch → apply → commit(applied facts + next_cursor) → crash after commit → replay is a no-op
fetch → apply → crash before commit  → cursor unchanged → same facts refetched → safe
process failure                      → cursor unchanged → retried
```

The cursor is a claim about work already done, so it must never move ahead of
that work: a failure leaves the position untouched and the same page is read
again. A position the Data Plane can no longer replay fails explicitly with
`cursor_expired` (HTTP 410) and never silently skips forward; a malformed
request fails with `invalid_request` (HTTP 400).

**Cursor ownership.** The Data Plane owns the facts — `usage_events`, a future
table; the Control Plane owns its consumption position —
`control.usage_ingestion_cursor`, a future table. Neither exists yet, and the
code says so rather than standing in for them. The Data Plane's fact reader is
a port whose production adapter refuses with a source-unavailable error, because
an in-memory feed would pass every test in this repository while losing every
fact on restart. The Control Plane's position is a repository-level port
(`apps/console-api/internal/ports/outbound/persistence`) with the future table
documented beside it and test fakes behind it. The schema PR fills both tables
in.

This replaced the one line this section used to carry — "usage facts and quota
consumption, keyed by `request_id` — idempotent" — which named the key and
nothing about how a fact reaches the consumer that derives money from it.
Transport, order, cursor, retry and crash behaviour are decisions, and they are
recorded here rather than left to the first implementation that needs them.

### 6. Hexagonal architecture inside every backend

Each of the three Go applications is laid out the same way and obeys one
dependency rule:

```text
adapters → application → domain
     ↘         ↓            ↑
       ports (inbound/outbound)
```

- `domain/` depends on nothing infrastructural — no `database/sql`, no
  `net/http`, no provider SDK, no configuration package.
- `application/` orchestrates domain and ports; it never imports a concrete
  adapter.
- `adapters/inbound` (HTTP) and `adapters/outbound` (postgres, valkey) depend
  on ports, never the reverse.
- `cmd/<app>/` is the composition root: load configuration, construct clients,
  repositories, adapters and services, wire the server, own the lifecycle and
  shutdown. No business logic lives in `main.go`.

Interfaces exist only at real boundaries. The scaffold states the rule as path
predicates in architecture tests, so `internal/domain` is governed the moment a
package appears there, and a package with no responsibility is not created to
satisfy a diagram.

### 7. Database ownership

One TimescaleDB deployment, **two databases, explicitly owned**:

```text
timescaledb (one cluster)
├── control     ← console-api   (migrations/control/)
└── dataplane   ← dataplane     (migrations/dataplane/)
```

- `dataplane-api` is a management façade (section 9): it owns **no** database,
  no persistence adapter and no state of its own. It must not become a second
  persistence owner.
- No application connects to the other plane's database, and no cross-plane
  foreign key exists in either direction. Two databases give separate
  namespaces, separate connection targets, independent transactions,
  independent migration history, and no ordinary cross-database SQL reference —
  a cross-plane join is not a statement PostgreSQL will parse, which is the
  useful part and the whole of it. They do **not** give separate credentials,
  they do not prevent a privileged role from reaching both, they do not stop an
  FDW or `dblink`-style path from bridging them, and they produce no
  production-grade least privilege by themselves. "Database ownership
  boundary" and "security/credential boundary" are two different claims, and
  this ADR makes only the first; the security boundary this decision needs is
  the authenticated management surface of section 9, not the fact that the two
  stores are separate databases.
- No cross-plane transaction. Where the two planes must agree, they converge
  through idempotent facts (section 5), not through a distributed commit.
- `migrations/` splits into the same two lanes; each database carries its own
  `schema_migrations`. The TimescaleDB extension and the hypertables belong to
  the Data Plane database, because the high-volume time-series facts are the
  runtime's. (The extension landed there; the hypertables did not — ADR 0005's
  amendment records why the runtime storage is plain tables.)
- The table-by-table ownership matrix, with a reason per row, lives in
  [../architecture/planes.md](../architecture/planes.md).

### 8. The API-key boundary

An API key is **two records in two planes**, and this is the security boundary
the split creates:

| Control Plane owns                                                                                                                                           | Data Plane owns                                                                                                                      |
| ------------------------------------------------------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------ |
| Which account owns the key; who created and revoked it from the user's perspective; display metadata (prefix, label, timestamps); authorisation to manage it | The secret's hash; the authentication lookup; runtime active/revoked state; runtime scopes and limits; everything the hot path reads |

- **Plaintext is never stored in either plane** after creation, and the
  browser never receives a stored hash — only the display metadata and, once,
  the secret at creation time.
- The Data Plane holds a _projection_ of key state: minted and revoked in the
  Control Plane, delivered to the runtime so that authentication costs no
  cross-plane call. Revocation therefore has a **bounded staleness**, and that
  bound is a named deployment property rather than an accidental one. A
  projection that is stale by seconds is the deliberate price of a hot path
  that cannot be taken down by the Control Plane. The delivery mechanism is
  [ADR 0007](0007-control-to-data-projection.md) — a durable change log in
  this plane's own database, drained by a stateless reconciliation loop over
  the management chain — and the bound is now the producer's interval (5 s by
  default) plus one failed cycle, by construction rather than by intention.
- Key creation does not require a distributed transaction: the Control Plane
  mints the secret, records ownership, and records the secret's digest into
  its own projection log in the same transaction (ADR 0007 §2); a delivery
  failure leaves the key owned, recorded and projected late, when the loop
  catches up — not owned-but-inactive, which was this bullet's answer when
  delivery rode the mint itself. The secret is shown once and is never
  reproducible, so recovery is revoke-and-re-mint — visible, not a
  half-charged account. (Amended twice: before the projection phase built on
  the earlier wording, which had the Control Plane asking the Data Plane for
  a credential the Control Plane in fact mints; and by
  [ADR 0007](0007-control-to-data-projection.md), which replaced the
  synchronous digest delivery this bullet described with the durable
  projection log.)

### 9. Why `dataplane-api` is a management boundary and not a second domain

`dataplane-api` is a **management façade**: an operator-facing HTTP surface over
the Data Plane's own configuration and state, and a transport. It owns no
aggregate, no database and no persistence adapter, and its package set is
deliberately smaller than `console-api`'s — an architecture test asserts both.

The composition is decided, not open:

```text
console → console-api → ports/outbound/dataplane → HTTP adapter → dataplane-api
                                                                      │ outbound port
                                                                      ▼ HTTP adapter
                                                            dataplane private listener
                                                                      │
                                                            Data Plane application/domain
                                                                      │
                                                            Data Plane DB
```

- **`dataplane-api` is management façade and transport.** It owns no provider
  state, no catalogue, no routing, no quota, no reservation, no usage semantics
  and no table of its own; it declares no `internal/domain` and no persistence
  or cache port, and it holds no state at all. `apps/dataplane-api/internal/arch`
  enforces the shape: the only thing that may appear under its outbound trees
  is the seam through which it reaches the Data Plane, and a second package
  beside that seam is a second owner of Data Plane data.
- **`dataplane` is domain, application and persistence authority.** The Data
  Plane's rules about what it serves live in the runtime's module, and the
  runtime's database is the only place they are kept. A management operation is
  a request the Data Plane can refuse, not a query the façade performs.
- **No shared foundation library is introduced, and no Data Plane logic is
  duplicated into the façade.** A shared core module would be a fourth artifact
  with a fourth version and would fuse the two transports' release cadence
  again; a copy of routing or provider logic in the façade would be the second
  owner this section exists to prevent.
- **The contracted surface is the façade, not the process behind it.**
  `api/openapi/dataplane.yaml` is `dataplane-api`'s contract: it is what a
  caller opening a socket to the façade may rely on, and it is the document
  that application's route table is compared against in both directions. The
  listener in `dataplane` is not that surface and does not implement that
  document. The hop between the two is a **private protocol** between two
  processes of the same product, defined in full in
  [docs/architecture/cross-plane-protocols.md](../architecture/cross-plane-protocols.md),
  pinned on each side by its own protocol test, and deliberately not a fourth
  OpenAPI document (AGENTS.md rule 2). The naming follows from this: an
  operation is "contracted" if the façade serves it, and "in the protocol" if
  the listener does.
- **The façade translates; it does not relay.** The two hops carry the same
  page and hold different failure vocabularies. A private failure is classified
  at the façade into one this application can answer from — `410` for a position
  that can no longer be replayed, `502` for anything that leaves the answer
  unknown, including the Data Plane refusing the façade's own credential — and
  the caller's answer is built there. No byte of the listener's body reaches a
  Control Plane caller, which is what keeps `502 upstream_unavailable` meaning
  "no answer came back from the Data Plane" — unreachable, or reachable and
  unusable, with the caller unable to tell which — rather than "something went
  wrong somewhere".

**The open question this section used to record is closed.** It read: the Data
Plane domain is empty, so a shared core module would be an abstraction with
nothing in it, and how `dataplane-api` reached Data Plane state — a core module
both transports depend on, or the runtime's module exposing public packages —
was undecided. The answer is the diagram above: it reaches the Data Plane
through an outbound port in its own module, over the Data Plane's private
management listener, exactly as `console-api` reaches it, and it owns nothing
on the way. The rule that no application module requires another's survives
this, and it is what made the call the answer rather than a shared module.

#### The management service-auth boundary

Every hop into a management surface is authenticated, and the boundary is
explicit and fail-closed:

- **One shared-secret service credential per hop**, supplied by deployment
  configuration: `console-api` presents one to `dataplane-api`, and
  `dataplane-api` presents one to the Data Plane's private listener. The two
  ends of a hop read their own variable — a process names the variables it
  reads — and deployment sets both to one value, so a hop has one secret and
  there is no second copy of it to drift. The two hops do not share theirs:
  `dataplane-api` reads a caller-facing credential and a separate
  Data-Plane-facing one, and refuses to start when the two are equal, because a
  single secret serving both would be a key every management caller holds to
  the Data Plane's private listener.
- **Compared in constant time, at a fixed width, on both hops.** How long a
  refusal takes is not a function of how much of the credential the caller
  guessed right — and, just as importantly, not a function of how long the
  configured secret is. `subtle.ConstantTimeCompare` returns early when its two
  arguments differ in length, so a direct comparison of the presented string
  against the configured one leaks the secret's length before examining a byte
  of it. Both surfaces therefore digest each side through SHA-256 and compare
  the digests, which are the same width whatever was presented; the property
  belongs to the boundary, so both ends of it keep it, and each has its own test
  for it.
- **Fail-closed at every step.** An unconfigured credential, a header presented
  twice, a credential under a scheme this surface does not accept, a token
  carrying whitespace: all refuse. The verification runs before routing and
  before any argument is read, so a caller that has not identified itself cannot
  learn which methods a path accepts or reach use-case code by any route.
- **An unconfigured credential authenticates nobody.** An absent or empty
  configured secret refuses every caller, including one presenting nothing, and
  the composition root refuses to start rather than serving an administrative
  surface it cannot guard. The failure is loud and closed, never open.
- **Service credentials only.** A browser session is never accepted on a
  management surface and neither is a customer LLM API key; a management
  surface authenticates peer applications, not people.
- **No identity system exists, and none is needed.** There is no user, session,
  role, scope, token issuance or authorisation model. The deployment trusts one
  caller per hop, so the only decision to make is whether the credential on the
  request is the configured one.
- **mTLS or a signed credential is the named follow-up.** Either proves the
  same identity cryptographically rather than by possession of a string, and
  either replaces the mechanism without changing application semantics: the
  verification remains a port, and the order stays verify-then-act.

### 10. Why this is not microservices proliferation

One repository, one release unit, one cluster, one migration runner, one
toolchain, one Moon workspace, one generated client. The four applications are
**process and database boundaries chosen for three named reasons** (section 2),
not a distributed-systems commitment.

The contexts of ADR 0001 are unchanged: Identity, Catalog, Execution, Commerce
and Accounting remain bounded contexts, and the planes are a deployment
grouping _of_ them, not a replacement for them. A context does not become a
service by being placed in a plane; the plane has two processes, not five.

### 11. Public and internal surfaces

```text
Public:    console-api            (the console's user-facing API)
           dataplane /v1/*        (LLM clients)
Internal:  dataplane-api          (management surface — not exposed publicly)
```

Administrative Data Plane operations are never exposed through the public
runtime API, and provider credentials and API-key hashes never appear in a
console response. Secrets are write-only and opaque wherever they cross a
boundary. The exact network exposure is a deployment decision; the
architectural contract is that the management surface is internal, and the
authentication that makes "internal" mean something is section 9's.

#### Each surface answers with its own error vocabulary

The three contracts do not share an error shape, and the split is a decision
rather than an oversight. `runtime.yaml` is the runtime's own contract and
answers with the OpenAI-compatible error object —
`{"error": {"message", "type", "param", "code"}}` — because its callers were not
written by this repository: an OpenAI-compatible client parses `error.type` and
`error.message`, and handing it a gateway-shaped envelope because the envelope
happened to exist would put this repository's vocabulary on somebody else's
wire. `X-Request-Id` stays on every runtime response for correlation.
`shared/errors.yaml` is the **console and management** envelope —
`{"error": {"code", "message"}, "request_id"}` — and a code is added to it only
where a real response produces one: `invalid_request`, `unauthenticated`,
`cursor_expired` and `upstream_unavailable` are on the wire because the
management surface returns them, not because they were imagined for it. That
document is the **façade's**; the private hop behind it answers in the same
envelope with its own closed code set and none of the façade-only codes, and the
façade classifies those answers into the codes above rather than passing them
along (section 9, and the mapping table in
[docs/architecture/cross-plane-protocols.md](../architecture/cross-plane-protocols.md)).
Internal application errors stay separate from wire serialization: a caller can
act on the status and the request ID, and cannot act on a stack frame, a query
or a provider message.

**Streaming is defined, not implemented.** How a runtime failure is reported
depends on whether a response has been committed, and the rule is stated in
`runtime.yaml`: before commitment the failure is an ordinary HTTP status and
JSON error body; after commitment no status can be revised, so the failure is
written into the stream as `data: {…error…}` followed by `data: [DONE]`, with
the stream closed and never a second HTTP status; a client disconnect writes no
wire error at all, because there is no channel left to write it on and the
observation belongs to the runtime's usage facts. The runtime's commitment
model — first content-bearing byte — is unchanged by any of this.

## Amendments to ADRs 0001, 0003, 0004 and 0005

These ADRs are amended, not superseded. Their domain reasoning stands; what
changes is the scope of the transactions and which plane owns which row.

**ADR 0003.** Its commerce model — concurrent subscriptions, the allocation
waterfall, PAYG spilling, the Client PriceList — is untouched. Two of its
sentences name rows whose plane moved, and both are corrected in place: the
waterfall's **Reserve** step no longer conditionally updates the funding bucket
(the bucket is the Control Plane's; admission draws down the runtime's
projection of it, and the `hold` legs follow from the reservation as a fact),
and a PAYG spill reads that balance's projection for the same reason. The cycle
roll is now additionally plane-local rather than only context-local: almost
every row it writes was already the Control Plane's, and the capacity it grants
reaches the runtime as a published fact. Its uniqueness key and its
all-or-nothing property are unchanged, as is the Client PriceList's placement —
ADR 0003 already called it Catalog-owned, which is why it is a Data-Plane row
in the matrix.

**ADR 0001.** Rule 1's distinction between aggregate ownership and explicitly
authorised coordinated transactions is unchanged (issue
[#19](https://github.com/ecoma-io/llm-gateway/issues/19) settled that wording),
and this ADR adds one rule above it: **no transaction crosses a plane
boundary**. Rule 6's four coordinated transactions are re-scoped to the plane
that owns their rows:

| Transaction          | Scope now                                                                                                                                                                                                                                                           |
| -------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Admission            | **Data-Plane-local**: request shell and intake, reservation with allocation legs, conditional drawdown of the runtime's quota projection, execution lease; the hold legs and bucket projections follow in the Control Plane, derived from the committed reservation |
| Settlement           | **Split by plane**: the runtime writes the usage fact and closes its reservation; the Control Plane writes the settlement, its ledger legs and its bucket projections from that fact, idempotently by `request_id`                                                  |
| Release/compensation | **Data-Plane-local** against the runtime's projection; the Control Plane learns of it as a fact                                                                                                                                                                     |
| Grant-cycle roll     | Unchanged — already Control-local (Commerce + Accounting) — and it additionally publishes the new capacity to the runtime's projection                                                                                                                              |

Rule 5 is amended the same way: the **ledger's** bucket is Accounting's and
lives in `control`; the **capacity the runtime enforces** is a Data-Plane-owned
projection whose only writer is the runtime, seeded by Control-Plane grants.
Rule 7 ("no transaction open while a provider call is in flight") is unchanged.
Rule 8 ("Catalog and Identity are read-mostly on the hot path") is unchanged in
its conclusion and sharpened in its reasoning: the catalog the runtime resolves
against is its own configuration, stored where it reads it (section 3), and
Identity reaches the hot path as a projection of key state the runtime holds
(section 8). Neither is a cross-plane read, and neither needs this plane to be
up.

**ADR 0004.** Its load-bearing atomicity sentence — "the cache is maintained in
the same transaction as ledger legs" — becomes a two-row statement:

> Within the **Control Plane**, a funding bucket's cached settled/held/available
> values are still maintained in the same transaction as its ledger legs and
> remain rebuildable from them. Separately, the **Data Plane** holds a _quota
> projection_ — the lockable capacity row the runtime's admission conditionally
> updates in the same transaction as its reservation and allocation legs. The
> projection is not a balance: it is the enforcement ceiling for one entitlement
> cycle or PAYG balance, seeded from Control-Plane grants and updated only by
> the runtime. Settlement of record happens in the Control Plane from the
> runtime's usage facts. The projection converges to the ledger by
> reconciliation, and the ledger — never the projection — is the source of truth
> for money.

Invariants 1–6 survive: exactly-once settlement is preserved twice over (the
runtime's reservation close is state-guarded locally; the Control Plane's
unique settlement per request is unchanged), and the no-double-billing-across-
fallback invariant is untouched, since one reservation still yields one
committed attempt and one usage fact.

**ADR 0005.** Its two-family placement rule and its retention policy are
unchanged _within_ a plane. What changes is the sentence in its Context
rejecting a split "because it forces a transactional outbox between a charge
and its usage fact":

> **Amended by ADR 0006.** The event family (`requests`, `request_attempts`,
> `usage_events`) lives in the Data Plane's database, beside the runtime's own
> relational rows — `request_intake`, `reservations`, the API-key records — while
> the Control Plane's relational working set stays in its own. The outbox
> objection is answered by inverting
> the dependency rather than by ignoring it: the usage fact is not downstream of
> the charge, it is the **authority the charge is derived from**, written by the
> process that observed it, and the Control Plane settles from it idempotently
> by `request_id`. What replaces the outbox is a fact that already exists, a
> retry that is safe, and a convergence property that is named. Between planes,
> rows are referenced by ID with no foreign key in either direction.

Its Consequences sentence "settlement writes both families in one transaction on
one cluster" becomes "within the Control Plane the settlement writes its
relational rows in one transaction; the usage fact it settles is written in the
Data Plane and referenced by ID".

## Consequences

- The runtime can be restarted, scaled and deployed without the Control Plane,
  and a Control Plane outage degrades management, never completions.
- The accounting path gains a reconciliation step and loses a transactional
  guarantee it previously had; the guarantee is replaced by idempotent facts
  plus convergence, and the window between a committed runtime reservation and
  its ledger settlement is now a named, bounded property instead of an
  impossibility.
- Two databases to migrate, back up and restore, in one cluster to run.
- Deployment must treat `dataplane-api` as internal; the runtime's `/v1/*` is
  the only public Data Plane surface.
- Boundaries that were previously conventions become tests: module separation,
  import predicates and contract-to-route assertions fail the build when a plane
  reaches into another.
- Some foundation code (HTTP server construction, request IDs, the error
  envelope, graceful shutdown) is duplicated per Go application rather than
  shared. This is deliberate: a shared module would be a fourth artifact with a
  fourth version, and it would fuse the planes' release cadence again.

## Alternatives considered

- **One application with an internal package boundary**: rejected — a package
  boundary is not a credential boundary and not a failure boundary; the runtime
  would still hold the ledger's credentials and share its blast radius.
- **One database, two schemas**: rejected — a schema is a namespace, not an
  ownership boundary, and it makes the accidental join easy rather than
  impossible: a role that can read the runtime's schema can read the money
  tables beside it unless someone remembers to separate them. Two databases
  make the ordinary cross-plane query fail to parse and give each plane its own
  connection target, transaction scope and migration history. That is an
  ownership boundary and not a credential boundary (section 7) — the credential
  boundary is the authenticated management surface of section 9 — and it is
  worth more than the schema arrangement precisely because it does not depend
  on a grant nobody re-derives.
- **A shared Go foundation module for the three services**: rejected — it
  re-couples their release cadence and invents a fourth artifact whose only
  purpose is to avoid copying a few hundred lines.
- **Splitting the event tables into a separate analytics database, keeping one
  application**: rejected — it pays the cross-database cost ADR 0005 warned
  about without delivering the isolation that justifies it; the plane boundary
  is what makes that cost worth paying.
- **Microservices from the start, per bounded context**: rejected — the same
  boundary, paid for with operational cost the scale does not justify.
