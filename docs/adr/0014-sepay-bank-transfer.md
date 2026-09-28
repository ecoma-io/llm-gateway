# ADR 0014: Payment integration, revised — a bank transfer to a provider-minted virtual account, and the doctrine that did not move

- Status: Accepted
- Date: 2026-09-29
- Issue: [#138](https://github.com/ecoma-io/llm-gateway/issues/138)
- Supersedes: [ADR 0013](0013-payment-integration.md) — the provider and the instrument, not the doctrine
- Builds on: [ADR 0001](0001-bounded-contexts-and-aggregates.md), [ADR 0004](0004-reserve-and-settle-accounting.md), [ADR 0006](0006-control-plane-and-data-plane.md), [ADR 0012](0012-management-console.md)

## Context

ADR 0013 established the commercial loop's way in: a customer who wanted to
fund their own account was sent to the provider's hosted checkout, and the only
thing in this system with the authority to credit an account was a
signature-verified server-to-server delivery reporting the capture. That
record's design held. Its provider did not.

The customers this platform serves are in Vietnam and pay by domestic bank
transfer, so the provider is now SePay and the instrument is not a page a
customer completes but an account they send money to. The session, the browser
redirect and the return URL have no counterpart in this instrument, and a
platform that kept their names on its rows would be describing a thing that
does not exist. The provider is replaced, to get the instrument; with them goes
a small, deliberately bounded vocabulary.

**This record is written because a provider swap invites a much larger change
than it is, and refuses it.** The doctrine ADR 0013 argued for is not a
property of that provider — it is the reason the integration can be handed to a
different one without re-arguing the security of money. So this record states
three things and nothing else: what the instrument is and how a transfer is
correlated to a payment; that the doctrine is unchanged and is not
renegotiated; and the handful of facts the provider's own documentation leaves
unstated, which are recorded as unverified rather than filled in with
assumptions.

## Decision

### 1. The instrument is a transfer to a virtual account, and that account is the resolution key

The provider's instrument is a domestic bank transfer to an account it issues
per payment. This platform calls the provider's order API —
`POST /v2/bank-accounts/{ba_xid}/orders`, where `ba_xid` names the merchant's
bank account with the provider — and the answer carries, for that one payment,
a `va_number` (the virtual account the customer transfers to), a `qr_code_url`
(the provider's own QR image of the same destination), a `bank_name` and an
`account_holder_name`. The customer is shown all four and sends money.

The provider then delivers a webhook, and its `subAccount` field carries that
same `va_number`. The correlation between an incoming transfer and a payment is
therefore the destination account itself: the platform resolves the delivery
against the column it WROTE when it asked for the destination, and the value
the payload carries is a thing it searches **for** rather than a thing it
believes. A delivery that names an account this platform never issued resolves
to no payment and is quarantined rather than guessed at.

That shape — a key issued by the provider, unique to one payment, and not the
customer's to change — is the whole reason the instrument can be swapped
without moving the security boundary, and it is worth saying precisely: a
customer does not send money to a reference, they send money **to an address**,
and a message that names that address is a statement that money arrived where
this platform asked for it.

The destination is stored write-once on the payment, in the column ADR 0013
had already given a write-once arm and a partial unique index — the schema
renames that column rather than adding a second one beside it, because a
payment that carried both the old reference and the new one would be a row
where two columns each claim to be the key a third party's payload is resolved
against, and the one lookup this design cannot afford to make ambiguous is the
one a stranger's message drives. The QR URL, the bank name and the account
holder are stored beside it as the provider stated them, verbatim and matched
against nothing: they are what the customer checks before sending, an
affordance and never evidence, and an image authenticates nothing.

### 2. The rejected alternative: correlation by the transfer's own description

The provider offers a second way to tie an incoming transfer to a payment. It
can extract a `code` from the transfer's description text according to a
pattern configured in the merchant dashboard, and a delivery then carries that
code. The first draft of this change used it, and it is rejected.

**A memo is the customer's own free text, and a key must not be.** The
description on a bank transfer is written by the payer, and between them and
this platform sit their bank's own processing: it may uppercase the text,
shorten it to fit a field, strip characters it does not accept, or replace it
with a template of its own, and the customer may simply delete it or type a
word of their own. A payment resolved by a memo is a payment that stops
resolving the first time somebody's banking app reformats a description — and
it stops silently, because the delivery still arrives and still names an amount
and a code, and only the lookup fails.

The virtual account has none of that fragility and the two are not symmetric in
kind. It is issued by the provider, it is unique to one payment, and it is not
the customer's to edit because it is not a comment on the transfer, it is the
transfer's **destination**. A delivery naming it is a statement about where
money went; a delivery naming a memo is a statement about what somebody typed.

### 3. The rejected alternative: composing our own QR, and minting our own code

The second rejected alternative is to mint the platform's own transfer code and
compose a VietQR/EMVCo image locally, so that the customer scans a payload this
platform built rather than one the provider returned.

It is rejected because it buys a reimplementation of a provider-owned format
for no gain. A payment QR is a tag-length-value encoding carrying a CRC, and
the provider gives both away in the order response; a platform that composed
one would own that encoding, and every banking app that disagreed with its
reading of it would be a customer who could not pay. Worse, the encoding can
only really be validated by decoding a generated payload inside a real banking
app — a correctness check no unit test in this repository can perform, and one
that would be performed in production by the first customer whose app refused
the scan. Storing the provider's own answer keeps the encoding where the
account number comes from.

### 4. What the doctrine keeps, and does not renegotiate

ADR 0013's security doctrine is unchanged in every particular. It is restated
here so that a reader of the landed code can see it was kept rather than
assumed, and so that a future provider swap has the same list to check itself
against.

- **Only a signature-verified server-to-server delivery funds an account.** No
  browser, no redirect, no QR display, and no value a customer supplies has any
  financial authority. The customer's view of the payment is a read of the row
  a delivery wrote.
- **A delivery is a claim, and it is resolved against rows this platform
  wrote.** The account and the funding bucket come from the payment row, never
  from the payload, so a signed message cannot name whose money moves.
- **The signature covers the exact raw bytes that are interpreted.** The
  verifier is handed bytes, not a parsed request, because an HMAC over a
  re-serialisation authenticates something the provider never sent.
- **The dedup insert is first in the unit of work.** The delivery record
  arbitrates two concurrent copies of one message by a unique key, before
  anything moves.
- **The status compare-and-swap precedes the credit, and the credit is last on
  the transaction's context.** The payment matures and the ledger leg commits
  as one fact, joined by transaction rather than by convention.
- **Amount and currency fail closed.** A disagreement is a quarantine with a
  reason an operator can act on, never a clamp and never a rounding.
- **Refunds are recognised and recorded rather than booked as ledger debits.**
  B6's algebra still cannot represent a debit that drives a settled balance
  below zero, and no provider's vocabulary changes that.
- **Provider protocol vocabulary stays out of the domain and application
  packages.** The endpoint shape, the header names, the signature scheme, the
  amount encoding and the event vocabulary are facts about the provider, and
  they live in the adapter.

### 5. The mapping: what each doctrine slot now holds

| Doctrine slot                  | What it holds now                                                                                                                                                                                                                                                                             |
| ------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| The resolution key             | The provider-issued **virtual account**, stored on the payment when the order was opened and matched against the delivery's `subAccount`. It is a destination, not a reference the customer types.                                                                                            |
| The customer's affordance      | The provider's own **QR URL** (`qr_code_url`, stored and returned as `provider_qr_url`), shown with the bank name and the account holder so the destination can be checked before money is sent. It is displayed and never interpreted, and it authenticates nothing.                         |
| The economic payment reference | The delivery's `id` — the provider's identifier for the transfer that arrived — stored write-once on the payment and used as the input to the funding leg's command key, so every delivery of one transfer derives the same key and converges instead of funding twice.                       |
| The move                       | `transferType == "in"` maps to a capture. `transferType == "out"` must **never** map to a refund: a payout is money leaving this platform's provider account for somewhere else, not money returned to a customer, and reading it as a refund would invent a credit this platform never owed. |
| The currency                   | The constant `VND`. The payload names no currency, so the delivery contributes none: the intent's currency is priced from the offer, and the platform's settlement currency is that same constant, so a delivery can never state a unit the payment was not priced in.                        |
| The amount                     | The provider's amount, read through the existing exact-integer reader. An amount that arrives as a decimal is unreadable rather than silently rounded, and an unreadable amount is a quarantine with the delivery recorded as evidence.                                                       |

The provider reports no refund through this instrument, and none of the
machinery in §4 is relaxed for that. The refund states and their projections
stay in the domain and in the schema and are simply never exercised by a
provider that never reports one — a payment platform that deleted the path
because today's provider does not use it would have to re-add it, from
scratch, on the day one did.

### 6. The acknowledgement is exact static bytes, and the reason is the provider's

The provider treats a delivery as successful only on **HTTP 200 or 201, within
30 seconds, with a body that is exactly `{"success": true}`**. Anything else —
a 4xx, a 5xx, a timeout, or a 200 whose body spells the member differently —
is a delivery it retries, up to seven times over roughly thirty-three minutes.

So this one third-party-visible response is written as **exact static bytes**
rather than produced by the JSON serialiser. The serialiser this service uses
elsewhere emits `{"success":true}` — without the space — and how it spells the
body is its own business, which is exactly the problem: the same member in a
different shape is indistinguishable from "not an acknowledgement" to the
program reading it, and a body the provider does not recognise as an
acknowledgement is a body it retries. The bytes are a string constant so that
what is sent is what was reviewed.

**The rule "all 2xx carry one identical body" survives, with `success` as its
member.** Applied, already recorded, quarantined and unreadable-event
deliveries all answer with the same one-member body and the same status, because
2xx means "this event needs no further delivery from you" and says nothing
about the disposition. The disposition lives in the rows and the log, where
this platform reads it, never in the response, where the provider would be
invited to branch on it.

The 4xx and 5xx meanings are unchanged: a permanently unusable delivery is a
4xx and writes no row anywhere, and a 5xx is reserved for the one case where
nothing was durably recorded and a redelivery could therefore change the
outcome. The provider's retry schedule applies to every non-2xx, so a permanent
refusal is a delivery it will spend up to seven attempts on; that is its
schedule, not a change in what this platform writes, and it is stated here so
that the retries it produces are not later mistaken for a fault.

### 7. The webhook: two headers, one digest, and a tolerance the default already meets

The signature arrives in two headers, `X-SePay-Signature` and
`X-SePay-Timestamp`, and both are required and each may appear at most once — a
request carrying either twice is refused rather than resolved to "the first of
several", which is how a smuggled value wins. The signature's value is
`sha256=` followed by the lowercase hex of an HMAC-SHA-256 whose key is the
webhook signing secret and whose message is the signed timestamp, a literal
`.`, and the exact bytes of the request body:

```text
sha256={lowercase hex HMAC-SHA256("{timestamp}.{raw_body}")}
```

The timestamp is Unix seconds. The separator and the body are inside the
digest rather than concatenated loosely, which binds the two headers to one
another: a signature over a body alone could be replayed under any timestamp,
and one over a timestamp alone would cover no content.

The freshness tolerance is ±300 seconds. The existing five-minute default
already matches it, so no default value changes — but the CEILING does, and that
is a real change for a deployment that raised it: the configuration previously
accepted anything up to fifteen minutes, and now refuses more than three
hundred seconds at startup. A window this process honours and the provider would
not is a number that reads like a defence and is not one, and the honest fix for
a skewed clock is the clock. The tolerance bounds staleness and nothing else —
the replay defence remains the durable dedup key, for the reason ADR 0013 gave.
The signature covers the exact bytes the adapter interprets, and the read is of
those bytes as delivered.

### 8. What the provider's documentation does not state

This section is first-class rather than a footnote. Each item below is a fact
the provider's documentation does not settle, and each is recorded with why the
uncertainty is safe: what is not known cannot decide money here.

**Whether the sandbox actually delivers webhooks to a publicly reachable URL.**
The documentation describes simulating a payment; that a simulation results in
a real delivery to an external address is not explicit. Local testing therefore
assumes a tunnel to a publicly reachable endpoint, plus the dashboard's own
"send test" button, and treats the delivery path as something confirmed against
the sandbox rather than assumed from the documentation. Nothing in production
depends on this: a real transfer produces a real delivery, and the handler is
written to the scheme above rather than to a simulation.

**Whether the payload's `accountNumber` names the payer's account or the
receiving one.** This build **does not read the field at all** — it is not
parsed into the event, not compared to anything, and not a value any decision
consults; the field correlation uses is `subAccount`. So a wrong reading of
`accountNumber` is not merely harmless, it is impossible on this side, because
there is no reading of it to be wrong. What an operator gets instead is the raw
authenticated body on a delivery this plane could not apply, where the field
appears unparsed among the provider's own words. The question is recorded here
because it is one a reader will ask, and because the first change that starts
reading the field is the change that has to answer it.

**The timezone of `transactionDate`.** The value carries no offset and the
documentation does not say what zone it is in. Like `accountNumber` it is **not
read**: the port's `OccurredAt` is the zero time on every delivery from this
provider, and the field appears only inside a quarantined delivery's raw bytes
and never as a parsed instant. Nothing orders, windows, deduplicates or expires
by it, and the authoritative moment for freshness is the signed
`X-SePay-Timestamp`, which is unambiguous because it is bound into the
signature.

Both entries share a shape worth naming, since it is what makes them safe: the
uncertainty is about fields this build declines to interpret, and the honest
place for a field whose meaning is unverified is unread rather than
best-guessed. The debt is that a later change could start reading either one
without revisiting this section.

**Whether order creation requires `tid` or `va_prefix`, depending on the
bank.** The provider's own examples show one or the other, and which is needed
varies by bank. Both are therefore sent from configuration when they are set,
and the exact set a given bank requires is confirmed in the sandbox; a
configuration that omits what a bank needs fails at order creation, where it is
visible, rather than silently producing a destination that cannot be paid.

**There is no documented source-IP allowlist for deliveries.** No range is
published to filter on, so the HMAC over the raw body remains the sole
authentication and the only thing standing between an unknown caller and an
accounting decision. This changes nothing about the design — the signature was
always the authentication — but it is stated because it removes an option a
reader might otherwise assume exists, and an assumption of network-level
protection is worse than its absence.

### 9. What this change defers: reconciliation and backfill

Filling the gap where a delivery never arrived is deliberately out of scope
here and is a separate issue. It is polling the provider's order or transaction
APIs to find transfers that landed but were never delivered — with a window and
cursor state that survives a restart, a backoff that respects the provider's
rate limit of 3 requests per second per IP, and a mapping between the integer
`id` a webhook carries and the UUIDs those APIs use. It needs a findings
vocabulary of its own and it is a change of its own.

**A console status re-read is explicitly not reconciliation.** Asking the
provider what became of one payment, on demand, finds nothing that was never
looked for; reconciliation is the unattended sweep that looks for what did not
arrive, and naming the difference here is what keeps a small read from being
mistaken for the loop this platform does not yet have.

## Consequences

- **The instrument changed and the security boundary did not.** A customer
  sends money to a provider-issued account, and a signature-verified delivery
  reporting that money is still the only input that can credit an account. The
  resolution key moved from a session reference to a destination account, and
  that is the whole of the correlation change.
- **A memo-correlated payment would have been a latent, silent failure**, and
  the virtual account is not merely a nicer key — it is the only one of the two
  the customer cannot rewrite. This is the decision in this record most likely
  to be revisited by somebody who wants a code a customer can type; the answer
  is §2.
- **Three names changed and no table was added.** The control lane's migration
  renames the old reference and affordance columns and the `checkout_open`
  status, and adds the bank name and the account holder. It creates no table and
  drops none, so every guard, index and projection ADR 0013 left in place is
  exactly as it left it.
- **One response body is now exact bytes rather than a serialisation.** This is
  the only place a third party's parsing of this platform's output decides
  whether a delivery is retried, and it is written as a constant so that the
  bytes sent are the bytes reviewed.
- **The unverified list is a debt list, and it is short.** Each entry names why
  it is safe, and two of them (`accountNumber`, `transactionDate`) are safe
  because they are not read at all: neither is parsed, and only a quarantined
  delivery's raw bytes carry them onward, unparsed. That is a property of the
  code rather than of the documentation, and therefore something a later change
  can break by starting to read either one.
- **Reconciliation is still absent, and it is now the only thing missing from
  the payment loop.** A transfer that arrived while the webhook was
  undeliverable is invisible until an operator looks, which is why §9 names it
  as the next change rather than leaving it implied.

## Alternatives considered

- **Correlating on the transfer memo.** Rejected, §2: the memo is the
  customer's free text and their bank's to reformat, and a payment resolved by
  one stops resolving silently the first time it is rewritten.
- **Minting our own code and composing a VietQR/EMVCo image locally.**
  Rejected, §3: it reimplements a provider-owned tag-length-value encoding with
  a CRC, and can only be validated by decoding a generated payload in a real
  banking app.
- **Mapping `transferType == "out"` to a refund.** Rejected: an outgoing
  transfer is a payout, which is money leaving the provider account for
  somewhere else, and recording it as a refund would book a return to a
  customer this platform never made.
- **Reading the currency out of the payload.** Impossible and not merely
  rejected: the payload names none, so the settlement currency is the platform
  constant and a delivery cannot disagree with the offer that priced it.
- **Rounding an amount that arrives as a decimal.** Rejected: the exact-integer
  reader refuses it and the delivery is quarantined, because the ledger records
  what happened and a figure the provider did not report is a record of
  nothing.
- **Returning the acknowledgement through the JSON serialiser.** Rejected, §6:
  the serialiser emits `{"success":true}` without the space, and a body the
  provider does not recognise as an acknowledgement is one it retries.
- **Treating a console status re-read as reconciliation.** Rejected, §9: a read
  of one chosen payment finds nothing that was never asked about.
