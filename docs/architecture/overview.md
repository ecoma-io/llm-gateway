# Gateway domain model — overview

This is the reference model of the gateway's business domains. It is the
picture an engineer needs before writing `migrations/` or
`api/openapi/openapi.yaml`: what exists, what owns what, and what words mean.
The decisions behind it are recorded as ADRs and cited throughout; when this
page and an ADR ever disagree, the ADR wins and this page is wrong.

| Decision                                                                                                         | ADR                                                                  |
| ---------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------- |
| Five bounded contexts; aggregate and transaction boundaries                                                      | [ADR 0001](../adr/0001-bounded-contexts-and-aggregates.md)           |
| Routing pipeline; router/adapters/egress ownership; commitment gate; v1 client protocol (chat completions); Kilo | [ADR 0002](../adr/0002-routing-and-fallback-ownership.md)            |
| Concurrent subscriptions, entitlement waterfall, PAYG                                                            | [ADR 0003](../adr/0003-concurrent-subscriptions-and-entitlements.md) |
| Reserve/settle accounting and its ten invariants                                                                 | [ADR 0004](../adr/0004-reserve-and-settle-accounting.md)             |
| PostgreSQL relational vs Timescale-oriented event storage                                                        | [ADR 0005](../adr/0005-relational-and-event-storage-split.md)        |

Reference pages: [routing](routing.md) · [request lifecycle](request-lifecycle.md)
· [commerce](commerce.md) · [accounting](accounting.md) ·
[data implications](data-implications.md)

## The five contexts

```text
                    ┌──────────────────────────────────────────────────┐
                    │                    Identity                      │
                    │   Account ─ User ─ APIKey                        │
                    └───────────────┬──────────────────────────────────┘
                                    │ api key identifies account
  client ── request ────────────────▼────────────────────────────────────────┐
                                    │                                        │
                    ┌───────────────▼───────────┐          ┌─────────────────▼──────────┐
                    │         Catalog           │          │         Execution          │
                    │  ModelAlias ─ Candidates  │────────▶ │  Request ─ RequestAttempt  │
                    │  Backend ─ Adapter        │ serves   │  commitment state          │
                    └───────────────┬───────────┘          └─────────────────┬──────────┘
                                    │ alias scope                          │ usage facts
                    ┌───────────────▼───────────────────────────────────────▼──────────┐
                    │                    Commerce ⇄ Accounting                     │
                    │  Plan ─ Subscription ─ Entitlement   Reservation ─ Settlement   │
                    │  PriceList revisions ─ PAYG          FundingBucket ─ LedgerEntry │
                    │                                      UsageEvent                 │
                    └────────────────────────────────────────────────────────────────┘
```

One request's journey through them is specified step by step in
[request lifecycle](request-lifecycle.md); the four cross-context coordinated
transactions (admission, settlement, release/compensation, grant-cycle roll)
are ADR 0001, rule 6.

## Entity catalog

Field sketches below are semantic, not DDL: they name what must exist and be
queryable, not column types. `id` is implied on every entity. The storage
family split is decided in
[ADR 0005](../adr/0005-relational-and-event-storage-split.md) and mapped
table-by-table in [data implications](data-implications.md).

### Identity

| Entity  | Fields (beyond `id`)                                                                                                      | Notes                                                                                                                                                                                                                                                                                                 |
| ------- | ------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Account | name, state (`active` \| `suspended` \| `closed`), PAYG enabled (bool), created_at                                        | The billing and ownership boundary. Amounts are integer minor units in the single platform-wide settlement currency (ADR 0003); per-account currency is an open question. The PAYG flag lives here; the bucket it enables exists from account creation (ADR 0003) and is Accounting-owned (ADR 0001). |
| User    | account_id, identity fields (email …), state (`invited` \| `active` \| `removed`)                                         | Console principal. Belongs to exactly one account (ADR 0001, rule 2). Not on the data-plane path.                                                                                                                                                                                                     |
| APIKey  | account_id, created_by (user id, nullable), key hash, display name, state (`active` \| `revoked`), revoked_at, created_at | The only data-plane credential. Authentication is by key hash lookup → account. Revocation takes effect immediately. Multiple keys per account. Key scoping (per-key limits) is an explicit open question — see below.                                                                                |

### Catalog

