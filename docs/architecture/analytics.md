# B16 — Analytics and Reporting Foundation: design

**Status:** pre-implementation. Authored 2026-09-27 against `origin/main` = `ce0047f`
(B13 merged as #115). Every claim below cites the file it rests on, because the
first seven adversarial reviews disagreed with each other on several points and
the disagreements had to be settled against the schema, not by vote.

**Revision note.** A second adversarial pass over the _written_ design (§ below
marks the changes) confirmed the two load-bearing decisions and corrected four
things this document had wrong. The corrections are recorded in §10 rather than
silently applied, because a reader who saw the first draft deserves to know
which sentences changed and why.

---

## 1. The two findings that decide the shape

### 1.1 The fact contract carries no account, no model, and no occurred_at

`grep` over `api/openapi/shared/usage-facts.yaml` for `account_id`, `api_key_id`
and `alias` returns **nothing**. `public.usage_events`
(`migrations/dataplane/000003_runtime_storage.up.sql:437-460`) carries
`append_seq, request_id, kind, schema_version, capture_method,
committed_attempt_id, provider_input_tokens, provider_output_tokens,
delivery_tokens, price_revision_id, input_unit_price, output_unit_price,
settled_amount, corrects_append_seq, payload, occurred_at` — and the payload is
_closed_ to exactly `{"allocations": [...]}` by a trigger. `control.applied_facts`
(`migrations/control/000008_fact_ingestion.up.sql:90-99`) narrows that to
`settled_amount, capture_method, append_seq, settlement_id, applied_at` and has
**no `occurred_at` at all**.

`occurred_at` _is_ already in hand at the apply site —
`factingestion.go` passes `persistence.Fact` (which carries it) and
`factapplier.go:174-182` records the disposition without it: the `AppliedFact`
written there names the request, the class, the kind, the append seq, the
settled amount, the capture method and the settlement, and no `occurred_at`.
Carrying it into a table would therefore be one column in one migration — and
control migration `000012` does **not** carry it, which is a decision rather
than an omission. The account dimension it adds keys on `applied_at`, this
plane's own record clock, because a bucket axis may not be another plane's
(`000012_analytics.up.sql:105`, §10.3). The column is available and refused on
the same grounds, not missing for want of an apply site.

`account_id` and the model are **not** recoverable from either plane without a
contract change, because the Data Plane's `public.requests` table (which has
both, at `:115` and `:117`) has **no cross-plane read path** —
`apps/dataplane-api/internal/adapters/inbound/http/` serves
`aliasgroups.go`, `projection.go`, `usageevents.go` and nothing else.

**Consequence for B16:** per-account analytics needs an account on the fact.
There are exactly two ways to get one, and this design takes the second.

| Option                                                  | Shape                                                                         | Why not                                                                                                                                                                                                                                                                                                                |
| ------------------------------------------------------- | ----------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| (a) Add `account_id` to `control.settlements`           | one column on the most consequential money table                              | `settlements` is referenced by FK from `applied_facts` and by the settlement leg algebra; it is read by reconciliation's `settled_without_settlement` and `settlement_total_mismatch` checks, whose scope ADR 0011 pins to the three founding subjects. Widening it is a signature change to the plane's money record. |
| **(b) A new `control.analytics_fact_dimensions` table** | one row per `(request_id, kind_class, account_id)`, denormalized, append-only | **Chosen.** Small, additive, reversible, and it is exactly the shape the read model needs — the denormalization is not a convenience, it is the only tenancy that can be made obviously correct (see §5).                                                                                                              |

Option (b) is populated by the **same transaction, on `txCtx`, in the same
`WithinTx`** as the settlement. It reads the account from the derivation that
is already being performed. No new cursor, no new loop, no new watermark.

