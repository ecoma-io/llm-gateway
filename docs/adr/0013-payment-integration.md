# ADR 0013: Payment integration — what has financial authority, and what merely has a browser

- Status: Superseded by [ADR 0014](0014-sepay-bank-transfer.md)
- Date: 2026-09-28
- Issue: [#128](https://github.com/ecoma-io/llm-gateway/issues/128)
- Superseded by: [ADR 0014](0014-sepay-bank-transfer.md) (2026-09-29) — the
  provider and the instrument changed: a hosted checkout session became a bank
  transfer to a provider-minted virtual account, so the mechanism this record
  describes is no longer what ships. Its doctrine is unchanged and is not
  renegotiated; the reasoning below stands as the record of why.
- Builds on: [ADR 0001](0001-bounded-contexts-and-aggregates.md), [ADR 0004](0004-reserve-and-settle-accounting.md), [ADR 0006](0006-control-plane-and-data-plane.md), [ADR 0008](0008-console-sign-in-identity.md), [ADR 0012](0012-management-console.md)

## Context

The Control Plane can move money and cannot acquire any. `Accounting.TopUp`
exists, is tested, and has exactly one caller: a surface an operator runs by
hand. A customer who wants to fund their own account has no path to do it, and
`internal/ports/outbound` has three directories, none of which talks to the
outside world. B14 gave the console a screen for every domain that existed; the
commercial loop it renders has no way in.

This record establishes the way in: a hosted-checkout integration with an
external payment provider, whose signed server-to-server webhook is the only
thing in this system with the authority to credit an account.

Seven adversarial reviews ran before any code was written. They did not agree
about the shape of the change. They agreed, independently, on the following,
and it is why this record exists rather than a design comment in the pull
request.

### What the reviews found, stated as the code sees it

**1. The obvious implementation funds a customer twice, and no existing test
would notice.** A provider emits several distinct events for one payment —
`payment_intent.succeeded`, `charge.succeeded`, a later re-notification under a
fresh delivery id. Each carries a different event id. A credit keyed on the
delivery identity, or on nothing at all, writes one ledger leg per delivery, and
every one of those legs is individually legitimate: right bucket, right amount,
right command-key scope. B6's `ledger_entries_bucket_command_key` would not
object, because each command key would be unique and therefore not a duplicate.
The double credit is invisible to every check the repository already has.

**2. A timestamp is not a replay defence, and the brief that proposed one
almost shipped it.** Signature verification with a tolerance window is the
standard advice and it is a bound on staleness, not on replay: an attacker who
captures one delivery resends it freely for as long as the window is open, and
the provider re-delivers by design anyway. The defence has to be durable state.

**3. `TopUp` refuses an entitlement bucket, and that is load-bearing rather
than incidental.** `domain/accounting` states it as a definition — "Topup is a
PAYG movement" — and `funding_buckets` carries
`CHECK ((entitlement_id IS NULL) <> (account_id IS NULL))`. So a payment only
ever funds an ACCOUNT bucket, and the ownership check is a same-column pairing
(`funding_buckets.account_id = payment_intents.account_id`) rather than the
two-hop entitlement → subscription resolution a reviewer expected. The two-hop
walk exists in the schema (`entitlements_subscription_id_idx`, migration
`000011`) and is not the guard here; the guard is simpler only because the
domain already refused the harder case.

**4. A refund is a debit B6 cannot represent, and the alternative is
minting an operator out of thin air.** A customer tops up 10,000, spends 9,000,
then is refunded. They are owed 10,000 and hold 1,000. `LedgerEntry.ApplyTo`
refuses any delta driving settled, held or available below zero, so there is no
adjustment that records the refund truthfully. Forcing one through would need a
machine principal to attribute the debit to, and `domain/accounting/ids.go`
already refuses to invent that grammar: `OperatorID` is "Text, not a
users-row reference: the operator surface is not built yet, and the ledger must
not guess at its grammar."

**5. `verify.sh` asserts a byte-exact table list, so a new table is a change to
the assertion and not an append.** The control lane is at `000011`; the
assertion is a `string_agg` equality over twenty-two tables and stays an
equality, because relaxing it to containment would silently unconstrain every
table already there.

**6. `internal/adapters/inbound/http` may not import `internal/domain`.** This
is `imports_test.go`'s rule, restated in `wire.go`'s own header: an inbound
surface that reached a domain package "would hand aggregates to the transport
instead of letting it render its own responses". A webhook handler that read a
`payments.Intent` would be the first violation of a rule that has held for
fourteen sections.

**7. The provider's protocol is a fact about the provider.** Its endpoint
shapes, its header names, its signature scheme, its amount encoding and its
event vocabulary are all things this platform does not control and cannot
decide. Every one of them that reaches the domain is a thing the domain has to
be re-compiled to correct.

## Decision

### 1. The provider is a port, and it is LOCAL INFRASTRUCTURE, not a second seam

`internal/ports/outbound/payments` is the fourth outbound port. The
distinction this forces is the one `TestTheControlPlaneHasExactlyOneCrossPlaneSeam`
exists to make, so the answer is asserted there rather than argued in review:
an external payment provider is reached over the public internet and **shares
nothing** with the runtime. It has its own database, its own credentials, its
own failure modes, and the inference path does not notice when it is down for a
week. What makes `dataplane` a SEAM is that its peer is the other half of this
product, owning rows this plane must agree with. A payment provider owns no row
of ours. The test's `want` list therefore gains a fourth name, deliberately and
in one place.

The port has two interfaces and they are separate on purpose. `Checkout` has one
method, and `WebhookVerifier` is its own interface because the two have exactly
one caller each and no common reason — a use case that opens checkouts should
not have to name a verification it never performs.

### 2. Only a verified server-to-server event has financial authority

**The browser is not a financial boundary and its redirect is not a
confirmation.** The customer returning from a hosted checkout reaches a page
that re-reads the payment's status from this platform's own rows and renders it.
It credits nothing, and no code path exists by which it could: the only caller
of `Accounting.TopUp` inside this feature is `ApplyProviderEvent`, which is
reached only from the webhook handler, which verifies a signature first.

The webhook is served at `POST /payment-webhooks/{provider}` — deliberately
outside `/api`, because it is not a console operation, carries no session and
must never be reached by a product client. It is the only route in the table
with `guardServerToServer`, and `routes_test.go` asserts that count is exactly
one.

**The signature is verified over the exact bytes that are interpreted.** This is
made unrepresentable rather than remembered: `WebhookVerifier.Verify` takes
`headers map[string][]string, rawBody []byte` and the port package does not
import `net/http` at all, so no implementation of it can be handed a parsed
request. A decoded struct has lost its whitespace, its key order, its duplicate
keys and its number spellings, and an HMAC over a re-serialisation authenticates
something the provider never sent.

Header multiplicity is preserved rather than collapsed, because an
implementation that accepted "the first of several" would make header smuggling
possible.

### 3. Provider-hosted checkout, and the PCI posture is structural

The checkout is the provider's own hosted surface. This process never sees a
card number, a CVV, or a cardholder field, and that is a property of the types
rather than a promise: `CheckoutRequest` and `CheckoutSession` have no field one
could arrive in, and a DTO without a card field cannot be made to accept one.

**This record does not claim PCI DSS compliance, and no part of this change
does.** Integrating a provider that is itself compliant is not the same thing as
this platform being compliant, and the difference is an assessment nobody has
run. What this platform claims is narrower and checkable: it has no card field —
no column, no member on any type, no place a card number could arrive in — and
it never takes custody of a payment instrument. The one place provider bytes are
retained verbatim is `control.payment_quarantine.payload`, which holds an
authenticated delivery's own body as the evidence an operator resolves a refusal
with; what that body may carry, and how it is classified, is stated in
[data implications](../architecture/data-implications.md), because "no card
field" and "no third-party personal data anywhere" are different claims and only
the first one is this design's to make. Everything else PCI would require —
network segmentation, logging and monitoring, access control review, an annual
assessment — is operational and out of scope here. Stating that is the point:
an unproven compliance claim is a liability, and the honest statement is the one
that names what was not done.

### 4. Credentials never leave the server

`SecretKey` and `WebhookSigningSecret` live in the configuration group, are read
by the adapter, and appear in no response, no log line and no frontend bundle.
`ProviderAccountKey` is the third member of the group and is NOT a credential:
it is the merchant account's public identifier, it is part of the deduplication
key, and it is logged deliberately so a delivery can be attributed to a
merchant. The two secret members are the ones the redaction covers, and they
are covered in every rendering — `%v`, `%s`, `%+v` and `%#v`, through the
`LogValue`/`String`/`GoString` trio the group's type implements, so a struct
that embeds the group cannot reach either. There is no publishable key in this
design, because a hosted checkout needs none: the browser is redirected to a
URL the server obtained, and it presents no provider credential of its own.

The adapter is the only package that holds them. Nothing logs a raw signature,
a full webhook payload, or a bearer token — the payload is stored only when an
authenticated delivery is quarantined, where it is evidence an operator is
meant to read, and never written by the default path.

### 5. An event for account A can never credit account B

The account is never taken from the payload. `resolveDelivery` resolves the
delivery against references this platform WROTE: the provider's **checkout
reference** first, which was stored on a row this plane created, and then the
payload's payment reference, matched byte for byte against the stored
`provider_payment_ref` column. The second arm is a fallback in lookup order and
not a relaxation of the rule — both values are compared against columns this
platform wrote, and a field the payload set is a thing searched FOR rather than
a thing believed. It is also load-bearing rather than a convenience: a refund
delivery names the PAYMENT the money was taken for and never the checkout it was
taken through, so an implementation with only the first arm would record every
refund against a payment this platform could not find, quarantining real money
that went back. A delivery that names neither a checkout this platform opened
nor a payment it recorded is quarantined, not guessed at. The intent row carries
the `account_id`; the funding bucket is that account's PAYG bucket, reached
through `OpenAccountFunding` and checked by `fundingTargetUsable` before
anything is written.

So the account is resolved by join from stored state, and a payload field that
claimed an account would have to overwrite a value already looked up. It has
nowhere to enter.

### 6. The amount is the platform's, and a disagreement fails closed

The intent's amount and currency are fixed by the server when the checkout is
opened — the browser names a top-up OFFER, the server prices it — and the
provider is asked to charge exactly that. On the way back, `MatchCapture`
checks, in this order:

1. the stored provider reference matches the one the delivery names;
2. the move is one the state machine has;
3. an amount is present, positive, and equal to the intent's;
4. **the currency matches, and it is checked BEFORE the amount.**

The currency comes first because an amount is not a figure until the unit it
counts in is known: comparing 10,000 against 10,000 and only then discovering
one is USD and the other is JPY is a comparison that approved the wrong number
before asking the question that would have refused it. `CurrencyCode` folds
case, so `usd` and `USD` are one currency, while the provider reference is
carried byte for byte, because two references differing in case are two
references to the provider.

A mismatch is a quarantine with a reason an operator can act on. It is never a
clamp and never a rounding: the ledger records what happened, and a correction
for a figure the provider did not report is not a record of anything.

**A price lives in configuration, and the client reads it.** The catalogue of
top-up offers — an id, an amount in minor units, a currency, an exponent, a
label — is a `config.Payments` field, and `GET /top-up-offers` publishes it.
Both halves are deliberate. In configuration, because a price is a deployment's
decision about what it sells in the currency it settles in, and a literal in Go
source would mean changing what a customer is charged requires a release. Read
by the client, because the alternative is a console that hardcodes prices, and
a console that hardcodes prices is a second pricing authority that disagrees
with the first one silently. The read is one-way: publishing an offer does not
make a price settable, and the request side still names an offer and never an
amount. A deployment with no offers publishes no top-up surface, which is a
coherent deployment; an offer set that is present and half-filled is a
configuration that failed to render, and is refused rather than completed with
defaults.

### 7. Dedup is durable, and the insert is the arbiter

`control.payment_events` carries a unique index on
`(provider, provider_account_key, provider_event_id)`, and the webhook's entire
work happens in ONE unit of work with that insert first:

1. the delivery is recorded;
2. the payment is resolved from the stored reference;
3. the domain checks the claim;
4. the status CAS runs;
5. the credit is written last, with **`txCtx`**.

The order is what makes it correct. Recording the delivery anywhere else
reopens a window: an insert that committed before the credit swallows the
credit's own redelivery, and a credit that committed before the insert leaves
this plane unable to say which delivery produced it. One transaction with the
insert first has neither, because a crash rolls back both.

Step 5's context is the single highest-risk line in the feature. `TopUp` opens
its own `WithinTx` internally, and `postgres.Store.WithinTx` **joins** a
transaction already in the context rather than nesting. Passing `ctx` instead of
`txCtx` would commit the money and the delivery record separately, silently, and
every test using an in-memory fake would still pass — which is why the parameter
is named `txCtx` at every level and why the atomicity claim is proven against
real PostgreSQL in an integration test rather than against a fake.

The dedup key is not the payment's identity. A provider routinely emits several
events for one payment, and the credit's key is derived from the PAYMENT
reference, so every delivery of the same capture derives the same command key
and B6's own convergence collapses them.

### 8. The response contract: 2xx means "this needs no further delivery"

| Case                                       | Status | Row written   |
| ------------------------------------------ | ------ | ------------- |
| Signature absent, repeated, or wrong       | 400    | **no**        |
| Signature valid, signed timestamp stale    | 400    | **no**        |
| Body larger than the endpoint's read limit | 400    | **no**        |
| `Content-Encoding` that is not identity    | 400    | **no**        |
| `Content-Type` that cannot be JSON         | 400    | **no**        |
| Delivery path names a provider not served  | 404    | **no**        |
| Authenticated, but no event id readable    | 200    | **no**        |
| Applied                                    | 200    | yes           |
| Duplicate delivery of a recorded event     | 200    | already there |
| Authenticated and recorded, not usable     | 200    | quarantine    |
| Nothing durably recorded (a real fault)    | 5xx    | no            |

**4xx is reserved for a delivery that is permanently unusable, and a wrong
signature is only one of the ways to be one.** The others are all equally
permanent: a correct signature whose signed timestamp sits outside the freshness
tolerance is a replay rather than a delivery, and a redelivery carries the same
signed timestamp, so a retry could only repeat the refusal; a body larger than
the endpoint's read limit is the same oversized bytes on every attempt; a
`Content-Encoding` that is not identity means the bytes signed and the bytes
received are not the same bytes; and a `Content-Type` that cannot be JSON cannot
carry a signed event this build reads. The 404 is a routing refusal and reads
nothing at all: the delivery path names a provider by name, and a name this
deployment does not answer for is a URL mistake rather than a failed
authentication — answering 400 there is what once made a mistyped endpoint look
exactly like a forgery. None of these belongs in a provider's retry budget, and
a retry is not optimism in any of them, it is a loop.

**An unverified body is never written anywhere.** A table that records one is a
table anyone on the internet can write to, in whatever volume they like,
forever. A refusal is logged instead — this endpoint writes one structured line
per refusal, and nothing else about it is durable; a log line is not a row.
This is what `ReasonUnverifiable`'s doc now says, and the rows that DO carry
that reason come from the one already-trusted caller: a reconciliation replaying
bodies this platform stored earlier, which can fail to verify for a reason that
is nobody's attack at all — a rotated signing secret.

**`ErrMalformedEvent` is narrow.** It is returned only when an authenticated
delivery could not be named at all, and the caller then writes nothing and
answers 200. A row keyed on an absent event id would swallow the NEXT unreadable
delivery as a duplicate, and reporting a real event as already-seen is worse
than reporting nothing. An unfamiliar kind or an unreadable amount is NOT this
case: those are read into a populated event and refused by the APPLICATION with
an operator-actionable reason, because "we understood your message and cannot
use it" and "we could not read your message" are different things to tell a
provider.

### 9. Refunds are recognised and recorded, not booked

There is no `payment_refunds` table and no ledger leg for a refund. The intent
carries `refunded_minor_units` and `uncovered_refund_minor_units`, and the
domain's `CanRefund` refuses any refund exceeding the captured amount.

The reason is §Context 4, and it is worth restating as a decision rather than a
limitation: a refund is a debit that leaves `settled`, and B6's algebra cannot
represent the movement for a customer who has already spent the money. Every
alternative was worse. Minting an operator principal to force the adjustment
through writes a permanent, un-retractable attribution onto an append-only
financial record on behalf of a party that does not exist.
`uncovered_refund_minor_units` is the honest record of what the provider
returned that this ledger has no movement for, and the operator's resolution is
`Accounting.Adjust`, which requires a reason and an identity and is a separate,
separately-confirmed act.

Both projections are written ABSOLUTELY, from the cumulative figure the
provider reported, and `uncovered_refund_minor_units` is measured against that
cumulative total rather than against one delivery's increment. The balance is a
property of the PAID PAYMENT and not of a single refund, so a figure taken per
delivery lets every delivery spend the same balance again: refunds of 40 and
then 70 out of a balance of 10 leave a shortfall of 60, and the per-delivery
reading records 50. The stored figure is what an operator resolves the case
with, so one that drifts from the case with each further refund is worse than
none.

`KindRefunded` is still interpreted rather than quarantined: "we did not
understand your message" and "we understood it and recorded it" are different
things to tell a provider, and the second is true.

### 10. Command keys are namespaced, keyed on the payment, and digested

`TopUpCommandKey` produces `pay:<provider>:topup:<digest>`. Three decisions:

- **The namespace is explicit.** B6's uniqueness on `(funding_bucket_id,
command_key)` is ONE SCOPE for every source of top-ups, and each source
  namespaces its own keys into it. A raw provider reference landing there
  unnamespaced is a coin flip against a key somebody else minted.
- **The payment, not the event.** This is the double credit of §Context 1
  closed mechanically. The digest is over the provider's PAYMENT reference, so
  every delivery of the same capture derives the same key.
- **Digested, not truncated.** B6 accepts 1..256 characters and a provider
  reference is unbounded, so something has to give. Truncation would be the
  obvious choice and it is the wrong one: two references sharing a prefix would
  produce one key and one credit where two were owed, silently and permanently.
  A digest is fixed length by ALGORITHM rather than by input, so the bound is
  unreachable and the full reference is stored on the row beside it for
  reconciliation.

### 11. Three tables, and the read side is not this change's

`payment_intents` (the aggregate), `payment_events` (verified deliveries and
their dedup key), `payment_quarantine` (deliveries that authenticated and could
not be used). Two tables for deliveries rather than one, because merging them
would require inventing an event id to store an unauthenticated body — the
forgery vector the signature check exists to prevent.

The control lane moves to `000013`, `verify.sh`'s exact table list goes from
twenty-two to twenty-five in the same change, and the reversal probe is updated
with it.

### 12. What this does not have

Named, so their absence is a decision rather than a gap someone has to infer:

| Not in this change                       | Why                                                                                                                                                                                                                                        |
| ---------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Card vaulting, a card field, a card form | Provider-hosted checkout. A field for a card number is the thing §3 exists to not have.                                                                                                                                                    |
| Refund issuance                          | Recording a refund the provider reports is not the authority to issue one. Issuing is a provider API call with its own authorisation, and it is a separate change.                                                                         |
| Payouts, settlement, provider fees       | The ledger records what a customer's balance is owed. What the provider keeps, and when it pays out, is the provider's own accounting.                                                                                                     |
| Reconciliation against the provider      | A reconciliation read needs a provider list API, a windowing rule and a findings vocabulary. It is a change of its own, and this one leaves it the rows to read.                                                                           |
| Multi-currency                           | The settlement currency is a platform-wide singleton (issue [#63](https://github.com/ecoma-io/llm-gateway/issues/63)), and the intent's currency must equal the provider's on every event. A second currency is that singleton's decision. |
| PCI DSS compliance                       | §3. Integrating a compliant provider is not being compliant.                                                                                                                                                                               |
| A hosted payment page of our own         | It would put this platform in the card-data path and rewrite §3.                                                                                                                                                                           |

### 13. Observability, and what may not be a label

The webhook path is the highest-cardinality thing this service has: every
delivery carries a provider event id, a payment id and an account. **None of the
three may be a metric label.** A label with unbounded cardinality is a memory
leak with a query language, and an account id as a label turns one counter into
one time series per customer. Counting, when it is added, is by outcome —
applied, duplicate, quarantined, refused — and by reason, both of which are
closed vocabularies. The identifiers go to logs and to the rows, where they are
looked up rather than aggregated.

**This change ships no counters, and the rule above is stated so that the change
which adds them inherits it rather than rediscovers it.** There is no metrics
library here to call: nothing in this service exports a series, so a sentence
that said a refusal was counted would be describing instrumentation that does
not exist. What an operator has today is the two things this change does
produce, and they divide the path cleanly: every REFUSAL leaves a structured log
line — those are the paths that write no row, which is why the line is their
only record — while every delivery that authenticated either leaves a row or is
deliberately given none, `payment_events` holding what was applied or absorbed
as a duplicate and `payment_quarantine` holding what was understood and could
not be used. A disposition is therefore read out of the tables and a refusal out
of the logs. Answering "how many deliveries failed to authenticate this week" today
means reading log lines rather than querying a counter, and saying so is
cheaper than a reader discovering it at the first dashboard nobody could build.
That the rule is unenforceable while no counter exists is exactly why it is
written down now: it is a constraint on a future change, and a constraint
recorded after the labels are chosen has already lost.

## Consequences

- **A double credit is now the class of defect this feature is built against**,
  and the two independent defences are named above: the command key derives from
  the payment, and the delivery insert is the arbiter. Both are tested, and the
  atomicity of the pair is proven on real PostgreSQL rather than on a fake,
  because a fake cannot model `WithinTx` joining.
- **The webhook is the first unauthenticated-by-session write surface in this
  service**, and it is authenticated by signature instead. It is the only route
  with `guardServerToServer`, and the count is asserted so a second one cannot
  appear unnoticed.
- **`application` gains a fifth `Code`.** `conflict` is now a real outcome — a
  top-up asked for on an account that cannot receive one — and `errorResponse`
  is now exhaustive over every code, with a default arm that logs. Before this
  change `unauthenticated` fell through to a 500, which is a defect this record
  fixes rather than inherits.
- **The console renders money in the currency the API declares**, per ADR 0012
  §4. The payments screen shows an amount, a status and an expiry, and it shows
  no card anything, because there is no card anything to show.
- **Expiry is not final.** `expired → succeeded` and `cancelled → succeeded` are
  legal edges: expiry and cancellation are LOCAL decisions, and the provider is
  the only party that gets to make a statement about the money. Any later report
  that counts expired payments as lost revenue is describing a set that may
  still arrive, and that is stated in the state table itself so it is not
  rediscovered as a bug.
- **A provider outage is not this plane's outage.** The inference path does not
  call the provider and does not notice it is down. What degrades is top-ups.

## Alternatives considered

- **Crediting from the browser redirect**: rejected absolutely. The redirect is
  a browser-controlled value; anyone who guesses a return URL would be funding
  themselves. It is a status refresh and nothing else.
- **Keying the credit on the provider's event id**: rejected. It is the
  §Context 1 double credit — several events per payment, each individually
  legitimate, and no existing check would catch the second leg.
- **Verifying a parsed-and-re-serialised body**: rejected. It authenticates
  something the provider never sent, and the failure is silent because the
  signature still matches the bytes the code chose to check. Made
  unrepresentable by taking `[]byte`.
- **Relying on the signature timestamp window as the replay defence**: rejected.
  It bounds staleness and nothing else; an attacker resends inside the window
  freely. The defence is the durable dedup key.
- **Recording every unverified delivery so an operator can see them**: rejected.
  It is an unauthenticated write path with unbounded volume, and it makes the
  quarantine table a place strangers can put things.
- **One table for verified and unverified deliveries**: rejected. It would need
  a synthesised event id for a body that has none, which is exactly the forgery
  the signature check exists to prevent.
- **`ON CONFLICT DO NOTHING` on the delivery insert**: rejected. It returns
  success for a redelivery, and the caller could not then tell whether it should
  credit.
- **Booking a refund as a ledger debit**: rejected, §9. It needs a machine
  principal the ledger has already refused to invent, or it drives a balance
  negative, which `ApplyTo` refuses.
- **A currency parameter on the top-up request**: rejected. The offer carries
  its own currency and exponent, and the server prices what it is asked for; a
  caller-set currency is a caller choosing what unit their money is in.
- **Renaming the port to avoid the `payments`/`payments` package collision**:
  rejected, and recorded because it is a real cost. `internal/domain/payments`
  and `internal/ports/outbound/payments` are both package `payments`, and the
  application layer aliases the port as `paymentprovider`. A rename would be
  tidier and would ripple through every importer; the alias is local, explicit,
  and states the relationship.