| Entity                  | Fields                                                                                                                                                                                                     | Notes                                                                                                                                                                                                                                                                                                                                           |
| ----------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| ModelAlias              | logical name (unique across active **and** retired — names are never reused), state (`active` \| `retired`), max_output_tokens limit, reservation cap, candidates (ordered 1..n)                           | Gateway-global, operator-managed. The only model identifier a client ever sees (ADR 0002). Alias + candidates form one aggregate. The output limit validates client requests; the reservation cap bounds hold sizing — both live on the alias because they apply before a candidate is chosen. Admission to a retired alias is `unknown_alias`. |
| ModelCandidate          | alias_id, position, backend_id, provider model id, optional per-candidate parameter overrides                                                                                                              | Order = fallback order; position 1 is primary. Referenced by attempts.                                                                                                                                                                                                                                                                          |
| Backend                 | adapter type (`openai-compatible` \| `anthropic` \| …), endpoint, credentials ref, egress policy ref, state (`active` \| `disabled`)                                                                       | A configured instance of an adapter (ADR 0002). "Kilo" is one row here with adapter type `openai-compatible` — nothing more.                                                                                                                                                                                                                    |
| AliasGroupVersion       | group name, version, immutable member alias set                                                                                                                                                            | Entitlements reference the version they were granted with; editing membership creates a new version (ADR 0003). `*` is the singleton wildcard version.                                                                                                                                                                                          |
| ClientPriceListRevision | version, effective_from, state (`draft` → `activated`; drafts editable, activation freezes it), one price entry (input + output unit price, integer minor units per 1,000,000 tokens) **per active alias** | Alias-exact pricing — no group precedence rule to invent (ADR 0003). The effective revision at any instant is the activated one with the greatest `effective_from` not after it; activated `effective_from` values are unique, so selection is total. Requests snapshot the effective revision at admission.                                    |

### Execution

| Entity             | Fields                                                                                                                                                                                                                                                 | Notes                                                                                                                                                                                                                                                                                                                     |
| ------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| RequestIntake      | account_id, idempotency key (unique per `(account, key)`, permanent), canonical request digest, request_id, terminal status pointer                                                                                                                    | The relational replay record (ADR 0004): created in the admission transaction, immutable afterwards. Replay decisions read **only** this table — never the event family (ADR 0005). A replay returns outcome metadata; re-serving a stored response body is an open question.                                             |
| Request            | account_id, api_key_id, alias, canonical bounds (input tokens, max_output_tokens), price revision snapshot, admitted_at, finished_at, status (`executing` \| `succeeded` \| `failed` \| `rejected`), rejection reason, committed_attempt_id (nullable) | The logical request. Event-family record (ADR 0005): created as a shell at admission (including rejections that pass account identification), finalised exactly once — by settlement or by release. Replay lives on RequestIntake; the reservation references the request, never the reverse.                             |
| RequestAttempt     | request_id, candidate position + backend_id + provider model id, provider request id, started_at, finished_at, outcome, error class, provider usage telemetry, delivery tokens observed, retry sequence within candidate                               | One row per upstream call, retries included (ADR 0002); unique per `(request_id, candidate_position, retry_sequence)`. Outcome: `succeeded` \| `failed_before_commitment` \| `failed_after_commitment`. Provider-side cost telemetry (including usage reported after a disconnect) lives here, never in customer billing. |
| Commitment (state) | not an entity — a point in time: first **content-bearing** byte forwarded to the client (text or tool-argument deltas; pre-content lifecycle frames are buffered)                                                                                      | The fallback gate (ADR 0002) and the billing subject selector (ADR 0004, invariants 6–7). Recorded implicitly by attempt outcomes.                                                                                                                                                                                        |

### Commerce

