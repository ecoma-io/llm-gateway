# Commerce

Reference page for plans, subscriptions, entitlements, and PAYG. The decision
and rationale are [ADR 0003](../adr/0003-concurrent-subscriptions-and-entitlements.md);
entity fields are in [overview](overview.md); balance formulas are formalised
in [ADR 0004](../adr/0004-reserve-and-settle-accounting.md) and summarised
below.

Commerce is a **Control Plane** context: plans, subscriptions and entitlements
are owned by `apps/console-api` and their rows live in the `control` database,
alongside Accounting's, and in no other
([ADR 0006](../adr/0006-control-plane-and-data-plane.md) §§3, 7). Nothing on
the request path reads them directly. What the runtime enforces is its own
projection of the capacity these rules grant (below), and the Control Plane
settles from the usage facts the runtime writes back.

## The shape being supported

```text
one account
  + multiple active subscriptions        (same plan twice included — they never merge)
  + multiple entitlements                (per subscription × cycle × grant definition)
  + optional PAYG overage                (account-level prepaid funding bucket)
```

No account field names a "current plan"; the concept does not exist in the
model. What an account may consume at time _t_ is fully described by "active
subscriptions at _t_ → their entitlements' available capacity, plus PAYG if
enabled".

## Subscription lifecycle and its fields

```text
                ┌──────────────┐
                │   pending    │  start_at in the future; grants nothing yet
                └──────┬───────┘
                       │ start_at reached
                       ▼
   cycle roll    ┌──────────────┐   payment hold / operator hold
   here ───────▶ │    active    │◀──────────────────────────┐
                 └──────┬───────┘                           │
                        │                        ┌──────────┴───────┐
                        │                        │    suspended     │ (reversible)
                        │                        └──────────┬───────┘
          cancel_at reached              natural end,               │ expiry while
          (scheduled cancel)  ▼          no renewal                 │ suspended
                 ┌──────────────┐                 ▼                   ▼
                 │  cancelled   │          ┌──────────────┐    ┌──────────────┐
                 └──────────────┘          │   expired    │    │   expired    │
                                           └──────────────┘    └──────────────┘
```

- **Scheduled cancellation is data, not a state**: the subscription stays
  `active` with `cancel_at` set (`cancellation_mode = scheduled`) and its
  quota usable until then; cycle rolls are suppressed past `cancel_at`. At
  the effective time it becomes terminal `cancelled`. **Immediate
  cancellation** sets `cancel_at = now` and transitions to `cancelled` at
  once; the cycle's unused quota is forfeited either way.
- **Renewal** requires `renewal_enabled` and no past-due `cancel_at`.
- `suspended` keeps its entitlements but admission treats their capacity as
  zero; reinstatement returns them to use with whatever remained.
- `expired` is terminal for a fixed-term subscription that ends naturally
  without a cancellation instruction. Historical subscriptions and
  entitlement rows are retained forever — immutable accounting references
  them.
- Cycle fields: `cycle_number`, `period_start`, `period_end`. `cycle_number`
  starts at 1 and increments per roll; a period is a calendar month anchored
  at `start_at` (day-of-month clipped at month end); `period_start`/`period_end`
  are null only while `pending`. A worker scans for due subscriptions using
  the database clock and performs the roll. At each roll the plan's grant
  definitions materialise **new** entitlements (quota does not roll over)
  against whatever alias-group versions are current at that roll — each cycle
  re-snapshots group membership, and grants never expand retroactively. An
  entitlement cycle's capacity is what the runtime's **quota projection**
  enforces for that cycle; the Control Plane's funding bucket is the ledger
  side of the same capacity, and the two are reconciled rather than merged
  ([ADR 0006](../adr/0006-control-plane-and-data-plane.md);
  [accounting](accounting.md)).
- The version's `recurring_price_minor_units` is recorded because the plan's
  terms are what a subscription pinned — it is informational in the commerce
  foundation, and nothing in commerce charges it: the charging, the holds and
  the settlement are the Accounting context's (B6,
  [ADR 0004](../adr/0004-reserve-and-settle-accounting.md)). Commerce prices
  the _entitlement_; it never moves money.

### The cycle roll is a transaction

The full roll of ADR 0001 (rule 6) — verify renewal → create each entitlement
and its funding bucket → append `grant` ledger legs → advance cycle fields —
is the grant-cycle-roll coordinated transaction, entirely Control-local
(ADR 0006's rule above ADR 0001's rule 6 — no transaction crosses a plane),
keyed by `(subscription_id, cycle_number)`.

