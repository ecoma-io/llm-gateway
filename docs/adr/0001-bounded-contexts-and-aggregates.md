# ADR 0001: Bounded contexts and aggregate boundaries

- Status: Accepted
- Date: 2026-09-23
- Issue: [#5](https://github.com/ecoma-io/llm-gateway/issues/5)
- Amended by: [ADR 0006](0006-control-plane-and-data-plane.md) — rules 5, 6 and 8 are plane-scoped, and one rule now sits above the list

## Context

The gateway is about to grow its business domains — identity, model routing,
request execution, commerce, and metering — on top of a scaffold that has none
of them. Nothing about the aggregate layout has been decided, and the first
schema PR will silently decide it if this document does not: table boundaries,
transaction boundaries, and which invariants are even expressible are all
fixed by the aggregate model chosen now.

The load-bearing questions:

- Which entities belong together in one transaction (one aggregate), and which
  only reference each other by ID?
- Where does a request's money/quota state change, and can those changes ever
  happen while a provider call is in flight?
- Which context owns each word — alias, request, entitlement — so that two
  engineers reading "request" mean the same thing?

## Decision

The gateway is modelled as **five bounded contexts**. A context owns a
vocabulary and a set of aggregates; contexts reference each other by
identifier only, never by shared tables or shared rows.

| Context    | Owns (aggregates in bold)                                                                                                      | Responsibility in one line                                              |
| ---------- | ------------------------------------------------------------------------------------------------------------------------------ | ----------------------------------------------------------------------- |
| Identity   | **Account**, **User**, **APIKey**                                                                                              | Who may call the gateway at all.                                        |
| Catalog    | **ModelAlias** (with its ordered candidates), **Backend**                                                                      | What can be requested and where it can be served from.                  |
| Execution  | **Request** and its **RequestAttempt** records (append-oriented history, not a mutable aggregate root)                         | What actually happened on the wire, per logical request.                |
| Commerce   | **Plan**, **Subscription**, **Entitlement**, **PriceList**                                                                     | What an account is allowed and expected to consume, at what price.      |
| Accounting | **Reservation**, **FundingBucket**, **LedgerEntry**, **UsageEvent** (UsageEvent is an immutable fact, not a mutable aggregate) | How consumption is held, recorded, and settled without double-counting. |

Aggregate rules:

> **No transaction crosses a plane boundary.** _Added by
> [ADR 0006](0006-control-plane-and-data-plane.md), and it sits above the list
> rather than in it — the rules are numbered, and every reference to "rule 6"
> elsewhere in this repository still means rule 6. It is written first because
> every rule below is now read through it: a coordinated transaction is local
> to one **plane** as well as to one context, and where two planes must agree
> they converge through idempotent facts rather than through a distributed
> commit. The rule constrains the transaction, not the model — a context does
> not become a service by being placed in a plane, and the five contexts below
> are unchanged._

1. **An aggregate is an ownership and invariant boundary.** Each aggregate
   exclusively owns its rows, and every invariant the gateway guarantees
   about a set of rows must be enforceable inside a transaction on the
   aggregate that owns them. The normal write is a **local aggregate
   transaction** — one aggregate's rows in one short transaction.
   Coordination across aggregates is never implicit: it exists only as three
   named exceptions — the four cross-context coordinated transactions of
   rule 6; Catalog configuration activation, a coordinated transaction
   local to the Catalog context (ADR 0003) that spans more than one
   Catalog aggregate; and the capacity-funding and correction flows of
   ADR 0004 (topup, adjustment, and a usage correction's compensating
   legs), coordinated transactions local to the Accounting context that
   span the funding bucket, the ledger legs, and — for a correction — the
   usage event it adjusts. Outside those named transactions, no transaction
   spans aggregates.
2. **Identity is three small aggregates, not one big one.** `Account` carries
   the billing relationship's identity; `User` and `APIKey` each reference the
   account and are independently mutable (a key is revoked without touching
   the account row; a user is removed without rewriting keys). A user belongs
   to exactly one account at this stage; multi-account membership is out of
   scope and deliberately not modelled.
3. **`ModelAlias` owns its candidate list atomically.** Reordering, adding, or
   removing candidates is one transaction on the alias aggregate, so a request
   in flight always resolves against a consistent candidate ordering.
   `Backend` is its own aggregate because credential rotation and endpoint
   changes must not be coupled to alias edits; a candidate references a
   backend by ID.
4. **Execution is append-oriented history with one living row.** The
   `Request` row is created as a shell at admission (so attempts and the
   reservation can reference it from birth) and is finalised exactly once
   when the request completes; `RequestAttempt` rows are appended as each
   upstream call finishes. The live state of an in-flight request (current
   candidate, stream position) lives in the request's own memory during
   execution. Nothing in Execution is a lock the business waits on.
5. **Quota capacity is guarded state, and the plane boundary makes it two
   rows rather than one.** Each entitlement cycle and each PAYG balance has,
   in the **Control Plane**, a **funding bucket**: an Accounting aggregate
   whose cached settled/held/available values are maintained in the same
   transaction as its ledger legs and are rebuildable from them (ADR 0004).
   The Commerce entitlement cycle it projects and the account's PAYG flag
   reference it by identifier and never write it outside the named
   transactions; its capacity is drawn down by conditional updates inside
   the Control-Plane halves of rule 6's settlement and cycle roll, and
   refilled by the ledger-backed flows that fund it — the grant-cycle roll,
   and the topup/adjustment flows of ADR 0004 — each carrying its own ledger
   legs and uniqueness guards.

   In the **Data Plane**, the same cycle has a **quota projection**: the
   lockable capacity row the runtime's admission conditionally draws down in
   the same transaction as its reservation and hold legs. It is not a
   balance, and it is not Accounting-owned — it is the **enforcement
   ceiling** for one cycle or PAYG balance, seeded from Control-Plane grants,
   written only by the runtime, and converging to the ledger by
   reconciliation. The ledger, never the projection, is the source of truth
   for money (ADR 0006).

6. **Exactly four cross-context coordinated transactions exist.** They are
   authorised here by name; each writes rows owned by more than one context
   in one short database transaction, and no other database transaction
   crosses a context boundary. Each is also scoped to a single plane, which
   is the one thing ADR 0006 changed about them:
   - **Admission** — create the `Request` shell and its intake record,
     insert the `Reservation` with its allocation legs, conditionally
     reserve quota-projection capacity, start the execution lease. One
     transaction, **entirely in the Data Plane**: the rows it writes are the
     runtime's own, and the capacity it draws down is the runtime's
     projection of a Control-Plane grant. The `hold` ledger legs and the
     bucket projections this transaction used to write are written in the
     Control Plane instead, from the reservation as a fact — the runtime
     must not hold ledger write authority (ADR 0006, section 2), and the
     capacity it can actually spend is the projection, not the bucket.
   - **Settlement** — create the unique `Settlement`, append the
     `UsageEvent`, append consume/release ledger legs per allocated bucket,
     update bucket projections, close the `Reservation`, finalise the
     request. **No longer one transaction**, because it is no longer one
     plane: the runtime writes the usage fact and closes its reservation
     locally, and the Control Plane writes the settlement, its ledger legs
     and its bucket projections from that fact, idempotently by
     `request_id`. The unique settlement per request is unchanged, so the
     customer is still charged exactly once; what is gone is the atomicity
     between the charge and the fact, replaced by a fact that is the
     authority the charge derives from (ADR 0006).
   - **Release/compensation** — when no candidate could serve, or the
     reaper expires an abandoned hold: return the capacity to the runtime's
     projection, flip the reservation state (guarded — one writer wins),
     finalise the request with its failure reason. One transaction, **in the
     Data Plane**; the same shape for the operator-facing release and the
     reaper. The `release` ledger legs and the bucket projections follow in
     the Control Plane, derived from the reservation's terminal state — the
     Control Plane learns of it as a fact.
   - **Grant-cycle roll** — on a subscription's period boundary: verify
     renewal, create the cycle's entitlements and funding buckets, append
     `grant` ledger legs, advance the cycle fields, keyed by
     `(subscription, cycle_number)` so a retry cannot grant twice. One
     transaction, **entirely in the Control Plane** as it always was — and
     it additionally publishes the new capacity to the runtime's quota
     projection, keyed by the entity's own identifier.
     All four are short, row-scoped, and never span a provider call. This is
     the saga: reserve → execute → settle, with release as the compensation
     for an abandoned reservation. These four are the complete set: work that
     seems to need a new cross-context (or cross-aggregate) database
     transaction is an amendment to this ADR first, never a new code path.
     Configuration activation (aliases, group versions, price revisions) is
     a coordinated transaction **inside the Catalog context only** (ADR 0003)
     — Catalog-local, not a fifth member of the cross-context set; it never
     spans contexts. The same holds for the Accounting-local funding and
     correction flows of ADR 0004 (topup, adjustment): with funding buckets
     Accounting-owned (rule 5), they coordinate bucket and ledger inside one
     context, so they are not members of the cross-context set either.
7. **No transaction is open while a provider call is in flight.** Quota is
   held as committed data (a reservation and its legs), not as a held lock,
   and an in-flight request keeps its hold alive by renewing its execution
   lease — committed state updated in its own short operation, never a held
   lock or an open transaction. This rule exists because holding row locks
   across a multi-second upstream call is the classic gateway deadlock/latency
   bug. The lease itself is a runtime concern and stays where the request is
   served: it is Data-Plane state, renewed by the process holding the
   reservation and read by nobody else (ADR 0006).
8. **Catalog and Identity are read-mostly on the hot path.** They may be
   cached aggressively; their aggregates remain the source of truth, and
   caches are downstream of the database, never upstream of it. Under the
   plane split this rule stops being a performance preference and becomes an
   ownership statement, though not the same one for both contexts. Catalog
   configuration is **the Data Plane's own**, stored where the runtime reads
   it and administered through the management surface (ADR 0006, section 3) —
   the alias and candidate rows the hot path resolves against are the
   catalogue, not a copy of one. Identity reaches the hot path as a
   **projection**: the runtime authenticates against key records it holds,
   seeded from the Control Plane, because the authority over a key is not the
   runtime's (ADR 0006, section 8). Either way the conclusion is the one this
   rule wanted, in stronger form: the runtime resolves an alias and
   authenticates a key with the Control Plane switched off, and neither read
   is a cross-plane call.

### Terminology ownership

Each term has exactly one owning context (full glossary in
[../architecture/overview.md](../architecture/overview.md)):

- _request_, _attempt_, _commitment_ — Execution;
- _alias_, _candidate_, _backend_, _adapter_, _egress_ — Catalog;
- _subscription_, _entitlement_, _plan_, _PAYG_ — Commerce;
- _reservation_, _settlement_, _usage event_, _ledger entry_, _funding
  bucket_ — Accounting;
- _account_, _user_, _API key_ — Identity.

A document or schema using a synonym where a glossary term exists is a defect.

## Consequences

- The PostgreSQL schema will decompose naturally along aggregate lines: one
  table per aggregate (plus allocation/event child tables), cross-aggregate
  and cross-context references by identifier, no cross-context join tables.
  Enforced foreign keys are permitted within the relational family (ADR 0005)
  where both endpoints are relational; they stop at the family boundary. The
  plane split then draws the outer line the aggregate lines were always
  heading for: the Control-Plane contexts' tables live in one database, the
  Data-Plane contexts' in another, and no foreign key may cross between them
  because no statement can (ADR 0006).
- `Request`/`RequestAttempt` become event-shaped tables (ADR 0005), which is
  what makes the Timescale split possible without re-modelling later. That
  split lands on the Data-Plane side, because those are the runtime's facts.
- Cross-context workflows (a request touching Identity, Catalog, Commerce,
  Accounting in one call) are choreographies over IDs: outside the four
  authorised coordinated transactions of rule 6, each context's writes stay
  in its own transactions. [../architecture/request-lifecycle.md](../architecture/request-lifecycle.md)
  specifies the choreography. Under the plane split it is a choreography over
  planes as well, and the two are not the same picture: contexts split the
  vocabulary, planes split the deployment, and a message that crosses a
  context boundary inside one plane is still a local transaction while a
  message that crosses a plane boundary is never one.
- The rules above are now **mechanically enforced**, which is what makes them
  unusually safe to have moved. Each application's build fails on a domain
  package reaching for a driver or a framework client, on an application
  package importing a concrete adapter, and on one module requiring
  another's; each application's route table is compared against its own
  contract in both directions. "No transaction crosses a plane boundary" is the one rule
  without a test — it is enforced by the engine instead, because two
  databases cannot be queried in one statement, and a test asserting that
  our code does not do something the database forbids would be ceremony.
- Adding a new invariant later means asking "which aggregate's transaction can
  enforce this?" first; an invariant that no single aggregate can enforce is
  a design smell, not a trigger for a new cross-aggregate transaction —
  authoring one of those means amending this ADR (rule 6).

## Alternatives considered

- **One `Account` mega-aggregate** owning users, keys, subscriptions, and
  entitlements: rejected — every API-key authentication and every quota
  allocation would contend on one row, and revoking a key would take a lock
  that admission waits for.
- **Microservice-grade contexts with per-context stores**: rejected for now —
  the operational cost is unjustified at this scale; the discipline of ID-only
  references keeps the option open without paying for it. The two-database
  split of ADR 0006 is not this alternative arriving by the back door: it is
  two stores for two **planes**, one cluster either way, and the five contexts
  still share one database each rather than owning one apiece.
- **Execution as a mutable aggregate** (a `Request` row updated at each step):
  rejected — the write amplification buys nothing; facts are final when
  written, and in-flight state is not shareable state.