| Entity       | Fields                                                                                                                                                                                                                                                           | Notes                                                                                                                                                                                                                                                                                                                                                    |
| ------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Plan         | name, versions each carrying a monotonic `version_number` and state (`draft` \| `published` \| `retired`); a version: recurring price, billing period, grant definitions [{stable grant_definition_id, alias_group_version, dimension (`cost`), amount}]         | A template; grants nothing alone. Drafts are editable; published versions are immutable; retiring stops new subscriptions only. A subscription pins its version forever. One grant definition per `(alias_group_version, dimension)` per version — duplicate grants are unrepresentable (ADR 0003).                                                      |
| Subscription | account_id, plan_id + plan version, state (`pending` \| `active` \| `suspended` \| `cancelled` \| `expired`), start_at, cycle_number, period_start/period_end, renewal_enabled, cancel_at (nullable), cancellation_mode (`scheduled` \| `immediate`), created_at | Lifecycle in [commerce](commerce.md). Scheduled cancellation is data (`cancel_at`), not a state. **No account field names a "current" subscription** — concurrency is the point (ADR 0003). Same plan subscribed twice is legal; they never merge.                                                                                                       |
| Entitlement  | subscription_id, cycle_number, grant_definition_id, alias_group_version, dimension (`cost`), granted amount, state (`active` \| `expired`), period_start/period_end                                                                                              | Unique per `(subscription, cycle_number, grant_definition_id)`. Scope is the immutable group version pinned at the cycle's roll (ADR 0003). There is no suspended entitlement — suspension is subscription state, applied at admission. Capacity is guarded by its funding bucket — an Accounting aggregate the entitlement references by ID (ADR 0001). |
| PAYG balance | the account's PAYG funding source — an enablement flag plus its bucket row, not a separate table concept                                                                                                                                                         | A funding source, never a subscription (ADR 0003; ADR 0004, invariant 9). The flag is this context's; the bucket row is Accounting-owned (ADR 0001).                                                                                                                                                                                                     |

### Accounting

| Entity        | Fields                                                                                                                                                                                                                                                                                                                | Notes                                                                                                                                                                                                                                                                                                                               |
| ------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Reservation   | request_id (**unique** — exactly one reservation per request, ever), price revision snapshot, canonical bounds, reserved_amount, state (`open` \| `settled` \| `released` \| `expired`), allocation legs [{funding_bucket_id, amount, ordinal}], created_at, expires_at, lease_owner + lease_expires_at (renewable)   | One per request (ADR 0004, invariant 6); the uniqueness is total, not state-filtered. The lease is two fields renewed by the live executor; TTLs are deployment tunables, strictly outliving the maximum accepted request duration. The ceiling is hard: adapters enforce the output bound, so settlement never allocates "excess". |
| Settlement    | request_id (**unique** — the exactly-once boundary), settled_total (sum of consume legs, written once), created_at                                                                                                                                                                                                    | The header its ledger legs hang from; unique per request, so double settlement is a rejected write, not a double charge (ADR 0004). Nothing on it is ever mutated.                                                                                                                                                                  |
| FundingBucket | entitlement_id **or** account PAYG, settled_amount, held_amount, available_amount, version                                                                                                                                                                                                                            | The lockable capacity projection every allocation conditionally updates (ADR 0004) — an Accounting aggregate (ADR 0001); the Commerce entitlement cycle or PAYG flag it projects references it by identifier. Cache maintained in-transaction; rebuildable from legs; the legs win any disagreement.                                |
| UsageEvent    | request_id, settlement_id, committed attempt id, normalized input/output token counts, alias id + name (snapshot), price revision snapshot, capture method (`reported` \| `gateway_observed` \| `reservation_floor`), classification (`billable` \| `unbillable_orphaned`)                                            | Immutable fact (invariant 1). Event-family table. `unbillable_orphaned` records a proven post-expiry completion for reconciliation — never charged, written with the reaper's release or by later reconciliation. Corrections are new events referencing these, with compensating legs.                                             |
| LedgerEntry   | funding_bucket_id, kind (`grant` \| `topup` \| `hold` \| `release` \| `consume` \| `adjustment`), positive amount, per-bucket `sequence`, price revision snapshot (required on `consume`; null otherwise), reservation_id or settlement_id, unique `(settlement_id, funding_bucket_id, kind)` where settlement-driven | A **bucket leg**, never an overloaded header (ADR 0004). The `sequence` is allocated by incrementing a counter on the bucket row in-transaction. Append-only (invariant 2). Balances are the formal projections (settled/held/available), cached on the funding bucket.                                                             |
| Adjustment    | pair of LedgerEntries with stated settled/held deltas, reason, original entry reference, operator identity                                                                                                                                                                                                            | Explicit operator correction only — never an automatic overdraw path (ADR 0004; the reservation ceiling makes automatic debt unrepresentable).                                                                                                                                                                                      |

## Glossary

Canonical terms; a synonym used where a canonical term exists is a defect
(ADR 0001).