**What the commerce foundation ships is the Commerce half of that
transaction**: the entitlement inserts and the guarded cycle advance in one
unit of work. A roll is one fact or none — either the cycle turned and its
entitlement rows exist, or neither happened; the schema's
`(subscription, cycle, grant_definition)` key is belt to the statement's
braces, so a retried roll cannot grant a cycle twice. The Accounting half —
the bucket row and its `grant` legs — arrives with the settlement phase
(B6): it joins the same unit of work, it does not change its key or its
all-or-nothing property, and until it lands the entitlement rows are the
complete record of what a cycle granted.

The roll's mechanics, as implemented:

- **The scopes are resolved before the unit of work opens.** A grant
  definition pins the alias-group **name**, never a version id; the roll
  resolves every definition's name to that group's _current_ version id
  through the Control → Data seam's group-version read
  ([ADR 0006](../adr/0006-control-plane-and-data-plane.md) §5) _outside_ the
  transaction — a roll never holds row locks while waiting on another process
  over a network. The entitlement rows carry the resolved version ids, which
  is the re-snapshotting: cycle N's scope is whatever the group meant at
  cycle N's roll.
- **The advance is a guarded single statement.** Every predicate the domain
  checked on the way in — the state, the cycle number, the period having
  ended on the database's clock, renewal enabled, no scheduled cancellation
  reaching into the new cycle, the owner account still active — is repeated
  in the advance's WHERE clause. The statement, not the earlier read, is
  what makes the roll true; a verdict that does not fire rolls the whole
  unit of work back and skips the row benignly.
- **A worker retry after a crash cannot grant a cycle twice; a partial roll
  cannot exist.** The lock the unit of work takes on the subscription row is
  what keeps two rollers from both opening the next cycle — the second
  blocks, re-reads the advanced cycle, and skips.

