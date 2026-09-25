# ADR 0003: Commerce — concurrent subscriptions, scoped entitlements, and PAYG

- Status: Accepted
- Date: 2026-09-23
- Issue: [#5](https://github.com/ecoma-io/llm-gateway/issues/5)
- Amended by: [ADR 0006](0006-control-plane-and-data-plane.md) — the cycle
  roll, and the capacity admission reserves, are plane-scoped

## Context

The gateway must support an account that holds several commercial
relationships at once: a monthly plan with quota for one model family, a
smaller plan for another, and optional prepaid PAYG credit when entitlement
quota runs out. A `current_plan` field cannot represent this: it erases
coexisting grants, makes allocation order undefined, and turns a funding
balance into a fictional subscription.

The model must decide, without an implementation inventing policy: which
quotas apply to an alias, their deterministic consumption order, what expires,
what concurrent requests can reserve, and when PAYG may be spent.

## Decision

### Entities and ownership

- **Plan** — an immutable, versioned commercial template: recurring price,
  billing period, and ordered **grant definitions**. A plan carries a
  monotonic `version_number`; a version has its own lifecycle
  (`draft` → `published` → `retired`). A `draft` may be edited freely; once
  `published` it is immutable — retiring it only stops new subscriptions,
  it rewrites nothing. A grant definition has a stable `grant_definition_id`,
  a scoped alias-group **version**, a dimension (`cost`; the only defined
  dimension), and an amount. A plan version may not contain two definitions
  with the same `(alias_group_version, dimension)`. A plan grants nothing by
  itself.

  > **Amended by B5's implementation.** The definition's stored scope is the
  > alias-group **name**, not a version: the version is resolved from the
  > name at each cycle's roll (the resolution this ADR already describes
  > below), and the uniqueness that guards a version's definition set is
  > correspondingly `(alias_group_name, dimension)`. Storing the name is
  > what makes the every-roll re-snapshot true for a definition authored
  > once — a definition naming a version would have pinned the snapshot at
  > authoring time, and the roll would have had nothing to resolve.

- **Subscription** — one account's instantiation of one specific plan
  **version**, which it pins forever: a plan change means a new version and a
  new subscription (or an explicit migration), never an edit in place. Any
  number of subscriptions may be active concurrently, **including multiple
  active subscriptions to the same plan version**. They never merge; each has
  independent cycles and entitlements. No account has a `current_plan` field.
- **Entitlement** — one live quota grant materialised from exactly one
  `(subscription, cycle_number, grant_definition_id)`. It records its
  immutable scope version, granted amount, and state (`active` or `expired`
  only — there is no suspended entitlement; suspension is a subscription
  state applied at admission, not stored per grant). The triple is unique,
  preventing duplicate grants when a cycle worker retries.
- **Alias group version** — Catalog-owned, immutable membership snapshot of a
  named group of aliases. Entitlements reference the version issued with their
  grant; editing group membership creates a new version and affects only
  grants issued after the change. `*` is the singleton immutable wildcard
  group version. Membership is **never expanded into per-alias rows**: the
  entitlement stores the version ID and admission evaluates against the
  version's stored snapshot. Grants never expand retroactively — each cycle
  roll pins whatever version of the named group is current **at that roll**,
  so each cycle re-snapshots membership at roll time, and every admission in
  that cycle sees exactly that snapshot.
- **PAYG** — an account-level prepaid funding source: an enablement flag and
  an account PAYG funding bucket. The flag is this context's; the bucket row
  is an Accounting aggregate the account references by identifier (ADR 0001,
  rule 5). It is neither a plan nor a subscription: no
  recurring price, period, grant cycle, or expiry. The bucket row exists from
  account creation (balance zero); enabling is only the spending flag.
  Disabling blocks **new** spills at admission — open holds already secured
  against the bucket still settle normally against it.

  > **Amended by [ADR 0006](0006-control-plane-and-data-plane.md).** The
  > bucket is a Control-Plane row; what admission spills against is the
  > runtime's PAYG **quota projection**, seeded from it (ADR 0001, rule 5).
  > The flag, the blocking rule and the settlement of already-open holds are
  > unchanged — only the row the spill reads moved planes.

