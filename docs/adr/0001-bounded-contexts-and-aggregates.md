# ADR 0001: Bounded contexts and aggregate boundaries

- Status: Accepted
- Date: 2026-09-23
- Issue: [#5](https://github.com/ecoma-io/llm-gateway/issues/5)

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
5. **Quota capacity is guarded state owned by Accounting, written only by
   the accounting flows.** Each entitlement cycle and each PAYG balance has
   a funding-bucket projection (ADR 0004). The bucket is an Accounting
   aggregate: the Commerce entitlement cycle it projects and the account's
   PAYG flag reference it by identifier and never write it outside the
   named transactions. Its available capacity is drawn down only by
   conditional updates inside the four cross-context coordinated
   transactions of rule 6; refills arrive through the ledger-backed flows
   that grant or fund it — the grant-cycle roll of rule 6, and the
   topup/adjustment flows of ADR 0004 — each carrying its own ledger legs
   and uniqueness guards.
6. **Exactly four cross-context coordinated transactions exist.** They are
   authorised here by name; each writes rows owned by more than one context
   in one short database transaction, and no other database transaction
   crosses a context boundary.
   - **Admission** — create the `Request` shell and its intake record,
     insert the `Reservation` with its allocation legs, conditionally
     reserve funding-bucket capacity, append `hold` ledger legs, start the
     execution lease. One transaction.
   - **Settlement** — create the unique `Settlement`, append the
     `UsageEvent`, append consume/release ledger legs per allocated bucket,
     update bucket projections, close the `Reservation`, finalise the
     request. One transaction.
   - **Release/compensation** — when no candidate could serve, or the
     reaper expires an abandoned hold: append release legs, update bucket
     projections, flip the reservation state (guarded — one writer wins),
     finalise the request with its failure reason. One transaction; the
     same shape for the operator-facing release and the reaper.
   - **Grant-cycle roll** — on a subscription's period boundary: verify
     renewal, create the cycle's entitlements and funding buckets, append
     `grant` ledger legs, advance the cycle fields, keyed by
     `(subscription, cycle_number)` so a retry cannot grant twice. One
     transaction.
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
   bug.
8. **Catalog and Identity are read-mostly on the hot path.** They may be
   cached aggressively; their aggregates remain the source of truth, and
   caches are downstream of the database, never upstream of it.

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
  where both endpoints are relational; they stop at the family boundary.
- `Request`/`RequestAttempt` become event-shaped tables (ADR 0005), which is
  what makes the Timescale split possible without re-modelling later.
- Cross-context workflows (a request touching Identity, Catalog, Commerce,
  Accounting in one call) are choreographies over IDs: outside the four
  authorised coordinated transactions of rule 6, each context's writes stay
  in its own transactions. [../architecture/request-lifecycle.md](../architecture/request-lifecycle.md)
  specifies the choreography.
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
  references keeps the option open without paying for it.
- **Execution as a mutable aggregate** (a `Request` row updated at each step):
  rejected — the write amplification buys nothing; facts are final when
  written, and in-flight state is not shareable state.
