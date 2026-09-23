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
[Amendments](#amendments-to-adrs-0001-0004-and-0005).

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

| Direction      | What crosses                                                                                | Keyed by                                 |
| -------------- | ------------------------------------------------------------------------------------------- | ---------------------------------------- |
| Control → Data | Configuration and grants: aliases, backends, price revisions, entitlements, key projections | the entity's own identifier — idempotent |
| Data → Control | Usage facts and quota consumption: what was delivered, what was held                        | `request_id` — idempotent                |

The first row crosses in two different senses, and the difference matters.
Catalog configuration — aliases, candidates, backends, price revisions — is
**stored where the runtime reads it**, in the Data Plane's own database, and
crosses as an instruction written through the management surface (section 3).
Entitlements and key projections cross as records, because the Control Plane is
their authority. The row states a direction, not where a record lives; the
record-by-record answer is the matrix in
[../architecture/planes.md](../architecture/planes.md).

Rules that follow:

- **No synchronous cross-plane call on the runtime path.** Management
  operations are request/response over the management contract; usage flows in
  the other direction as facts.
- **No shared table and no cross-plane transaction.** Each plane writes its own
  database (section 7).
- **Every cross-plane message is a fact with a key**, so redelivery is a no-op
  and no distributed transaction is needed. Where coordination is required —
  a key being minted, capacity being granted — it is explicit
  request/response with a defined failure behaviour, never a two-phase commit.
- **The console never talks to `dataplane-api`.** When the console needs a Data
  Plane operation, the call is:
  `console-api application → DataPlaneManagementPort → HTTP adapter → dataplane-api`.
  The application layer depends on the port; the port is where the plane
  boundary is legible in code.

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

- `dataplane-api` is a management transport: it owns **no** database and no
  persistence adapter. It must not become a second persistence owner.
- No application connects to the other plane's database, and no cross-plane
  foreign key exists in either direction. This is enforced by the engine itself
  — PostgreSQL cannot query across databases in one statement — rather than by
  convention.
- No cross-plane transaction. Where the two planes must agree, they converge
  through idempotent facts (section 5), not through a distributed commit.
- `migrations/` splits into the same two lanes; each database carries its own
  `schema_migrations`. The TimescaleDB extension and the hypertables belong to
  the Data Plane database, because the high-volume time-series facts are the
  runtime's.
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
  that cannot be taken down by the Control Plane.
- Key creation does not require a distributed transaction: the Control Plane
  records ownership and asks the Data Plane for the credential through the
  management port; a failure leaves an owned-but-inactive key, which is
  recoverable and visible, not a half-charged account.

### 9. Why `dataplane-api` is a management boundary and not a second domain

`dataplane-api` is a **transport**: an operator-facing HTTP surface over the
Data Plane's own configuration and state. It owns no aggregate, no database and
no persistence adapter, and its package set is deliberately smaller than
`console-api`'s — an architecture test asserts that.

The intended shape, once the Data Plane domain has content:

```text
management HTTP adapter → Data Plane management application → Data Plane domain → repositories / runtime ports
runtime HTTP adapter    → Data Plane runtime application    → the same Data Plane domain → the same repositories
```

Both transports must sit over one domain core; duplicating routing or provider
logic into `dataplane-api` is the failure mode this section exists to prevent.

**Open question, recorded rather than guessed.** At this stage the Data Plane
domain is empty, so a shared core module would be an abstraction with nothing
in it. How `dataplane-api` reaches Data Plane state — a core module both
transports depend on, or the runtime's module exposing public domain and
application packages — is therefore **undecided**. The decision belongs to the
ADR of the first management operation that needs it; until then, the rule that
no application module requires another's stays true and enforced.

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
architectural contract is that the management surface is internal.

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
- **One database, two schemas**: rejected — schemas separate namespaces, not
  credentials; a runtime credential with `USAGE` on the cluster can read the
  money tables. Two databases make the violation impossible to express.
- **A shared Go foundation module for the three services**: rejected — it
  re-couples their release cadence and invents a fourth artifact whose only
  purpose is to avoid copying a few hundred lines.
- **Splitting the event tables into a separate analytics database, keeping one
  application**: rejected — it pays the cross-database cost ADR 0005 warned
  about without delivering the isolation that justifies it; the plane boundary
  is what makes that cost worth paying.
- **Microservices from the start, per bounded context**: rejected — the same
  boundary, paid for with operational cost the scale does not justify.