- **Client PriceList** — Catalog-owned price revision that gives exactly one
  input and output unit price to **every active alias**. It is deliberately
  alias-exact, never group-priced, so price selection has no precedence rule
  to invent. A revision is drafted, then **activated**; from activation it is
  immutable and carries `effective_from`. The effective revision at any
  instant is the activated revision with the greatest `effective_from` not
  after that instant; the Catalog activation transaction enforces that
  activated `effective_from` values are unique, so the selection is total. A
  request snapshots the effective revision at admission.

**Money is one platform-wide settlement currency at v1.** Every ledger
amount, price, hold, and grant is denominated in that single currency;
per-account currencies are deferred (open question,
[overview](../architecture/overview.md)). Amounts are **integers in the
currency's minor units** — never floats. Unit prices are **integer minor
units per 1,000,000 tokens**, one for input and one for output per alias.
Rounding happens **once, at settlement**, from the snapshot's unit prices;
the admission hold is the **ceiling of the same arithmetic** over
`input + max_output_tokens`, so a settled amount can never exceed its hold.
Cost is the only entitlement dimension now; token/request dimensions are
named future extensions rather than implicit conversion rules.

### Subscription lifecycle and its data

```text
pending ── start_at ──▶ active ── payment hold ──▶ suspended
                            │  ▲                       │
                            │  └──── reinstated ───────┘
                            │
                            ├── cancel_at reached / immediate revoke ──▶ cancelled
                            │
                            └── fixed term ends without renewal ───────▶ expired
```

- `pending` has `start_at`; it grants no quota before that time.
- `active` has a current `cycle_number`, `period_start`, and `period_end`.
  It rolls into the next cycle only when `renewal_enabled` is true and
  `cancel_at` is null or after the new cycle would end.
- `suspended` holds its historical entitlements but admission treats their
  available balance as zero. Reinstatement returns it to `active`.
- A **scheduled cancellation** is not a state transition: the subscription
  remains `active` and stores `cancel_at` plus `cancellation_mode=scheduled`.
  At the effective time it becomes terminal `cancelled`; renewal/grant rolling
  is suppressed. An **immediate cancellation** stores the same fields with
  `cancel_at=now`, changes to `cancelled`, and ends admission immediately.
- `expired` is terminal only for a fixed-term non-renewing subscription that
  reaches its natural end without a cancellation instruction. Historical
  subscriptions and their entitlement rows remain forever because immutable
  accounting entries reference them.

Cycle arithmetic is fixed so no implementation invents it:

- `cycle_number` starts at **1** on the first roll and increments by one per
  roll; the `(subscription_id, cycle_number)` pair is unique.
- A period is a **calendar month anchored at `start_at`**: period 1 runs
  `start_at` → same day-of-month one month later (clipped to month end when
  the day does not exist), and each later period continues from the previous
  one's end. Other billing periods are named future extensions.
- `period_start`/`period_end` are null only while `pending`; the first roll
  sets them and they are never null afterwards.
- Rolls are performed by a **worker that scans for due subscriptions**
  (`active`, `renewal_enabled`, no blocking `cancel_at`, period ended) using
  the database clock; the roll transaction itself is the only writer of cycle
  fields.

### Cycle roll is a coordinated transaction

A subscription cycle roll is one of the four cross-context coordinated
transactions of ADR 0001 (rule 6): lock the
subscription; verify state/time/renewal; create each unique entitlement;
create its funding bucket; append its `grant` ledger legs; update
the cycle fields. It is keyed by `(subscription_id, cycle_number)` and commits
all of those changes or none. Thus two workers cannot grant a cycle twice, and
a crash cannot leave quota without its grant history.

> **Amended by [ADR 0006](0006-control-plane-and-data-plane.md).** All of
> those rows are the Control Plane's, so the roll is now entirely
> Control-Plane-local — it always was one context's transaction, and the split
> only pinned it to one **plane** as well. The capacity it grants reaches the
> runtime as a published fact, seeded into that cycle's quota projection
> (ADR 0001, rule 5); the projection, not this transaction, is what admission
> draws down. The uniqueness key and the all-or-nothing property are
> unchanged.