The _model/alias_ dimension is **not** shipped in v1. It requires the B12
fact-contract v2 (the Data Plane's writer plus `usage-facts.yaml`), it is
**forward-only** (no existing row can be backfilled: `requests.alias` is the
name as sent, not the catalog id, and the fact carries no join key), and it is
not needed for the financial metrics B16 v1 serves. It is recorded in the
metric registry as an explicit exclusion rather than shipped as a wrong number.

### 1.2 A materialized aggregate of per-fact ceilings is not the ceiling of the total

`accounting.SettledAmount` (`internal/domain/accounting/derive.go:52-86`)
applies **one ceiling over the SUM of both raw arms**, and its own comment says
why: _"a per-arm ceiling rounds twice, and the half-minor-unit difference would
be a charge the reservation never held."_ The consumer **re-derives** the amount
from the fact's own counts and prices and refuses disagreement
(`interpret.go:320,339`).

So a per-fact amount is a **rounded** figure. Verified by running the
arithmetic (`ceil` over `priceScale = 1_000_000`):

```
3 facts x raw cost 1 each:   ceiling(3/1e6) = 1     sum(ceiling(1/1e6)) = 3
1000 facts x raw cost 1 each: ceiling(1000/1e6)=1  sum(ceiling(1/1e6)) = 1000   ← 1000x overcount
```

This makes the choice of aggregate source a **financial-correctness** decision,
not a convenience one.

**Decision: financial aggregates read `control.settlements.settled_total`,**
which is the Control Plane's _authoritative_ settlement of record — written
once at creation and never re-derived (`migrations/control/000006`, header at
`:199-200`). Summing settlements is additive with no rounding, and
`settlements.request_id` is `UNIQUE`, so a settlement is counted at most once
by construction. The reservation/hold figures, which are _not_ rounded per
request in the same way, come from the ledger's own cached balances where a
balance is what's wanted — never from a re-derived per-fact sum.

Every money aggregate carries `::bigint` on the `SUM`, copied verbatim from the
existing precedent at `adapters/outbound/postgres/accounting.go:845-847`.
PostgreSQL widens `SUM(bigint)` to `numeric`; the cast back is deliberate so an
overflow is a loud error at the driver instead of a silently-typed scan.

---

## 2. What is answerable, and from where

Reviewer A's census, cross-checked against the schema. **This table is the
metric registry's skeleton** and any metric not in it does not ship.

| Area           | Metric                                                                                                 | Numerator                                                     | Denominator / population                                                 | Source                                       | v1?                                                                                      |
| -------------- | ------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------- | ------------------------------------------------------------------------ | -------------------------------------------- | ---------------------------------------------------------------------------------------- |
| request        | requests with usage facts                                                                              | count of distinct `request_id` in `analytics_fact_dimensions` | the requests whose usage reached the account's capacity and was recorded | `control.analytics_fact_dimensions`          | **yes**                                                                                  |
| request        | requests settled                                                                                       | count of `settlements`                                        | requests with usage facts                                                | `control.settlements`                        | **yes**                                                                                  |
| request        | requests released / expired                                                                            | count of `applied_facts.kind`                                 | requests with usage facts                                                | `control.applied_facts`                      | **yes**                                                                                  |
| financial      | settled amount                                                                                         | `SUM(settlements.settled_total)`                              | — (absolute, not a rate)                                                 | `control.settlements`                        | **yes**                                                                                  |
| financial      | settled at zero                                                                                        | count of settlements where `settled_total = 0`                | **settlements** (not requests)                                           | `control.settlements`                        | **yes**                                                                                  |
| financial      | settlement rate                                                                                        | settled requests                                              | **requests with usage facts**                                            | derived                                      | **yes**                                                                                  |
| usage          | released amount                                                                                        | `SUM(ledger_entries.amount) WHERE kind='release'`             | —                                                                        | `control.ledger_entries`                     | **yes**                                                                                  |
| financial      | held (unconsumed)                                                                                      | `SUM(funding_buckets.held_amount)`                            | —                                                                        | `control.funding_buckets`                    | **yes**                                                                                  |
| financial      | available                                                                                              | `SUM(funding_buckets.available_amount)`                       | —                                                                        | `control.funding_buckets`                    | **yes**                                                                                  |
| commerce       | funds added                                                                                            | `SUM(amount) WHERE kind IN ('grant','topup')`                 | —                                                                        | `control.ledger_entries`                     | **yes**                                                                                  |
| usage          | settlement capture mix                                                                                 | count per `capture_method`                                    | **settlements with `kind='settled'`**                                    | `control.applied_facts`                      | **yes**                                                                                  |
| reconciliation | open findings                                                                                          | count where `status='open'`                                   | open findings — **not** a rate                                           | `control.reconciliation_findings`            | **yes**                                                                                  |
| reconciliation | facts quarantined                                                                                      | count                                                         | applied + quarantined facts                                              | `control.quarantined_facts`                  | **yes**                                                                                  |
| request        | **attempts, latency, error classes, retries, per-model spend, routing outcomes, provider mix, egress** | —                                                             | —                                                                        | `public.requests`, `public.request_attempts` | **NO — no cross-plane read path.** Recorded as exclusions, not shipped as zeros.         |
| request        | **token counts**                                                                                       | —                                                             | —                                                                        | `control.applied_facts` (they DO arrive)     | **NO — the counters are byte lengths, and the two families differ in width. See §10.2.** |
| request        | **rejection rate, admission rate**                                                                     | —                                                             | admitted requests                                                        | —                                            | **NO — the denominator is not derivable here. See §10.1.**                               |

**The denominators are the point.** A "rejection rate" whose denominator is
attempts is wrong (requests are admitted before any attempt exists); one whose
denominator is admitted requests is _also_ unavailable, for a reason that has
nothing to do with the cross-plane read gap and is settled by this plane's own
schema (§10.1). A "capture rate" whose denominator is all facts is wrong
(released and expired facts carry no capture method — the
`applied_facts_capture_shape` CHECK at `000008:122-124` enforces it), and worse,
a capture mix over _facts_ counts `unbillable_orphaned` facts, which carry a
capture method and book no charge; the population is **settlements**. A
"quarantine rate" whose denominator is applied facts only is a rate of nothing;
the population is **applied ∪ quarantined**.

**Zero is a value, not an absence.** `internal/domain/accounting/money.go:46-48`
already draws the line: _"a zero-priced model books a hold of nothing, settles
for nothing, and is still a settlement of record."_ Every metric with a
monetary or counting answer therefore returns a real `0` and never a null,
never an empty list standing in for a zero.

---

## 3. Result semantics — three states on two axes

Reviewer G's shape, adopted because it is the only one that keeps the repo's own
`0`-is-a-value invariant intact while still distinguishing failure:

| Axis                   | Values                                     | Transport                       |
| ---------------------- | ------------------------------------------ | ------------------------------- |
| **is there an answer** | `availability: available \| not_available` | 200, an explicit required field |
| **is there a failure** | the ErrorEnvelope                          | non-2xx, never a 200 body       |

A single `status` enum would collapse `0` and "no data" into one member, which
is exactly the confusion `usage-facts.yaml:138-142` was written to prevent.
`failed` is never an enum member because putting it there would require a 200 for
a failure, contradicting `errors.yaml:14-20` ("a code exists in the contract
before it can be returned") and the single error-translation point in
`server.go`.

`not_available` is an **answer, not a failure**: the Control Plane holds no
derived rows for that ACCOUNT at all — none in the queried range and none in any
other. It is distinct from a range that is simply quiet, which is `available`
with every bucket zero, and a caller renders it as an empty series and asks for
a later range.

The axis is the account's coverage and not the range's, and the mechanism is why:
the series statement LEFT-joins its bucket bounds, so every requested bucket
returns a row whether or not a fact landed in it — a range this plane has no rows
for and a range the account was quiet in are the same shape. Only a read of the
account's own derived rows can tell them apart, and the adapter makes that read
in the same statement and the same snapshot as the freshness instant
(`readFreshness`, where the position paragraph explains why the two ride
together).

---

## 4. Freshness — stated honestly, computed from the wrong thing

The honest watermark is _not available to this plane_:
`internal/application/reconciliation.go:1196-1205`
records that the cursor-lag check is deferred "to the day the feed contract
exposes a watermark", and the Data Plane does expose the number
(`usage_events_stream.last_seq`) but does not send it. The cursor itself is an
opaque string this plane is forbidden to parse.

So `data_through` in v1 is `control.ingestion_cursor.updated_at` — **and it is
labelled as what it is**: _"the instant the Control Plane last completed a pass
over the usage-fact feed"_, which is a **liveness** signal, not a
**completeness** one. The contract says so in the field's description, in the
polarity the repo already uses for instants that order nothing
(`usage-facts.yaml:239-242`): a reader can tell this is a record-time and not an
event-time, and cannot mistake one for the other.

A `freshness: stale` marker is deliberately **absent** rather than guessed: the
plane cannot distinguish "the Data Plane stopped emitting" from "the Control
Plane is caught up" without a watermark, and a marker that cannot fire honestly
is worse than none. Recorded as the B12-lineage dependency that would make it
computable.

---

## 5. Tenancy — the security core

**Server-side authorization is the authority.** The account scope is derived
from the authenticated principal and is **not a request parameter**. There is no
`?account_id=` on any operation, so cross-account filter tampering has no
surface to tamper with. The scope conjunct is written into the statement at
construction time — there is no code path where a `WHERE` is built without it.

The read model is **multi-tenant from day one**: analytics serve a _principal_,
and the principal's account set is a server-side derivation. Single-tenant
scoping is an implementation-only narrowing behind that predicate, never a
parameter and never a contract change. This is strictly safer than shipping
single-tenant and widening later, because widening a _server-side_ predicate is
invisible to clients while adding a query parameter later is a public contract
change that every client can observe.

**Why the account must be denormalized.** `funding_buckets.account_id` is
**nullable** (`migrations/control/000006:130-131`) with
`CHECK ((entitlement_id IS NULL) <> (account_id IS NULL))` at `:148` — so every
subscription-entitlement bucket has `account_id = NULL`, and
`UNIQUE (account_id)` at `:156` is not a backstop because PostgreSQL permits many
NULLs. A read model that filtered on `funding_buckets.account_id` would
**silently drop every entitlement-funded row** — a figure that is wrong in the
direction that looks like good news.

---

## 6. Boundedness

Every bound is a **refusal**, never a clamp: a clamped answer is
indistinguishable from a real page, so the caller "could learn about the mismatch
nowhere" (`usage-facts.yaml:78-91`).

| Bound            | Value                                                                                                                | Where enforced                              |
| ---------------- | -------------------------------------------------------------------------------------------------------------------- | ------------------------------------------- |
| Max range        | 90 days                                                                                                              | contract description; `400 invalid_request` |
| `from`/`to`      | required, half-open `[from, to)`                                                                                     | `400`                                       |
| Granularity      | closed enum `hour \| day \| calendar_month`                                                                          | `400`                                       |
| Max series items | 2161 (90 days × 24 h, plus the bucket containing an unaligned start) — the natural cap, stated rather than asked for | contract `maxItems`                         |
| Dimensions       | **no client-named `group_by` at all** (see below)                                                                    | —                                           |
| Query timeout    | 5 s at the application layer + `statement_timeout` in the read's own transaction                                     | maps to `503`                               |
| Page size        | n/a — aggregate series are bounded by `maxItems`                                                                     | —                                           |

**No client-named dimension.** Each granularity is a _pre-written query_ in the
adapter, chosen by an allowlist of `const` statements. A `?group_by=` parameter
would make this module's SQL client-influenced for the first time, and no arch
rule in `internal/arch/` protects query text — only imports. The dimension
vocabulary is fixed at `(granularity × metric)` in v1 and grows by adding a
statement and a contract row.

**A read pool.** `console-api`'s pool is `MaxOpenConns = 10`
(`internal/config/config.go:56`) and the projection, ingestion and
reconciliation loops plus `/readyz` all share it. Five concurrent 30-day
dashboard queries would starve the **settlement path**. Analytics therefore get
a **separately-configured read pool** with its own sizing and its own
`application_name`, and a test proves that saturating the read pool leaves the
ingestion loop's pass latency unchanged.

---

## 7. Read-back only — no rebuild writer

Analytics is derived and reconstructable, so the **rebuild path is a read from
authoritative state**, not a second writer. `control.settlements`,
`control.applied_facts` and `control.ledger_entries` are all retained forever
and are append-only-guarded, so the aggregate is always reconstructable.

This is not a shortcut; it is the shape that removes the entire class of
hazards the reviewers named:

- **No second writer, therefore no I3 race.** A rebuild writing buckets
  concurrently with ingestion is the one concurrency defect that is silent
  rather than loud, and there is no existing precedent for a background writer
  to a Control-Plane table.
- **No double count is structurally possible.** The source set is deduplicated by
  `settlements.request_id UNIQUE` and `applied_facts` PK `(request_id,
kind_class)`. There is no arithmetic in which the same fact contributes twice.
- **The attribution key carries the account, and must.** A settlement's
  allocation tail can draw on buckets owned by more than one account — the
  waterfall plans per bucket, and `funding_buckets_owner_xor` constrains a
  BUCKET's owner, never a FACT's set of them — so a key of `(request_id,
kind_class)` would record the first account and drop the rest through
  `ON CONFLICT DO NOTHING`, leaving each dropped account's report short by
  exactly the requests it funded. The key is `(request_id, kind_class,
account_id)`: one row per (fact, account), and a redelivery still converges
  to a no-op exactly as it does in `applied_facts`.
- **The two money routes resolve the account differently, and that is
  deliberate.** `settled` is scoped through `analytics_fact_dimensions`
  (a settlement header names a _request_, and the request is what carries the
  attribution); `released` and `funds_added` are scoped through the account's
  bucket set, both arms of it (a ledger leg names a _bucket_, and the bucket is
  what carries the ownership). Neither route could be expressed as the other:
  the dimensions say who was charged, and only the bucket set can say who paid.
  The consequence is that the two are reconciled by no constraint on this
  plane, so a test asserts they name the same account — one request settled
  entirely out of an entitlement cycle, read back through all three figures.
  A request attributed to an account whose bucket was not its own, or a
  settlement visible to the dimensions whose buckets belonged to someone else,
  would show up as a settled amount no flow supports.
- **The `Converged` page-stop becomes unreachable.** `factapplier.go:306-316`
  stops the page when the ledger already holds a settlement; a rebuild that
  never calls `Settle` cannot reach it. Any design routing rebuild through the
  settlement use case is broken on day one.
- **Byte-identity is a test assertion, not an engineering project.**

---

## 8. Corrections

The correction path is **reserved in the Data Plane and refused in the Control
Plane** (`ErrCorrectionUnsupported`; `interpret.go:279-281`). B16 v1 keeps it
refused. A quarantined fact is a row the aggregate never saw, so this costs
nothing and forecloses nothing — and a correction, when it arrives, is a _new_
fact at a new `append_seq` that the source-set arithmetic picks up with no sign
convention and no interaction with the single-axis adjustment algebra.

**B16 must never become the layer that discovers it can reverse a settlement.**
That would make the analytics read model a second accounting engine, which is
the one thing ADR 0011 explicitly refuses.

---

## 9. What B16 does not do

The non-goals, restated as the concrete items the reviews surfaced:

- no operational analytics (attempts, latency, error classes, routing, provider,
  egress) — no cross-plane read path exists, and inventing one is a wire
  contract change, not an analytics feature
- no token metrics — and the reason is stronger than "not derivable" (§10.2)
- no per-model or per-provider spend — forward-only, and it would need B12's wire
- no rebuild writer, backfill endpoint, or export file — **and this one has a
  consequence a reader has to be told rather than left to discover**:
  `control.analytics_fact_dimensions` is created empty by `000012` and nothing
  populates it except the ingestion transaction of a fact applied _after_ that
  migration. Facts this plane applied before it — every fact in `applied_facts`
  today — carry no attribution and never will. A report run the day after
  `000012` answers "no derivations" for an account that has been settling for
  months, and that answer is indistinguishable at the surface from a new
  account's: `AccountHasDerivations` is a statement about this table, not about
  the account's history. There is no backfill in B16 because a backfill is a
  second writer to a Control-Plane table (§7) and reconstructing the account of
  a past fact means re-reading an allocation tail whose buckets may since have
  been deleted. The gap closes only by time passing: the difference between "no
  derivations" and "we were not recording then" is a question about the ledger,
  and this surface does not answer it — it has no settlement read at all, which
  is why the exclusion is recorded here rather than left for a reader to infer
  from an empty chart.
- no currency value — issue #63; amounts stay integer minor units
- no audit log — read access is **not** recorded, and no documentation may imply
  that server-side scope is accountability
- no `identity.Secret`, no raw prompt, no `quarantined_facts.payload`, no proxy
  URL, no request id as a dimension

---

## 10. What the second review pass corrected

The first draft was written before the contract existed; a second adversarial
pass over the written design and the finished contract found four things wrong.
They are recorded here rather than folded silently into the sections above,
because three of them change a number's name and one changes a bound.

### 10.1 The request population is not derivable here, and the repo already said so

The first draft published `requests_admitted` and used it as the denominator of
`requests_settled`. That figure is **not derivable in this plane**, and the
argument is not the cross-plane read gap — it is this plane's own schema.
`internal/application/reconciliation.go:1263-1288` records it in the B13 pass:

> "A request this plane has no usage fact for" is undecidable from this plane's
> own rows, and the only request-id population derivable here is
> `applied_facts ∪ quarantined_facts`. That population is self-fulfilling: a
> request with no fact produces no row in either table, so the query that
> "finds" it finds nothing, and a detector built on it would report on the set
> of requests this plane knows about and call it the set of requests.

B13 refused to publish a coverage figure for exactly this reason. B16 publishing
one under the name `requests_admitted` would have been the same error with a
friendlier label — a number that reads as the denominator of every request
metric and measures the set of requests the Control Plane was told about.

**Corrected:** the field is `requests_with_usage_facts`, defined as a request
whose applied fact names at least one of the account's funding buckets. The
settlement rate it denominates is still a real rate with a real meaning — the
share of this account's fact-producing requests that reached a charge — and it
is named as what it is. A rejection rate, an admission rate and an error rate
are recorded as **excluded**, because their denominators are the population this
plane does not have.

The second reason this matters, which is a _tenancy_ finding as much as a
metric one: the account is attached to a fact through its **funding buckets**.
A request whose fact draws on no bucket of the account belongs to no account,
and a query that reported it anywhere would be a report about a request the
account did not pay for. Making the account dimension part of the fact row's
definition, rather than a column added to the read, is what makes that
impossible to get wrong.

### 10.2 The token counters are byte lengths, not tokens

The first draft excluded token metrics as "not derivable from Control-Plane
state". True, but incomplete, and the weaker reason is the one that would have
let someone "fix" it later. The counters are:

```go
func CountInputTokens(body []byte) int64      { return int64(len(body)) }
func CountDeliveredTokens(delivered []byte) int64 { return int64(len(delivered)) }
```

`apps/dataplane/internal/domain/catalog/tokenize.go:39` and
`apps/dataplane/internal/domain/accounting/deliver.go:16`. They are **byte
lengths** with a temporary tokenizer's signature, documented as such at
`tokenize.go:28-30` (the canonical rule, "shared with the delivery counter in
the accounting package: byte length now, a real tokenizer later") and at
`deliver.go:11-13` ("A real tokenizer, when one arrives, replaces the bodies of
both counters with their signatures unchanged"). Publishing them under the name
`token_count` would be a
category error that is _invisible in the output_ — an integer is an integer.

Compounding it: the counters' two families have **different widths in the
schema**. `public.requests.input_tokens` and `reservations.input_tokens` are
`integer` (int32, `migrations/dataplane/000003:118,338`); the priced
`usage_events.*_tokens` are `bigint` (`:444-446`). A future join between them
would silently report the admission basis as the settled figure.

**Corrected:** the registry records the exclusion as "the counters are bytes and
the two families are different widths", so the exclusion cannot be read as an
oversight to be fixed by a join once a real tokenizer lands.

### 10.3 The money clock was unnamed, and nothing closes a period

The first draft got _freshness_ right (§4) and said nothing about
**attribution** — which instant places a figure in a bucket. Three clocks are in
play and only two of them are this plane's:

| Instant                                                | Whose clock                                  | Use                                                            |
| ------------------------------------------------------ | -------------------------------------------- | -------------------------------------------------------------- |
| `usage_events.occurred_at`                             | the Data Plane process                       | **telemetry only, never a key** (`fact.go:94-95`)              |
| `applied_facts.applied_at`                             | this plane's ingestion                       | dispositions (`requests_with_usage_facts`, `requests_settled`) |
| `settlements.created_at` / `ledger_entries.created_at` | **the database** (`transaction_timestamp()`) | money                                                          |

B13 settled the general rule in its own words at
`internal/adapters/outbound/postgres/reconciliation.go:567-573`:
_"The bound is applied_at — when this plane recorded the derivation — and not
the fact's occurred_at, which is the Data Plane's clock: ordering one plane's
records by another plane's timestamps is how a modest clock skew becomes a fact
applied outside the window it was applied in."_

And the consequence the first draft missed: **nothing in this system closes a
period.** No accounting-period table, no `period_closed_at`, no lock. The
`reconciliation_runs.window_from/window_to` are the worker's own sweep windows,
half-open for tiling — not financial periods and never to be presented as such
(`migrations/control/000009:31-36,134-135`). So a late fact silently changes a
figure already reported, and a re-run of the same range may return a different
number.

**Corrected:** the contract names the axis per metric family, and adds
`final_bucket_partial: boolean` — a fact about the _range_, structurally
computable, so "this last bucket has not finished filling" is never a guess.
Combined with `freshness.data_through`, a caller can tell a provisional number
from a stable one without asking.

### 10.4 The rounding result is a proof, not a preference

§1.2 recorded the finding; the second pass asked for it as a theorem, and the
theorem is the reason the whole design is shaped this way. Let `N` be the facts
in a range, each with raw cost `c` (an exact integer in price-scaled units) and
`priceScale = 1_000_000`:

- the truth is `ceil((Σc) / priceScale)`
- summing per-fact ceilings gives `Σ ceil(c / priceScale)`
- for every `N ≥ 2` with `0 < c < priceScale`, the second is **strictly
  greater** than the first, and the gap `Σ ceil(c/priceScale) − ceil(Σc/
priceScale)` grows **linearly in N** while the truth grows sub-linearly.

There is no volume at which this becomes safe, and it is not fixable by
precision: the ledger stores the _rounded_ amount and the consume leg's price
snapshot carries no token counts (`ledger.go:59`), so the raw cost is
**unrecoverable from Control-Plane state**. Summing per-fact amounts is not
merely inexact — it is the only thing the ledger permits, and it is wrong by
construction.

`control.settlements.settled_total` is exempt because it is the sum of that
settlement's own consume legs, written **once** at creation by the builder that
wrote them (`settlement.go:175`, migration comment `:199-200`). Summing
integers is exact and associative, so the aggregate-of-aggregates equals the
aggregate-of-originals. The same argument is what makes `held`/`available`
publishable from the **cached** balances: a balance is a running exact
accumulation of signed leg deltas, never a sum of rounded per-request figures.

**The corollary, and it is the reason this is stated as a rule rather than a
note: the two families must never share a query, a helper, or a Go accumulator.**
One is a sum of exact integers; the other is a sum of point-in-time balances
with the opposite bucketing semantics (a flow is bucketed, a balance is not). A
reader who "helpfully" unifies them introduces a factor-of-N error into the
money.

### 10.5 The balances are read at the read, not at the range's end

§2 shipped `held` and `available` as point-in-time figures and said "as at the
range's end", which was wrong in a way a caller could act on. The accounting
projection **caches the account's present balance and keeps no history**, so a
range that ended in June is answered with the balance the account holds today.
There is no as-of read that could answer anything else: the legs are
append-only, but the balance columns are a mutable cache of the present, and
reconstructing a past balance would mean re-deriving from the leg stream — a
rebuild, which this plane refuses (§7).

The correction is a change of vocabulary in three places and a change of
meaning in none of the code. The field docs in the contract
(`api/openapi/shared/analytics.yaml`), the domain type
(`internal/domain/analytics/usage.go`) and the port
(`internal/ports/outbound/persistence/analytics.go`) now say "as at the moment
the READ ran". The consequence the old wording hid is the one a caller has to
be told: **two historical ranges compared side by side are one balance read
twice**, and a report that presents them as a trend is presenting the read
clock rather than the money.

It is also why the balances are reported once, beside the series rather than on
every point. A per-point balance would have implied it was true of each bucket;
it is true of none of them.

---

## 11. Migration shape

One control migration, `000012_analytics`:

| Object                                 | Why                                                                                                                                                             |
| -------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `control.analytics_fact_dimensions`    | `(request_id, kind_class, account_id)` PK, `account_id uuid NOT NULL`, one row per (applied fact, account), written on `txCtx` inside the ingestion transaction |
| index on `(account_id, applied_at)`    | the read's only access path, and the one the §6 bounds are stated against                                                                                       |
| index on `settlements (created_at)`    | **required**, see below                                                                                                                                         |
| index on `ledger_entries (created_at)` | **required**, see below                                                                                                                                         |

**The two new time indexes are not an optimization, they are the difference
between a working surface and a dead one.** Verified across every migration in
the lane: the only indexes on `ledger_entries` are the three partial uniques on
`(funding_bucket_id, …)` and the PK. `created_at` is indexed **nowhere**. A
90-day money series is therefore a sequential scan of the entire ledger, and
because the range bound is a _constant_ 90 days while the table grows without
bound, report latency grows without limit against a fixed `statement_timeout`
— the surface fails closed forever, which is safe and useless.

`applied_facts` already has `applied_facts_applied_at_idx` (`000009:307`), so
the disposition axis is covered; the two money axes are the gap.

**`verify.sh` co-edit.** The script pins the control schema to exactly 23 tables
with an exact alphabetical list (`verify.sh:507-511`). A new table requires
that list and its count to be updated in the same diff, or the lane verify
fails. That is the point of the pin, and this is the change it is asking for.
