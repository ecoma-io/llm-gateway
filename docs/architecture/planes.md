# Planes and ownership

Reference page for the two-plane split: which application owns which record, and
the rule that decides it. The decision is
[ADR 0006](../adr/0006-control-plane-and-data-plane.md), which wins over this
page wherever the two disagree. The four applications, the hot-path invariant,
the communication rules, the API-key boundary and the public/internal surfaces
are stated there and are **not repeated here** — this page carries the part
ADR 0006 delegates to it, the table-by-table ownership matrix, and the
reasoning that generates it.

## The rule

Every row below is decided by one sentence:

> **The plane that observes a fact owns its record; the plane that decides a
> policy owns its authority. The two meet only as facts.**

It is worth reading twice, because the halves pull in opposite directions and
the interesting rows are the ones where they appear to conflict. A request
attempt is _observed_ by the runtime, so the runtime owns it; a subscription is
_decided_ by commerce, so the Control Plane owns it.

The second half needs more care than it looks like it needs, because "policy"
is not the Control Plane's monopoly. **How the gateway serves** — which
candidate an alias resolves to, in what order, at what price — is configuration
of the serving process, it changes at the runtime's release cadence, and it
belongs to the Data Plane (ADR 0006, section 2). What the Control Plane holds
authority over is narrower and heavier: **who an account is, what it is allowed
to consume, and what it has been charged.** Read the matrix with that in mind
and every row follows.

Three consequences do most of the work:

- **A projection is not a second owner.** Where the runtime needs data the
  Control Plane decides — aliases, key state, capacity grants, prices — it holds
  a projection it can read with the Control Plane down. The projection is the
  runtime's row because reading it on the hot path is the runtime's job; the
  authority and the record of it are different things, and nothing is owned
  twice.
- **The runtime writes no money.** Ledger legs, settlements and funding buckets
  are Control-Plane records, and the runtime's transactions do not touch them
  (ADR 0006, section 2). Where admission used to write a `hold` leg beside its
  reservation, it now draws down the projection and the Control Plane derives
  the leg from the reservation as a fact
  ([ADR 0004](../adr/0004-reserve-and-settle-accounting.md)).
- **A plane boundary is not a context boundary.** Both databases hold tables
  belonging to several of ADR 0001's five contexts. The contexts are still a
  vocabulary split and are still not services; the planes are a deployment
  split over the same five.

## The ownership matrix

The **plane** column is the answer; the **why** column is the rule above,
applied.

| Record                                                             | Plane         | Why                                                                                                                                                                                                                                                                           |
| ------------------------------------------------------------------ | ------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `accounts`, `users`                                                | Control Plane | Decided here: who an account is, who may sign in, who may act for it. Nothing on the request path needs either.                                                                                                                                                               |
| API-key ownership records                                          | Control Plane | Decided here: which account a key belongs to, who created and revoked it, its display metadata, and who may manage it (ADR 0006, section 8).                                                                                                                                  |
| API-key authentication records                                     | Data Plane    | Observed here: the secret's hash, the lookup, the runtime's own active/revoked state and scopes. Authenticating on the hot path is the runtime's job, so the row it authenticates against is the runtime's.                                                                   |
| `model_aliases` (+ candidates), `backends`, `alias_group_versions` | Data Plane    | Observed here: the runtime resolves an alias to a candidate on every request, and must do so with the Control Plane unreachable. The console's operator reaches them through `dataplane-api` (ADR 0006, sections 3, 5).                                                       |
| `client_price_list_revisions`                                      | Data Plane    | Catalog configuration, not commerce: a price revision is Catalog-owned and bound to an alias (ADR 0003), and the runtime prices every admission from it. The Control Plane holds what an account may consume and what it has been charged — not what serving one token costs. |
| `plans`, `subscriptions`, `entitlements`                           | Control Plane | Decided here: what an account is allowed and expected to consume. The runtime sees only the capacity this produces, as a grant.                                                                                                                                               |
| `funding_buckets`                                                  | Control Plane | The ledger's bucket: maintained in the same transaction as its legs and rebuildable from them. It is the source of truth for money, which is the one thing the runtime must not hold (ADR 0004).                                                                              |
| `settlements`, `ledger_entries`                                    | Control Plane | Money. Written from the usage facts the runtime observed, idempotently by `request_id`.                                                                                                                                                                                       |
| `request_intake`                                                   | Data Plane    | Observed here: idempotent replay must be decidable inside the admission transaction from relational state alone, and admission is the runtime's (ADR 0004).                                                                                                                   |
| `reservations` (+ allocation legs)                                 | Data Plane    | Observed here: the runtime creates the hold and closes it, and the hot path must be able to draw down and release with the Control Plane down.                                                                                                                                |
| Quota projection                                                   | Data Plane    | Projected here: the lockable capacity row admission guards on, seeded from Control-Plane grants and written only by the runtime. Not a balance — an enforcement ceiling that converges to the ledger (ADR 0004).                                                              |
| `requests`, `request_attempts`, `usage_events`                     | Data Plane    | Observed here: what actually happened on the wire. The high-volume time-series facts, and the hypertables with them (ADR 0005).                                                                                                                                               |