> **Implemented by B5, the commerce foundation — with one deliberate gap.**
> What shipped is the roll's Commerce half: the entitlement inserts and the
> guarded cycle advance in one unit of work, with each grant definition's
> alias-group **name** resolved to the group's current version through the
> Control → Data seam read _before_ the unit opens. The funding bucket and
> its `grant` legs are the Accounting context's; they join the same unit of
> work at settlement (B6) without changing the key or the all-or-nothing
> property — until then the entitlement rows are the complete record of what
> a cycle granted ([commerce](../architecture/commerce.md)).

### Admission and the allocation waterfall

For a request on alias `a`, the account has a request **admission timestamp**
from PostgreSQL `transaction_timestamp()`; this database time, not an app-node
clock, decides both cycle membership and the effective price revision.

1. **Match** entitlements whose subscription is `active`, whose cycle covers
   the admission timestamp, whose entitlement state is active, and whose
   immutable alias-group version contains `a`.
2. **Order every match totally** by:
   1. scope specificity: a named group before `*`;
   2. earliest `period_end`;
   3. oldest `subscription.created_at`;
   4. stable `entitlement_id` ascending.
3. **Reserve** from funding buckets in that order using a conditional update
   on each bucket's materialised available capacity (ADR 0004). A reservation
   may split across several entitlement buckets. The matching rows,
   allocation rows, and reservation are one admission transaction — no
   provider call occurs in it.

   > **Amended by [ADR 0006](0006-control-plane-and-data-plane.md).** The
   > conditional update is no longer on the bucket: the bucket is a
   > Control-Plane row, and the capacity admission draws down is the
   > runtime's **quota projection** of it, in the Data Plane's database,
   > inside this same admission transaction (ADR 0001, rule 5). The `hold`
   > legs and the bucket projections are written in the Control Plane
   > instead, from the committed reservation as a fact. The waterfall, its
   > ordering and its all-or-nothing behaviour are unchanged — what changed
   > is which row the conditional update guards, and which plane writes the
   > audit legs this step used to write.

4. **Spill to PAYG** only after all matching entitlements: if PAYG is enabled
   **and its available balance covers the whole shortfall**, reserve that
   shortfall there. Otherwise reject the request as `insufficient_entitlement`.
   Partial reservations never execute.

Worked example:

| Subscription | Scope       | Cycle ends | Remaining |
| ------------ | ----------- | ---------- | --------- |
| S1 (oldest)  | `anthropic` | in 2 days  | 20        |
| S2           | `anthropic` | in 26 days | 45        |
| S3           | `*`         | in 9 days  | 80        |

A 60-cost reservation on `claude-sonnet-5` (in `anthropic`) allocates
**S1: 20 → S2: 40**. Scope specificity precedes expiry, so S3's wildcard is
saved for aliases without dedicated entitlement. This exact ordering is the
one and only waterfall; implementation must not add a “reasonable” order.

### Reservation is a hard execution ceiling

The request contract requires a canonical `max_output_tokens`. It is rejected
when absent, non-positive, or above the alias's `max_output_tokens` limit;
adapters translate the canonical bound to provider parameters or reject a
backend that cannot enforce it. The gateway counts input tokens with its
**canonical tokenizer** before admission — the reservation and the enforced
output bound are both denominated in canonical tokens, so the ceiling does
not depend on any provider's counter. The hold is then:

```text
price(snapshot, canonical input tokens + max_output_tokens)
```

The price revision and canonical bounds are immutable fields of the
reservation, and the hold is additionally capped by the alias's **reservation
cap** (a maximum hold in minor units): a request whose computed hold exceeds
the cap is rejected `invalid_request` before any capacity is taken. Because
client prices are alias-exact rather than candidate-exact, every
candidate/fallback is covered by the same client hold. The adapter must
stop/cancel output at the bound. Therefore actual billable usage is never
allowed to exceed the reservation — actual usage commonly **differs by being
lower**, then the unused hold is released. An upstream that reports more than
the enforced bound is a provider-cost anomaly recorded on its attempt, not
unsecured client debt and not a reason to bill extra.

