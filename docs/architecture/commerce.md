# Commerce

Reference page for plans, subscriptions, entitlements, and PAYG. The decision
and rationale are [ADR 0003](../adr/0003-concurrent-subscriptions-and-entitlements.md);
entity fields are in [overview](overview.md); balance formulas are formalised
in [ADR 0004](../adr/0004-reserve-and-settle-accounting.md) and summarised
below.

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
  re-snapshots group membership, and grants never expand retroactively.

### The cycle roll is a transaction

The roll — verify renewal → create each entitlement and its funding bucket →
append `grant` ledger legs → advance cycle fields — is **one** transaction,
keyed by `(subscription_id, cycle_number)` (ADR 0001, rule 6). A worker
retry after a crash cannot grant a cycle twice; a partial roll cannot exist.
Admission and the roll serialise on the subscription row: an admission either
sees the old cycle or the new one, never a half-rolled state, and cycle
membership is decided by the database clock (`transaction_timestamp()`), not
by any gateway node's clock.

## Entitlement scope and the allocation waterfall

An entitlement is scoped by an **alias-group version** — an immutable
membership snapshot of a named alias set owned by Catalog (`*` is the
singleton wildcard version). Editing a group's membership creates a new
version; entitlements keep the version they were granted with, so purchased
scope never changes retroactively. Entitlements are denominated in ledger
currency (dimension `cost`; further dimensions are named extensions).

For a reservation of `n` on alias `a` at admission:

1. **Match** — entitlements where the subscription is `active`, the cycle
   covers the admission timestamp, entitlement state is active, and the
   entitlement's scope version contains `a`.
2. **Order every match totally** — (1) scope specificity: named group before
   `*`; (2) soonest `period_end`; (3) oldest subscription `created_at`;
   (4) `entitlement_id` ascending. The last tie-break makes the order total
   and replay-stable.
3. **Reserve** — in that order, take `min(available, still-needed)` from each
   bucket via a conditional update on its funding-bucket capacity (ADR 0004),
   inside the admission transaction. A reservation may split across buckets;
   the split's legs keep an ordinal.
4. **Spill to PAYG** — only after all matching entitlements: if PAYG is
   enabled **and** its available balance covers the entire shortfall, reserve
   the shortfall there; otherwise reject `insufficient_entitlement`. A
   reservation is fully secured or the request is rejected whole — there is
   no partial admission and no automatic debt.

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
guarded by the same mechanism as entitlements — its funding bucket is a
lockable row, so two admissions cannot jointly reserve more PAYG than the
account holds. No in-memory balance, no read-modify-write outside the
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
  self-service later. It authorises spending; it funds nothing. The PAYG
  funding-bucket row exists from account creation with a zero balance, so
  enabling is only a flag flip. **Disabling** blocks new spills at admission
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
  The waterfall spills to PAYG only against available.
- **No expiry, no cycles**: PAYG is a balance, not a subscription; it never
  appears as "the current plan" because no such field exists.

## Insufficient entitlement — decision table

| Situation at admission                                                       | PAYG off                          | PAYG on, balance short            | PAYG on, balance covers |
| ---------------------------------------------------------------------------- | --------------------------------- | --------------------------------- | ----------------------- |
| Entitled capacity covers reservation                                         | serve                             | serve                             | serve                   |
| Entitled capacity partial, shortfall exists                                  | reject `insufficient_entitlement` | reject `insufficient_entitlement` | serve (spill)           |
| No matching entitlement; alias servable only via PAYG (ADR 0003 access rule) | reject `no_access`                | reject `insufficient_entitlement` | serve at PAYG rates     |
| Account suspended (key and alias both good)                                  | reject `account_suspended`        | reject `account_suspended`        | reject                  |
| Request missing/invalid `max_output_tokens`, or above the alias limit        | reject `invalid_request`          | reject `invalid_request`          | reject                  |