Two rows are worth naming as the ones people get wrong. **`funding_buckets` and
the quota projection are not two copies of one thing**: the bucket is a
statement about money that the ledger can rebuild, and the projection is a
statement about what the runtime will permit, which nothing but reconciliation
can rebuild. Only one of them is authoritative, and it is the one the runtime
cannot write. **API keys are deliberately two records**, not one record with a
replica — the split is what makes the hash unreachable from the Control Plane's
session surface and the account identity unreachable from the runtime
(ADR 0006, section 8).

## What crosses, and what must not

The directions are ADR 0006, section 5: configuration and grants one way,
usage facts and quota consumption the other, each keyed by the entity's own
identifier or by `request_id` so that redelivery is a no-op. What follows from
the matrix is the shape of the references:

- **Within a database**, references between the two storage families are by ID
  and may carry an enforced foreign key at the family boundary (ADR 0005).
- **Across the boundary**, references are by ID and carry **no** foreign key in
  either direction. That is not a discipline anyone has to keep: PostgreSQL
  cannot query across databases in one statement, so a cross-plane join is not a
  rule to be broken, it is a statement that does not parse.
- A reference that never arrives — a settlement whose usage fact is delayed — is
  what reconciliation exists to notice. It is not repaired by a distributed
  transaction, which is the thing the split rules out.

The Control Plane reaches the Data Plane only through the management call
ADR 0006 describes, and never on the request path. The console never reaches
`dataplane-api` at all.

## What the split costs

Both costs are named deployment properties rather than accidents, and both are
recorded in ADR 0006:

- **Revocation is bounded-stale.** The runtime authenticates against its own
  projection of key state, so a revoked key stops working after the projection
  catches up, not instantly. A deployment that claimed otherwise would be
  claiming a cross-plane call inside authentication.
- **Settlement converges.** The window between a committed runtime reservation
  and its Control-Plane settlement is now a bounded property rather than an
  impossibility. Exactly-once survives twice over: the reservation close is
  state-guarded in the runtime, and the unique settlement per request is
  unchanged.

## Exposure

```text
Public:    console-api            (the console's user-facing API — api/openapi/console.yaml)
           dataplane /v1/*        (LLM clients — api/openapi/runtime.yaml)
Internal:  dataplane-api          (management surface — api/openapi/dataplane.yaml)
```

Administrative Data Plane operations never appear on the public runtime
surface, and provider credentials and API-key hashes never appear in a console
response. The exact network exposure is a deployment decision; the
architectural contract is that the management surface is internal (ADR 0006,
section 11).

## What this page does not decide

- **How `dataplane-api` reaches Data Plane state.** A core module both
  transports depend on, or the runtime's module exposing public packages —
  ADR 0006, section 9 records this as open, and it stays open here. The rule
  that no application module requires another's holds either way.
- **Any schema.** The table names above are the ones the ADRs already use; no
  column, type, index or constraint is decided by this page. That is the schema
  PR's, and it derives from ADR 0005 and the pages beside this one.
- **Whether the boundary moves again.** The split is a deployment grouping of
  ADR 0001's contexts, chosen for the three reasons in ADR 0006, section 2. It
  is not an invitation to split the contexts into services, and nothing here
  makes that cheaper or more expensive.