What crosses the plane boundary is still only the roll's **result**: the
capacity publication to the runtime's quota projection, keyed by the
entity's own identifier so a redelivered publication is a no-op (the
projection seeding is a later phase; the contract it will consume is
[below](#the-projection-seed-contract)). Cycle membership is decided by the
database clock (`transaction_timestamp()`), not by any gateway node's clock.

### The due-work lanes

One worker pass runs five lanes, in an order the model's dependencies fix,
each bounded by its own batch limit:

| #   | Lane                  | Fires when (database clock)                                               | Verdict                                                                         |
| --- | --------------------- | ------------------------------------------------------------------------- | ------------------------------------------------------------------------------- |
| 1   | Promotions            | a `pending` subscription's `start_at` has arrived                         | promote: activate with cycle 1 and insert its entitlements, one unit of work    |
| 2   | Cancellations         | a scheduled `cancel_at` has passed                                        | complete: `active` → `cancelled`, keeping the instructed instant                |
| 3   | Rolls                 | an active, renewing subscription's `period_end` has passed                | roll into the next cycle with its entitlements, one unit of work                |
| 4   | Subscription expiries | a fixed-term subscription's term has ended without renewal or instruction | `active`/`suspended` → `expired`, history intact                                |
| 5   | Entitlement expiries  | an active entitlement's cycle has ended                                   | `active` → `expired`, the record-keeping flip — enforcement is the projection's |

Cancellations run before rolls so a due instruction ends the subscription
before the roll asks whether to extend it; promotions run first because a
due pending subscription is dead capacity until it activates; the expiry
lanes only observe what the first three left behind.

The lanes' shared vocabulary:

- **The database clock decides everything due.** Every scan predicate and
  every guarded statement tests `transaction_timestamp()` — reached through
  the persistence Clock port, never the process wall clock — and the clock
  read inside a unit of work is transaction-stable, so one read per unit is
  one read per decision.
- **A verdict that does not fire is a skip, not an error.** The world moved
  between the scan and the statement; the row stays exactly as due as it
  was, nothing rolls back half-way, and the next pass asks again.
- **The first real failure stops the pass.** A persistence failure, or a
  scope the catalog cannot resolve, ends the run; every completed row is
  already its own committed fact, the remainder stay due, and the next pass
  resumes from the scans. A group the catalog has never opened is that
  second kind of failure on purpose — a plan granting a scope that resolves
  to nothing is a commercial catalog defect, and a roll that "skipped" it
  every pass would silently sell a plan that grants nothing.
- **Batch limits are the caller's**, passed per lane; zero disables a lane.
  The composition root owns the numbers; no lane infers one from another's.

### The projection seed contract

What a cycle grants reaches the runtime as the **seed of that cycle's quota
projection** — the publication ADR 0001's rule 5 and
[ADR 0006](../adr/0006-control-plane-and-data-plane.md) name, delivered after
the roll's unit of work has committed. The commerce foundation defines the
contract; building the projection and its delivery is a later phase, and
nothing ships either yet. For one entitlement, a seed carries:

| Field                        | Value                                                              |
| ---------------------------- | ------------------------------------------------------------------ |
| `entitlement_id`             | the seed's identity — the key a redelivery is a no-op against      |
| `subscription_id`            | the grant's owner                                                  |
| `cycle_number`               | which cycle the ceiling belongs to                                 |
| `alias_group_version_id`     | the resolved scope, exactly as the entitlement row carries it      |
| `dimension`                  | `cost` in the foundation; a later dimension seeds its own ceilings |
| `granted_amount`             | the ceiling in minor units — what the projection starts from       |
| `period_start`, `period_end` | the cycle's `[start, end)` bounds, UTC                             |

Three properties are the contract's spine:

- **Keyed by the entity's own identifier.** A redelivered seed is a no-op —
  the same idempotence every Control → Data message carries
  ([cross-plane protocols](cross-plane-protocols.md)).
- **Complete on its own.** The projection derives expiry from the carried
  bounds and never asks the Control Plane a question, the same rule the
  settlement facts obey in the other direction. The entitlement-expiry lane's
  record-keeping flip ([above](#the-due-work-lanes)) is not a message to the
  runtime; the bounds are.
- **One direction only.** A seed states a ceiling; it carries no consumption.
  Drawdown happens in the runtime's admission transaction, and what was
  consumed comes back as usage facts — the Data → Control direction.

The PAYG half of the projection — the balance ceiling the waterfall spills
against — seeds by the same rule, but a balance for it to carry does not
exist until the Accounting context settles one (B6); its contract arrives
then, and nothing above changes.

## Entitlement scope and the allocation waterfall

An entitlement is scoped by an **alias-group version** — an immutable
membership snapshot of a named alias set owned by Catalog (`*` is the
singleton wildcard version). Editing a group's membership creates a new
version; entitlements keep the version they were granted with, so purchased
scope never changes retroactively. Entitlements are denominated in ledger
currency (dimension `cost`; further dimensions are named extensions).

A grant definition pins the group's **name**, never a version id: at each
cycle roll the name is resolved to whatever version of that group is current
_then_ ([above](#the-cycle-roll-is-a-transaction)), and the entitlement row
carries the resolved version id. The wildcard group `*` is a legal scope like
any named one — a plan may grant it, and nothing in the waterfall or the
access rule special-cases the grant; specificity and fallback fall out of the
ordering, not of the scope.

For a reservation of `n` on alias `a` at admission:

1. **Match** — entitlements where the subscription is `active`, the cycle
   covers the admission timestamp, entitlement state is active, and the
   entitlement's scope version contains `a`.
2. **Order every match totally** — (1) scope specificity: named group before
   `*`; (2) soonest `period_end`; (3) oldest subscription `created_at`;
   (4) `entitlement_id` ascending. The last tie-break makes the order total
   and replay-stable.
3. **Reserve** — in that order, take `min(available, still-needed)` from each
   bucket via a conditional update on its capacity (ADR 0004), inside the
   admission transaction. A reservation may split across buckets; the split's
   legs keep an ordinal.
4. **Spill to PAYG** — only after all matching entitlements: if PAYG is
   enabled **and** its available balance covers the entire shortfall, reserve
   the shortfall there; otherwise reject `insufficient_entitlement`. A
   reservation is fully secured or the request is rejected whole — there is
   no partial admission and no automatic debt.

Steps 3 and 4 are the runtime's own transaction, and the rows they guard are
its **quota projections** — one ceiling per entitlement cycle, one for the
PAYG balance. The Control Plane's funding buckets record the same movement
from the runtime's facts, keyed by `request_id` and written outside the
admission transaction, so the runtime never holds ledger write authority and
the waterfall order above is unchanged
([ADR 0006](../adr/0006-control-plane-and-data-plane.md);
[accounting](accounting.md)).

Worked example:

| Subscription | Scope       | Cycle ends | Available |
| ------------ | ----------- | ---------- | --------- |
| S1 (oldest)  | `anthropic` | in 2 days  | 20        |
| S2           | `anthropic` | in 26 days | 45        |
| S3           | `*`         | in 9 days  | 80        |

A reservation of 60 on `claude-sonnet-5` (in `anthropic`) allocates
**S1: 20 → S2: 40**. Scope specificity outranks expiry, so S3's wildcard is
preserved for aliases without dedicated entitlement. This ordering is the
single waterfall; every call site (admission; settlement of split holds) uses
it identically.

### Concurrent consumption

Two requests racing the last capacity of a bucket each run the guarded
conditional update; exactly one wins a given unit of capacity, and the loser
continues down the waterfall (or to PAYG, or to rejection). PAYG's capacity is
guarded by the same mechanism as entitlements — its quota projection is a
lockable row, so two admissions cannot jointly reserve more PAYG than the
projection makes available. No in-memory balance, no read-modify-write outside the
transaction, no lock beyond the transaction (ADR 0001, rule 7).

### Settlement of split holds

When the settled usage is below the hold, consumption draws from the
reservation's allocation legs **in their stored waterfall order** (soonest
allocation first), releasing the unconsumed tail; when usage is above the
hold — it cannot be, the reservation is a hard execution ceiling (below).
One settlement, many legs; see [accounting](accounting.md).

## Reservation is a hard execution ceiling

The client contract requires canonical `max_output_tokens`: rejected when
absent, non-positive, or above the alias's `max_output_tokens` limit
(`invalid_request`). Admission counts input tokens with the gateway's
canonical tokenizer, prices `input + max_output_tokens` at the admission-time
price revision, and reserves that amount — unless it exceeds the alias's
**reservation cap** (a maximum hold in minor units), which rejects
`invalid_request` before any capacity is taken. Adapters must stop output at
the bound. Consequences:

- actual billable usage can be **below** the reservation (release the tail)
  but never **above** it;
- "PAYG exhausted mid-flight" cannot happen: every billable unit was already
  inside a secured hold;
- an upstream that reports more tokens than the enforced bound is a
  provider-cost anomaly on the attempt record — operational, not customer
  debt and not extra client billing.

## Expiration and in-flight work

- Entitlement capacity stops being available to **new** admissions at
  `period_end`; unused capacity expires with the cycle. Rows remain
  (`expired`), referenced by ledger history.
- An allocation made before `period_end` stays settlement-eligible against
  its original entitlement after the cycle ends. It never hops to the next
  cycle — and since there is no excess path, nothing ever needs to.
- An executing request keeps its hold alive through a **renewed execution
  lease**; the reaper expires only `open` reservations whose lease is dead
  and `expires_at` has passed. The maximum accepted request/stream duration
  is strictly below the maximum lease, so a live stream never loses its hold
  at a cycle boundary or TTL boundary (ADR 0004).

## PAYG activation and behaviour

- **Activation** is the per-account enablement flag — operator-set today,
  self-service later. It authorises spending; it funds nothing. The flag is a
  Commerce-owned row keyed by the account (`control.account_payg` — an absent
  row is PAYG off), and its funding-bucket reference is write-once: commerce
  refuses to re-point an assigned bucket, while the bucket row it names
  remains the Accounting context's (B6). The PAYG funding-bucket row exists
  from account creation with a zero balance —
  created as the Accounting-side write of the account-creation workflow, a
  choreography over IDs rather than a shared transaction (ADR 0001; the
  bucket is an Accounting aggregate), so enabling is only a flag flip.
  **Disabling** blocks new spills at admission
  (the waterfall treats PAYG as absent); holds already secured against the
  bucket settle normally against it.
- **Funding** appends `topup` legs to the account's PAYG funding bucket
  (operator-recorded today; payment-provider webhooks later —
  [request lifecycle](request-lifecycle.md)). Each topup/adjustment carries an
  idempotency key unique per `(bucket, source, key)`, so a redelivered webhook
  cannot fund twice.
- **Access**: an alias is servable iff a matching active entitlement exists
  **or** PAYG is enabled (ADR 0003). Client prices are **alias-exact** — the
  effective price revision prices every active alias exactly once, so a
  PAYG-servable alias always has one unambiguous price (the price coverage
  rule is validated at configuration write time, ADR 0003).
- **Two balances** (ADR 0004): **settled** = grants/topups − consumes (what
  statements show); **available** = settled − held (what admission checks).
  The waterfall spills to PAYG only against available. This balance is what
  the runtime's quota projection enforces at admission; the ledger remains the
  source of truth for money, and the projection converges to it
  ([ADR 0006](../adr/0006-control-plane-and-data-plane.md);
  [accounting](accounting.md)).
- **No expiry, no cycles**: PAYG is a balance, not a subscription; it never
  appears as "the current plan" because no such field exists.

## Insufficient entitlement — decision table

Every row below is a decision made at admission, which is the runtime's: it
enforces the outcome against its quota projection, and the Control Plane's
ledger is settled from the usage fact the runtime writes for the requests it
did serve.

| Situation at admission                                                       | PAYG off                          | PAYG on, balance short            | PAYG on, balance covers |
| ---------------------------------------------------------------------------- | --------------------------------- | --------------------------------- | ----------------------- |
| Entitled capacity covers reservation                                         | serve                             | serve                             | serve                   |
| Entitled capacity partial, shortfall exists                                  | reject `insufficient_entitlement` | reject `insufficient_entitlement` | serve (spill)           |
| No matching entitlement; alias servable only via PAYG (ADR 0003 access rule) | reject `no_access`                | reject `insufficient_entitlement` | serve at PAYG rates     |
| Account suspended (key and alias both good)                                  | reject `account_suspended`        | reject `account_suspended`        | reject                  |
| Request missing/invalid `max_output_tokens`, or above the alias limit        | reject `invalid_request`          | reject `invalid_request`          | reject                  |