| Term               | Meaning                                                                                                         | Avoid saying                     |
| ------------------ | --------------------------------------------------------------------------------------------------------------- | -------------------------------- |
| Account            | The billing/ownership boundary; owns everything below it.                                                       | tenant, workspace, organisation  |
| User               | Human console principal; one account.                                                                           | member                           |
| API key            | Data-plane credential of an account.                                                                            | token, virtual key               |
| Model alias        | The logical model name clients request; resolves to ordered candidates.                                         | model group, deployment          |
| Candidate          | One (backend, provider model) entry in an alias's ordered list; position is fallback order.                     | provider, target                 |
| Backend            | A configured adapter instance (endpoint, credentials, egress policy).                                           | provider, upstream               |
| Adapter            | The protocol-translation implementation a backend uses (`openai-compatible`, `anthropic`, …).                   | connector                        |
| Egress             | Outbound network layer (proxy pools) below adapters; not a domain concept.                                      | proxy, route                     |
| Request            | The logical client request; the billing subject. Created at admission, finalised once.                          | call                             |
| Attempt            | One upstream call, retries included; never bills independently.                                                 | request (as provider call)       |
| Commitment         | First **content-bearing** byte forwarded to the client; the fallback gate.                                      | first token, first byte (bare)   |
| Plan               | Commercial template with grant definitions; versions are immutable.                                             | package, tier                    |
| Subscription       | An account's instantiation of a plan version, with its own grant cycles.                                        | current plan                     |
| Grant cycle        | One period instance of a subscription (`cycle_number`); entitlements are granted per cycle.                     | billing period (on entitlement)  |
| Entitlement        | A cost-denominated quota grant scoped to an alias-group version for one grant cycle.                            | budget, quota (bare)             |
| Funding bucket     | The lockable capacity projection behind an entitlement or the PAYG balance; an Accounting aggregate (ADR 0001). | balance column                   |
| Allocation         | A reservation's leg against one funding bucket, with its waterfall ordinal.                                     | deduction, spend                 |
| PAYG               | Account-level prepaid funding source consumed at alias-exact client prices.                                     | pay-as-you-go plan, credits plan |
| PriceList revision | An immutable, alias-exact client price table; requests snapshot the effective revision at admission.            | pricing, rate card               |
| Reservation        | Pre-execution hold and hard execution ceiling; `open → settled \| released \| expired`.                         | authorization, hold (bare)       |
| Settlement         | The unique-per-request accounting header; its transaction consumes per bucket and releases the tail.            | charge, finalization             |
| Usage event        | Immutable record of actual usage for a settled request, with its capture method.                                | meter event, record              |
| Ledger entry       | One append-only bucket leg of a reservation or settlement.                                                      | transaction, posting             |
| Adjustment         | An explicit operator correction: compensating legs with reason and original reference.                          | refund (bare), edit              |

Where the industry uses other words, the mapping is (LiteLLM / Stripe /
TigerBeetle / Lago vocabulary → our canonical term): virtual key → API key ·
model group / `model_name` → model alias · deployment → candidate ·
credit grant / wallet → entitlement, PAYG balance · pending transfer /
authorization hold → reservation · post / void → settlement / release ·
meter event → usage event · ledger transaction → ledger entry. Docs and code
use the canonical term; equivalents belong in comments at most.

## Explicit open questions

Deliberately undecided here; each needs its own decision before the surface
it touches is built:

1. **API key scoping** — whether a key can be constrained (per-alias, per
   spend ceiling) beyond identifying its account. The model above treats a
   key as account-identifying only.
2. **Adjustment authorisation** — `adjustment` legs exist and are
   operator-only by definition; who may authorise one, and with what
   approval flow, is a process decision.
3. **Rollover** — whether a future plan version may carry unused entitlement
   capacity into the next grant cycle (today: never, ADR 0003).
4. **Multiple ledger currencies** — v1 settles in one platform-wide currency
   (ADR 0003); per-account currency, conversion, or multi-currency balances
   are out of scope until a market demands them.
5. **Per-plan spill policy** — today PAYG spill is account-level (ADR 0003);
   a plan attribute meaning "hard-capped: never spill to PAYG" is a named
   future extension, not part of this model.
6. **Operator-set allocation priority** — the waterfall's order is derived
   and total (ADR 0003); an explicit operator-facing priority field would be
   a policy feature on top, decided when a real customer shape demands it.
7. **Body replay on idempotent retry** — a same-digest replay returns outcome
   metadata (ADR 0004); whether a stored completed response body may also be
   re-served is undecided (storage cost, content staleness, streaming).
8. **Management-plane authentication** — the console/operator surface that
   writes accounts, plans, aliases, prices, and adjustments needs its own
   authentication and authorisation model; this ADR set governs the
   data-plane domain only and does not decide it.