At settlement the capture-method order of ADR 0004 applies: provider-
**reported** usage is priced when the committed attempt carries it;
otherwise delivered content is re-counted with the canonical tokenizer.
Provider and canonical counts may legitimately differ; the ceiling is
canonical, the billed amount follows the capture method.

This deliberately chooses **hard stop** over debt/overdraft: the client never
receives an unreserved billable unit, and `adjustment` is for explicit
operator corrections, never automatic unbounded overdraw.

### Expiration and in-flight work

- A grant's entitlement becomes unavailable to **new** admission at its
  `period_end`; unused available quota expires then.
- An allocation reserved before period end remains settlement-eligible against
  its original entitlement even after the cycle ends. It never hops to a new
  cycle and no excess allocation exists (the reservation is the hard ceiling).
- An active request maintains a reservation lease. The reaper expires an
  `open` reservation only when `expires_at` is past **and no live execution
  lease exists**; the maximum request/stream duration is strictly below the
  maximum renewable lease. A live stream therefore never loses its hold at a
  cycle boundary or ordinary TTL boundary.
- If the process dies, the lease stops and the reaper releases the open
  allocations. Settlement after expiry is then **forbidden** — the released
  capacity may already belong to a newer request — so a proven orphaned
  completion is recorded as an unbillable usage fact for reconciliation,
  never charged (ADR 0004).

### PAYG activation

PAYG is enabled per account by the operator today (later the account console).
Enabling authorises spending but adds no money. Funding appends `topup` ledger
legs into the account's PAYG funding bucket. An alias is servable iff it has a
matching active entitlement **or** PAYG is enabled; because price revisions are
total over active aliases, an enabled PAYG account can serve an unentitled
alias entirely at that alias's rates. A disabled or insufficient PAYG bucket
rejects — it never partially funds an execution.

### Configuration activation is Catalog-local

Alias state, alias-group versions, and Client PriceList revisions are one
Catalog configuration consistency boundary. An activation transaction locks
that revision and the relevant aliases, and validates: every active alias has
exactly one price in the effective revision; all candidate aliases exist; and
an activated group version has immutable membership. Retiring an alias
removes it from service but **does not free its name**: alias identities are
unique across active and retired alike, so historical ledger rows keep an
unambiguous referent, and admission to a retired alias is `unknown_alias`. A
concurrent alias or price activation serializes on that Catalog
configuration revision. The activation transaction is coordinated but stays
entirely inside the Catalog context — Catalog-local, deliberately **not** a
fifth cross-context transaction in ADR 0001's set (rule 6). Commerce
references the resulting immutable IDs; it does not attempt cross-context
validation at runtime.

## Consequences

- The schema has no `current_plan`; it has subscriptions, cycle identities,
  one entitlement per unique grant definition/cycle, alias-group versions,
  and lockable funding-capacity rows (ADR 0004).
- Same-plan double subscriptions are first-class and deterministic, not an
  accidental duplicate to merge.
- Past pricing and past entitlement scope can be reconstructed without asking
  what today's group membership or price list says.
- A client cannot use a small reservation to obtain an unbounded stream;
  upstream provider cost beyond the client cap is the gateway's operational
  anomaly, not hidden customer debt.

## Alternatives considered

- **Single `current_plan` on Account**: rejected — cannot represent concurrent
  grants or a PAYG-only account.
- **Live mutable alias-group membership**: rejected — it changes purchased
  entitlement scope retrospectively and makes historical admission
  unreproducible.
- **Price by alias group with fallback precedence**: rejected — overlapping
  groups force a second waterfall for prices; alias-exact pricing makes one
  price unambiguous.
- **Soft reservation plus settlement overdraw**: rejected — turns a prepaid
  admission gate into unsecured credit under concurrent long streams.
- **PAYG as synthetic subscription**: rejected — it incorrectly imports
  grant-cycle and expiry semantics into a balance.
