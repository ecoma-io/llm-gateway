# Onboarding SePay: sandbox to first funded account

The operator's guide to taking this gateway's payment integration from nothing
to a working sandbox transfer, and then to live. It assumes no prior SePay
knowledge: every dashboard step, every `curl`, and every configuration value is
written out, and the whole thing can be followed top to bottom with the code in
front of you.

Two vocabularies run through it and it is worth separating them before you
start. **SePay's** words — order, virtual account, order code, webhook — are
facts about a provider this platform does not control. **This gateway's** words
— payment, funding bucket, the `CONSOLE_API_PAYMENTS_*` variables — are facts
about this repository, and they are taken from the code, not invented here.
Where a step needs something the provider's documentation does not state, the
step says so and tells you to confirm it in the sandbox rather than filling the
gap with a guess.

## 1. What this document gets you

By the end you will have a running sandbox in which a top-up started from the
console hands a customer a real SePay virtual account and QR code, a simulated
payment into that account produces a signed webhook, and the webhook — and only
the webhook — moves the customer's funding balance. You will have seen a
resent delivery credit nothing twice, and you will have a go-live checklist
that swaps the sandbox for production without changing anything but values.

### What has been run, and what you are running first

This guide was written from SePay's published documentation, and the gateway
side of it was exercised before it was written down: a real `console-api` was
started against a real database, signed deliveries were posted to its webhook
endpoint by hand, and the acknowledgements, the refusals, the single credit and
the quarantined mismatch described below were read back out of the rows rather
than taken from the status line. Section 10's recipe is the same one used to
produce those deliveries, and section 8's configuration is the configuration
they ran under.

What was **not** run is anything that needs a SePay account, because writing
this document did not involve one. That covers every step in the dashboard —
sections 3 through 6 — the simulated payment in section 9, and the go-live
exchange in section 11. They are written from SePay's documentation and are
reported there as current, and the places where that documentation is silent or
ambiguous are called out where they occur rather than smoothed over. So the
first thing you are doing is not only testing your gateway: you are also
confirming the provider's half of these instructions. Where a step disagrees
with what your dashboard shows you, the dashboard is right, and the step is a
defect worth reporting against this repository.

## 2. How the integration works, in one page

The mental model is small, and every failure in section 12 is a failure of one
of its four steps.

1. **The gateway asks SePay for a destination.** When a customer chooses a
   top-up offer, this process calls SePay's order API and receives a **virtual
   account** — an account number minted for that one payment — together with the
   bank and account holder it is held in, and a QR image SePay drew. This
   process stores what came back and shows it to the customer. It does not
   compose the QR or mint the account itself; a platform that did would own an
   encoding it does not control.
2. **The customer transfers money to that account.** The customer is handed a
   number to pay into, not a page to complete. They may leave the page, close
   the laptop, and come back tomorrow — the destination keeps working until it
   expires, and nothing they do on the page changes what happened.
3. **SePay posts a signed webhook.** When money arrives at the virtual account,
   SePay's own servers send a delivery to this gateway's endpoint, signed with a
   secret only the two of you share.
4. **The gateway verifies the signature, resolves which payment the delivery is
   about, and credits the account.** The delivery names the virtual account the
   money arrived at; that account was issued to one payment, so the delivery
   resolves to one payment and one account.

**Only step 4 moves money.** Nothing else in this system can credit an account:
not the QR being displayed, not the customer saying they have paid, not the
return to any URL. That is why a customer may safely leave the page — the
credit does not depend on them staying, and it does not depend on them telling
the truth. It depends on a signed message from SePay's servers arriving at an
endpoint the customer cannot reach. A second delivery of the same event, from a
retry or a replay, credits nothing again: the delivery is recorded under its own
identity before anything else happens, and a delivery already recorded is
absorbed as a duplicate.

Keep that model in mind and the configuration in section 8 reads as one
sentence: point the gateway at SePay, tell it which merchant account and which
bank account to use, and give it the signing secret it will check deliveries
against.

## 3. Register and enable Test mode

1. Create an account at `my.sepay.vn`. Nothing here needs a bank relationship to
   start.
2. Complete the **company profile** the dashboard asks for. It is the merchant
   identity SePay is onboarding, and the sandbox will let you create orders once
   it is filled in.
3. In the dashboard sidebar, enable **Test mode**. The toggle is in the sidebar,
   and the dashboard makes the current mode visible — confirm you are in Test
   mode before creating anything, because a token or webhook created in the
   wrong mode is invisible to the other one.
4. Understand what Test mode gives you before you rely on it: **sandbox data is
   fully isolated from production.** Orders, virtual accounts, terminals and
   webhooks created in Test mode exist only in Test mode, and they are invisible
   to — and cannot collide with — anything live. Test mode needs **no real bank
   account to be linked**, so you do not have to connect a bank to follow this
   guide.
5. Test mode supports creating orders, VA (virtual account) prefixes, terminals,
   and **simulating a payment** ("giả lập thanh toán"). The last one is how
   section 9 performs the end-to-end test without moving a real đồng.

The two modes have **separate API tokens**. A token created while Test mode is
on is not valid against production, and vice versa. Getting this wrong is one of
the more common ways to spend an afternoon on a 401.

## 4. Create the API token

1. Go to **Settings → API Keys** in the dashboard and create a key. Do this
   while **Test mode is on** for the value this guide uses; you will create a
   second one for live in section 11.
2. The token is a **64-character alphanumeric string**, and it is shown **in
   full only once**. Copy it the moment it appears; if you lose it you create a
   new one rather than recover the old one.
3. Treat it as what it is. **Every API key is full-access: there are no
   permission scopes to narrow it down.** It can open orders and read the
   merchant account's data, so it is a sensitive credential — never commit it,
   never log it, never paste it into a browser or into frontend code. This
   repository's configuration redacts it from every log rendering (see
   `config.Payments.LogValue`), and the guide keeps it in a shell variable
   rather than on a command line for the same reason.
4. Note the host you will talk to. The API base URL differs by mode and the
   gateway takes it as configuration:
   - **Production:** `https://userapi.sepay.vn/v2`
   - **Sandbox:** `https://userapi-sandbox.sepay.vn/v2`

   Both are called with `Authorization: Bearer {api_key}`.

One provider-imposed limit to keep in your head from here on: the API allows
**3 requests per second per IP**, and exceeding it returns **HTTP 429**. This
integration makes one order call per top-up, so a human-paced console will not
approach it; a script that backfills orders will.

## 5. Find your bank account id (`ba_xid`)

Order creation is scoped to one of the merchant's bank accounts, and the
identifier it needs is the `ba_xid` — the `id` field of an entry in SePay's
bank-accounts list. You read it out of the API rather than out of the dashboard.

Export the token as a shell variable first. A token typed directly on a command
line is a token in your shell history and in `ps` output; a variable is neither.

```sh
export SEPAY_API_TOKEN='paste-the-64-character-sandbox-token-here'
```

Then list the merchant's bank accounts:

```sh
curl -sS https://userapi-sandbox.sepay.vn/v2/bank-accounts \
  -H "Authorization: Bearer $SEPAY_API_TOKEN" | jq .
```

The response lists the merchant's bank accounts. **Read the `id` field of the
entry you intend to receive money into** — that value is the `ba_xid`, and it is
what section 8 configures and what order creation is scoped to. Confirm it is
the account you expect (the entry names the bank and holder) before you copy it.
If the list is empty, go back to section 3 and confirm Test mode is enabled and
the company profile is complete before assuming the account is missing.

Keep it in a variable too:

```sh
export BA_XID='paste-the-id-value-here'
```

The two most useful follow-up reads confirm you are pointed at the right place.
Both need the same bearer token.

```sh
# The orders this bank account holds. Sorting and paging only: created_at_sort,
# amount_sort, page (default 1) and per_page (max 100, default 20).
curl -sS "https://userapi-sandbox.sepay.vn/v2/bank-accounts/$BA_XID/orders?page=1&per_page=20" \
  -H "Authorization: Bearer $SEPAY_API_TOKEN" | jq .
```

Note what that endpoint **cannot** do, because it is the question an operator
always asks first: it **cannot filter by order code or by virtual account
number.** "Find the order for this customer" is not a query this list answers.
You read the page and look, or you look the order up by its own id. The response
reports `meta.pagination` with `total`, `per_page`, `current_page`, `last_page`
and `has_more` if you need to walk it.

```sh
# One order, by its own id. This is where you read paid_amount, status,
# refund_status, and the va[] array of virtual accounts.
curl -sS "https://userapi-sandbox.sepay.vn/v2/bank-accounts/$BA_XID/orders/$ORDER_XID" \
  -H "Authorization: Bearer $SEPAY_API_TOKEN" | jq .
```

The `va[]` array is the part that matters for debugging a transfer: each entry
carries `va_number`, `va_holder_name`, `amount`, `status`, `expired_at` and
`paid_at`, so it tells you whether the account the customer was given has been
paid and when.

## 6. Create the webhook

The webhook is the only thing that credits an account, so this is the step to
get exactly right.

1. In the dashboard, go to **Webhooks** and add one with **"Thêm"**. Set the URL
   to the gateway's delivery endpoint — the path is fixed by the code and ends
   in the configured provider name:

   ```text
   https://<your-public-host>/payment-webhooks/sepay
   ```

   The route is `POST /payment-webhooks/{provider}` (see
   `apps/console-api/internal/adapters/inbound/http/routes.go`), and
   `{provider}` is the value you will set in `CONSOLE_API_PAYMENTS_PROVIDER`.
   With the provider set to `sepay` — lowercase, as the configuration requires —
   the path is `/payment-webhooks/sepay`. A delivery addressed to any other
   provider name is answered 404, which is deliberate: it lets a typo in the URL
   be told apart from a signature failure.

2. Choose **HMAC-SHA256** authentication. SePay generates a **signing secret**;
   copy it immediately, for the same reason as the API token. This secret is
   what the gateway will use to check deliveries, and it is a different value
   from the API token.

3. The delivery carries two headers the gateway reads:
   - `X-SePay-Signature` — the value is
     `sha256={lowercase hex HMAC-SHA256("{timestamp}.{raw_body}")}`, keyed by the
     signing secret, where `{timestamp}` is the value of the timestamp header
     and `{raw_body}` is the exact request body bytes.
   - `X-SePay-Timestamp` — the moment SePay signed the delivery, in Unix seconds.

   The timestamp is part of what is signed, not an independent hint, and the
   gateway accepts deliveries only within a **±300 second** window. That is why
   it defaults its tolerance to five minutes and why section 8 tells you not to
   raise it: a deployment whose clock is skewed beyond five minutes refuses
   every honest delivery, and the failure looks exactly like a wrong secret.

4. Fire the built-in test delivery: the **⋮** menu beside the webhook →
   **"Gửi thử"**. What you are looking for in the delivery history is a response
   of **HTTP 200** whose body is exactly:

   ```text
   {"success": true}
   ```

   SePay treats a delivery as successful **only** on HTTP 200 or 201, within 30
   seconds, with that exact body. The block above is marked `text` rather than
   `json` on purpose: it is the byte string this deployment writes rather than a
   JSON document for a reader to reformat, and a formatter that pretty-printed it
   would show a body the process never sends. SePay's parser would not care, but a
   document that says "exactly" and then shows something else is how the next
   person learns to distrust it. The gateway answers it for every authenticated
   delivery — applied, already recorded, or quarantined — because "2xx" means
   "this event needs no further delivery from you" and nothing more. If the test
   delivery shows a 400, the delivery did not authenticate or was outside the
   tolerance; section 10 is how you tell those apart. If it shows nothing at
   all, the URL is not reachable — section 7.

5. Anything that is not a 200/201/`{"success": true}` is retried: SePay retries
   up to **7 times over roughly 33 minutes**. That window is why a quick fix and
   a resend is usually a better move than waiting, and why the gateway's
   deduplication has to be durable rather than best-effort.

Delivery history and manual resend live at `my.sepay.vn/webhooks`. You will use
the resend in section 9.

### Confirm in the sandbox

SePay's documentation does not answer two questions about the delivery payload,
and neither is safe to assume. Confirm both against a real test delivery:

- **`transactionDate`'s timezone.** The field's format is
  `YYYY-MM-DD HH:mm:ss`, and the zone is not stated. **The gateway does not read
  this field at all** — it is not parsed into anything, so there is no reading
  of it for the gateway to get wrong. The authoritative moment for freshness is
  the signed `X-SePay-Timestamp`, which is unambiguous because it is bound into
  the signature. Confirm the zone anyway, by comparing a delivery's
  `transactionDate` with the clock at which you fired it, so that nobody later
  builds logic on an assumption this integration deliberately avoids making.
- **Whether `accountNumber` is the payer's or the receiver's account.** The
  field is present and its owner is not documented. **The gateway does not read
  this field either.** The value a payment is resolved by is `subAccount` — the
  virtual account the payer sent to — and nothing anywhere consults
  `accountNumber`, so its owner cannot affect anything the gateway does. It is
  still worth confirming, by making two test transfers from different accounts
  and watching which value changes, because the first person to build on that
  field should be building on a fact.

Where you can actually LOOK at either field is the raw body of a **quarantined**
delivery, which the gateway stores verbatim as evidence. An applied delivery's
bytes are not stored — the ledger leg and the payment's state are its evidence —
so these two fields survive an applied delivery only in SePay's own delivery
history, not in this deployment's records.

## 7. Expose your local endpoint

SePay's servers call your webhook URL from the public internet. A process bound
to `localhost` is not reachable from there, so during development you need a
tunnel that gives your machine a public HTTPS URL.

```sh
# Forward the console-api's port. The API listens on :8080 by default
# (CONSOLE_API_ADDR), so the target is 8080.
ngrok http 8080
```

`ngrok` prints a public URL (something like `https://<id>.ngrok.app`). Your
webhook URL is that host plus the fixed path:

```text
https://<id>.ngrok.app/payment-webhooks/sepay
```

**Put that URL into the webhook configuration in the dashboard**, replacing
whatever was there. Two warnings that cost real debugging time:

- The URL in the dashboard is the one SePay calls. Changing the tunnel without
  changing the webhook means SePay delivers to the old host, gets a connection
  error, and retries on its own schedule while you watch an idle process.
- Free tunnels hand you a **new URL every restart**. Every restart is therefore
  a dashboard edit. If you are going to iterate, a reserved or named tunnel
  saves you the loop.

What works without a tunnel: the built-in test delivery and its **delivery
history**. You can fire "Gửi thử" and read, on SePay's side, the attempt and the
result it got — which is enough to confirm the webhook exists and to see the
status SePay recorded. What does not work without a tunnel is the delivery
actually reaching your process, and therefore the whole simulated-payment flow
in section 9: until the process is publicly reachable, no delivery lands, no
payment resolves, and no balance moves. If you have no tunnel and still need to
exercise the verifier, section 10 builds a signed request by hand against
`localhost`.

**Confirm in the sandbox** that Test mode really does deliver to a public URL.
The documentation states sandbox data is isolated and needs no linked bank, but
it does not say whether a sandbox webhook is dispatched to the internet the same
way a live one is. The way to confirm it: with the tunnel up and the webhook URL
set to it, fire "Gửi thử" and watch your process's log for the delivery. A
delivery that appears proves the sandbox reaches out; a delivery that stays in
the delivery history with a connection error, while the same URL answers a
`curl` from outside your network, means the sandbox does not. That answer
changes how you run section 9 — and if sandbox deliveries do not leave SePay,
use section 10's hand-built request as the proof that the verifier and the
credit path work, and reserve the end-to-end test for the first live transfer.

## 8. Configure the gateway

This section is the whole of the deployment's payment configuration. There is
**no environment template file in this repository**, so the block below is the
block; copy it and edit the values.

The group is **all-or-nothing**. A process with none of these variables set
starts with no payment surface at all — a coherent deployment, and the payment
routes simply are not there. But the moment **any one** of them is set, every
**required** variable below must be set, or the process refuses to start with an
error naming the missing one. A half-written payment configuration is a
deployment template that failed to render, and it is refused rather than
completed with defaults.

### Required

| Variable                                      | Sandbox value                         | Notes                                                                                                                                                                                                                                                                                                                           |
| --------------------------------------------- | ------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `CONSOLE_API_PAYMENTS_PROVIDER`               | `sepay`                               | Lowercase letters, digits, hyphens and underscores only. This value becomes the `{provider}` in the webhook path.                                                                                                                                                                                                               |
| `CONSOLE_API_PAYMENTS_API_TOKEN`              | _(your 64-character token)_           | SePay's API token. The value the adapter sends as `Authorization: Bearer`. It is a full-access credential with no permission scopes, which is why it is redacted from every log rendering; see the note below if you are upgrading a configuration from the previous provider.                                                  |
| `CONSOLE_API_PAYMENTS_WEBHOOK_SIGNING_SECRET` | _(the signing secret from section 6)_ | Must not be one of the documented example values (`changeme`, `secret`, `test`, `placeholder`); the process refuses those, because an endpoint that ships one has a signature anyone who read the docs can forge.                                                                                                               |
| `CONSOLE_API_PAYMENTS_PROVIDER_ACCOUNT_KEY`   | `sepay-sandbox-merchant-001`          | Your own stable identifier for this merchant account. It is part of the webhook deduplication key, **not** a credential, and it is logged. Keep it stable across restarts; it must be at most 128 characters.                                                                                                                   |
| `CONSOLE_API_PAYMENTS_API_BASE_URL`           | `https://userapi-sandbox.sepay.vn/v2` | **Must be HTTPS.** The process carries your bearer token to this URL, and the configuration refuses a cleartext base URL at startup. No query, no fragment, no userinfo.                                                                                                                                                        |
| `CONSOLE_API_PAYMENTS_BANK_ACCOUNT_XID`       | _(the `ba_xid` from section 5)_       | The merchant bank account order creation is scoped to. It is placed into the request **path**, not a query parameter, so the configuration refuses at startup any value outside `A–Z a–z 0–9 - _ .` — the adapter will not escape a path segment, and a value that would need escaping is refused here rather than sent broken. |
| `CONSOLE_API_PAYMENTS_ORDER_VA_HOLDER_NAME`   | `CONG TY TNHH VI DU`                  | The name the virtual accounts are issued in, sent with every order. It is the field a customer's banking app verifies the destination against — the one thing that tells them the money is going to this business — so an empty value is refused with the group rather than sent.                                               |

### Optional, with defaults

| Variable                                     | Default              | Notes                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| -------------------------------------------- | -------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `CONSOLE_API_PAYMENTS_WEBHOOK_TOLERANCE`     | `5m`                 | How stale a signed delivery may be. **`5m` is the maximum, not a suggestion**: SePay's own signing tolerance is ±300 seconds, so a wider window here would accept deliveries SePay itself would have rejected, and the process refuses a value above `300s` at startup with that reason. Zero and negative are refused too. There is nothing to tune here; if honest deliveries are being refused, the host's clock is what is wrong. |
| `CONSOLE_API_PAYMENTS_REQUEST_TIMEOUT`       | `10s`                | One outbound SePay call. It sits on a browser-facing request path with no deadline above it, so this bound is what the customer's patience is spent against. Must be positive.                                                                                                                                                                                                                                                        |
| `CONSOLE_API_PAYMENTS_ORDER_TID`             | _(unset — not sent)_ | A terminal id, for banks whose orders are keyed by terminal. Sent as `tid` **only when set**, because which banks need one is SePay's documentation's business and it changes; a deployment that always sent an empty one would be asserting something about the bank it does not know.                                                                                                                                               |
| `CONSOLE_API_PAYMENTS_ORDER_VA_PREFIX`       | _(unset — not sent)_ | A virtual-account prefix, for banks whose orders are keyed by prefix. Sent as `va_prefix` only when set, on the same reasoning as `ORDER_TID`.                                                                                                                                                                                                                                                                                        |
| `CONSOLE_API_PAYMENTS_ORDER_QRCODE_TEMPLATE` | `compact`            | Which of SePay's QR templates the order asks for, sent as `qrcode_template`. The value is a template name SePay defines; this build does not validate it against a list, because the list is the provider's and it can grow.                                                                                                                                                                                                          |

### The top-up offers

An empty offer list is legal and means this deployment publishes no top-up
surface. A present list must be complete: for every id in
`CONSOLE_API_PAYMENTS_TOP_UP_OFFER_IDS`, all four variables spelled from that id
must exist, or the process refuses to start.

The id is spelled into the variable names uppercased with hyphens written as
underscores, so the offer `starter` becomes `..._TOP_UP_OFFER_STARTER_*`.

A worked example: a single 50,000 VND top-up. VND has no minor unit, so its
exponent is `0` and the amount is the đồng figure itself.

```sh
CONSOLE_API_PAYMENTS_TOP_UP_OFFER_IDS=starter
CONSOLE_API_PAYMENTS_TOP_UP_OFFER_STARTER_AMOUNT_MINOR_UNITS=50000
CONSOLE_API_PAYMENTS_TOP_UP_OFFER_STARTER_CURRENCY=VND
CONSOLE_API_PAYMENTS_TOP_UP_OFFER_STARTER_MINOR_UNIT_EXPONENT=0
CONSOLE_API_PAYMENTS_TOP_UP_OFFER_STARTER_LABEL="50,000 VND"
```

`AMOUNT_MINOR_UNITS` is an integer in the currency's smallest unit — never a
decimal; the process refuses `"50000.00"` rather than parse it. `CURRENCY` is
three uppercase ISO 4217 letters. `MINOR_UNIT_EXPONENT` is `0`–`4`.

The amount is also bounded, at `1,000,000,000` minor units. That ceiling is
**this deployment's policy and not SePay's encoding** — the order API takes an
integer in a currency with no minor unit, and this build knows of no bound on
it. The figure exists so an extra digit typed into a top-up price is refused at
startup rather than sold to a customer, which is why it is high enough to be
irrelevant to a real price list: an offer of one billion đồng is a mistake
before it is a price. A deployment that genuinely sells larger top-ups raises it
deliberately, in the code.

### The whole block

```sh
# ---------------------------------------------------------------------------
# SePay sandbox — payment configuration for console-api
# ---------------------------------------------------------------------------
CONSOLE_API_PAYMENTS_PROVIDER=sepay
CONSOLE_API_PAYMENTS_API_TOKEN='<your 64-character sandbox token>'
CONSOLE_API_PAYMENTS_WEBHOOK_SIGNING_SECRET='<the sandbox webhook signing secret>'
CONSOLE_API_PAYMENTS_PROVIDER_ACCOUNT_KEY=sepay-sandbox-merchant-001
CONSOLE_API_PAYMENTS_API_BASE_URL=https://userapi-sandbox.sepay.vn/v2
CONSOLE_API_PAYMENTS_BANK_ACCOUNT_XID='<the ba_xid from section 5>'
CONSOLE_API_PAYMENTS_ORDER_VA_HOLDER_NAME='CONG TY TNHH VI DU'
CONSOLE_API_PAYMENTS_WEBHOOK_TOLERANCE=5m
CONSOLE_API_PAYMENTS_REQUEST_TIMEOUT=10s

# Optional: set these only if your bank's orders require them (section 8's
# "what the gateway does not let you configure"). Unset means not sent.
# CONSOLE_API_PAYMENTS_ORDER_TID='<terminal id, if the bank needs one>'
# CONSOLE_API_PAYMENTS_ORDER_VA_PREFIX='<VA prefix, if the bank needs one>'
# CONSOLE_API_PAYMENTS_ORDER_QRCODE_TEMPLATE=compact

CONSOLE_API_PAYMENTS_TOP_UP_OFFER_IDS=starter
CONSOLE_API_PAYMENTS_TOP_UP_OFFER_STARTER_AMOUNT_MINOR_UNITS=50000
CONSOLE_API_PAYMENTS_TOP_UP_OFFER_STARTER_CURRENCY=VND
CONSOLE_API_PAYMENTS_TOP_UP_OFFER_STARTER_MINOR_UNIT_EXPONENT=0
CONSOLE_API_PAYMENTS_TOP_UP_OFFER_STARTER_LABEL="50,000 VND"
```

> **Upgrading a configuration from the previous provider.** The API credential
> variable is `CONSOLE_API_PAYMENTS_API_TOKEN`, and a deployment that ran the
> previous provider spelled the same slot `CONSOLE_API_PAYMENTS_SECRET_KEY`. The
> name changed because the VALUE changed: the old one was a scoped key and this
> one is a full-access token, and a name that said "secret key" would describe
> the wrong credential. There is no fallback and no alias — the process reads
> one name, and a configuration carrying the old one is a configuration with no
> payment surface at all, because the group is all-or-nothing.
> `CONSOLE_API_PAYMENTS_CHECKOUT_RETURN_URL` likewise no longer exists: this
> provider performs no browser redirect, so there is nothing to return a
> customer from, and the variable was removed rather than left accepted and
> ignored. If you are unsure what your build reads, search the source for
> `CONSOLE_API_PAYMENTS_` — the names in
> `apps/console-api/internal/config/payments.go` are authoritative.

### What the gateway does _not_ let you configure

Four of SePay's order-creation fields have no environment variable, and none of
the four is missing by omission — each is a value this build derives, and a knob
for one would create a second authority for a single fact:

| Field         | Why there is no variable                                                                                                                                                                                                                                                                       |
| ------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `order_code`  | Derived from the payment's own identity, which is the point: a retry of the same payment derives the same code and therefore reaches the SAME order rather than opening a second one. A configurable value could not be stable across a retry.                                                 |
| `amount`      | The top-up offer's price, resolved from the offer the customer chose. A client never names an amount, and neither does the operator at request time.                                                                                                                                           |
| `duration`    | The platform's own patience — the same 30-minute window the console prints as the payment's expiry. It travels with the order from the application rather than from configuration, so that the deadline a customer is shown and the lifetime of the account they are given cannot drift apart. |
| `with_qrcode` | Always requested. The QR is one of the three things this integration exists to hand a customer, so it is not a deployment choice.                                                                                                                                                              |

Do not invent variables for these — a variable nothing reads is a variable that
does nothing, and this group is all-or-nothing, so an unknown name in it makes a
valid configuration look partial.

**`tid` and `va_prefix` are the opposite case**, and they are why this is worth
a section rather than a sentence. Which of the two an order requires **depends
on the bank** — SePay's own examples show a terminal id for one bank and a VA
prefix for another — the provider's documentation does not give a table, and
this build therefore sends each **only when you set it** rather than guessing.
Confirm what your bank wants in the sandbox before you rely on it:

1. Create one order against your `ba_xid` using SePay's own API or dashboard,
   with the fields you believe are correct for the account from section 5.
2. Observe whether the sandbox accepts it without a `tid`, without a
   `va_prefix`, or requires one of them.
3. Read the created order back from
   `GET /v2/bank-accounts/{ba_xid}/orders/{order_xid}` and check that the
   `va[]` entry carries the `va_number` you expected and an `expired_at` that
   matches the `duration` you sent in seconds.

If the gateway's order call needs one of these fields for your bank, that is a
code change with an accompanying configuration change, and it belongs in the
repository rather than in an undocumented environment variable. Report what the
sandbox demanded.

### Starting the service

The payment surface is wired in `apps/console-api/cmd/console-api/main.go`,
which builds the SePay client and the webhook verifier from this group. If the
group is incomplete, the process exits before it accepts traffic, naming the
variable it needs. Start the backend and the console:

```sh
# The API, on :8080 by default (CONSOLE_API_ADDR).
pnpm dev:console-api

# The console, on the Vite dev server (:5173 by default). Its dev proxy forwards
# /payment-intents and /top-up-offers to :8080, so the two work together with no
# CORS setup and no API-URL override.
pnpm dev:console
```

The local database and cache fixtures are in `deploy/` if you have not started
them:

```sh
docker compose -f deploy/postgres/compose.yaml up -d --wait
docker compose -f deploy/postgres/compose.yaml run --rm migrate up
docker compose -f deploy/redis/docker-compose.yml up -d
```

## 9. First end-to-end test

This is the test that proves the whole loop, and the second half of it — the
resend — is the property you most need to have watched with your own eyes.

1. **Start a top-up from the console.** Sign in, open the **Payments** screen
   (`/payments`), and choose the offer from section 8. The console asks the
   gateway for a payment, the gateway asks SePay for an order, and the screen
   shows you what came back: the virtual account number, the bank and account
   holder it is held in, and SePay's QR image. These are SePay's own values,
   reproduced verbatim; nothing on this side composes them.

2. **Reproduce the transfer with Test mode's simulated payment.** In the SePay
   dashboard, use **simulating a payment** ("giả lập thanh toán") for the
   virtual account you were just shown — the `va_number`/`transfer_code`. Do not
   change the amount: send exactly the figure the payment asked for.

   A simulated payment of a _different_ amount is worth doing once on purpose,
   later, because it is how you see the `amount_mismatch` quarantine in
   section 12 without losing real money.

3. **Watch the payment move to `succeeded`.** The console re-reads the payment;
   the status the page shows is the status SePay's delivery wrote, not anything
   the page decided. Refresh the Payments screen, or the payment's row will
   update as it is re-read.

4. **Watch the funding balance rise.** Open the **Accounting** screen and find
   the account's funding bucket. The credit lands there because a signed
   delivery said the money arrived — the only path by which a balance can move.

5. **Now resend the delivery — this is the important part.** Go to
   `my.sepay.vn/webhooks`, open the delivery history, find the delivery that
   just credited the account, and **resend it**. Then check two things: the
   payment did **not** gain a second credit, and the funding balance did **not**
   move again.

   The resent delivery carries the same event `id`, and the gateway resolves
   every delivery under (`provider`, merchant account, event `id`). An event id
   it has already recorded is absorbed as a duplicate: the durable record of the
   first delivery is what makes the second a no-op, not the clock and not the
   signature. This is the property that makes retries — and SePay's own 7-retry
   schedule — safe. If the balance moved twice, stop and fix it before going
   anywhere near production.

There is a final confirmation worth making while you are here: leave a payment
unpaid, and confirm that nothing about the page — refreshing it, revisiting it,
"marking" it as paid with the browser — moves the status or the balance. The
only writer is a verified delivery.

## 10. Verify a signature by hand

When a delivery is rejected, the two causes look identical from the dashboard:
either the signing secret you configured is wrong, or the request is genuinely
malformed. Reproducing the signature locally tells them apart.

Take the **exact** body bytes and the **exact** timestamp header value from a
real delivery (the delivery history shows both), export the signing secret, and
compute:

```sh
export WEBHOOK_SIGNING_SECRET='the sandbox webhook signing secret'
TIMESTAMP='1759000000'                       # the X-SePay-Timestamp value, verbatim
BODY='{"id":"evt_test_1","transferAmount":50000}'   # the exact body bytes

# The signed material is "{timestamp}.{raw_body}", and printf is used rather
# than echo so no trailing newline is added — the HMAC is over bytes, and a
# newline is a byte the signer did not include.
SIGNATURE="sha256=$(printf '%s.%s' "$TIMESTAMP" "$BODY" \
  | openssl dgst -sha256 -hmac "$WEBHOOK_SIGNING_SECRET" | awk '{print $NF}')"

printf '%s\n' "$SIGNATURE"
```

Compare that string, byte for byte, with `X-SePay-Signature` on the delivery.
Case matters: the digest is lowercase hex, and a value that differs only in case
is a different string. Then send it yourself to prove the endpoint accepts a
correctly signed request:

```sh
curl -sS -i -X POST "http://localhost:8080/payment-webhooks/sepay" \
  -H "Content-Type: application/json" \
  -H "X-SePay-Signature: $SIGNATURE" \
  -H "X-SePay-Timestamp: $TIMESTAMP" \
  --data-binary "$BODY"
```

What a correct request answers depends on what you sent, and the split is the
one thing to get right when reading the result, because it is not the split
people expect:

- **200 with `{"success": true}`** — the delivery authenticated and was acted
  on, one way or another. In the hand-built case above it almost certainly
  resolves to no payment at all, and **that is still a 200**: a delivery this
  build cannot resolve is quarantined with a reason and recorded as evidence,
  and the provider is told to stop. The `{"success": true}` means "this event
  needs no further delivery from you", not "this event was applied", which is
  why the same body answers an applied delivery, a duplicate and a quarantine
  alike. A quarantine is visible in **this deployment's own records**, not in
  SePay's delivery history — SePay will show you a 200 and nothing else.
- **400** — a refusal of the delivery _as a delivery_, and it writes no row:
  a signature that does not match, a timestamp outside the window (which is
  what you get if you reuse an old one), a body over the read bound, or a
  declared encoding or content type this endpoint will not interpret.

So a signature you computed correctly and sent immediately, naming an event
this deployment has never heard of, answers **200** — and if you were expecting
a 400 to tell you the payment was unknown, that is the sentence to remember.

Reading the result:

- **Your local computation matches the header, and the gateway still refuses an
  identical request** — the gateway is not using the secret you think. Check
  `CONSOLE_API_PAYMENTS_WEBHOOK_SIGNING_SECRET`, and make sure it is the
  **sandbox** webhook's secret and not the live one you will create in
  section 11.
- **Your local computation does _not_ match the header** — either the secret is
  wrong on your side, or you are not hashing the same bytes. A pretty-printed or
  re-serialised body is a different byte string from the one that arrived; use
  the raw body. If your clock is more than 300 seconds off, a _fresh_ valid
  signature will also be refused as stale — compute the signature over a
  timestamp that is now, and if that passes while the provider's real
  deliveries do not, the clock is the problem, not the secret.
- **`unverifiable`/400 with the signature present** — the signature did not
  match: almost always the wrong signing secret. **`stale`/400** — the signature
  was good but the signed timestamp is outside the ±300 second window: a replay,
  or a badly skewed clock.

Note the number of failures that do **not** land here by design: a `404` means
the path named a provider this deployment does not serve — a URL typo, not a
signature problem — and it is answered without reading the body at all.

## 11. Go live

Every value changes; no code does. The go-live checklist is:

1. **Swap the API token.** Create a live key under **Settings → API Keys** with
   **Test mode off**, copy it once, and set it as
   `CONSOLE_API_PAYMENTS_API_TOKEN`. A sandbox token will not authenticate
   against production and vice versa.
2. **Switch the API base URL** to `https://userapi.sepay.vn/v2`.
3. **Create the Live webhook**, with its **own** signing secret — a webhook
   created in Test mode does not exist in production — and set it as
   `CONSOLE_API_PAYMENTS_WEBHOOK_SIGNING_SECRET`. The old sandbox secret must
   not be reused, and vice versa.
4. **Confirm the webhook URL is publicly reachable over HTTPS.** No tunnel
   pointed at a laptop: a production deployment terminates TLS at a real host,
   and the delivery path is `/payment-webhooks/sepay` behind it. Check it is
   reachable from outside your network before you announce anything.
5. **Do one real, small transfer** through the whole loop — start a top-up, make
   a real transfer into the virtual account, confirm the payment reaches
   `succeeded` and the balance rises — before you tell a single customer this
   works.
6. Keep the two provider limits in mind. **3 requests per second per IP**; a
   burst past it earns HTTP 429, which this gateway treats as a retryable
   provider condition rather than a failure. And the delivery retry schedule —
   **7 attempts over roughly 33 minutes** — applies in production exactly as it
   does in the sandbox, which is why the delivery endpoint's own
   deduplication, not the retry schedule, is what keeps a credit single.

Because the mode switch changes only values, the safest way to run it is to keep
both blocks beside each other and switch at deploy time rather than editing one
in place. Re-run section 9's resend step against production once, on the first
real payment, and only then call it live.

## 12. Troubleshooting

Each row is a failure an operator can actually cause, and what it means.

One fact to read the whole table against: **SePay's delivery history shows the
HTTP status it received and nothing more.** A quarantined delivery and a
duplicate both appear there as a 200, because that is what they are — so every
row below that describes a quarantine is found in **this deployment's own
records**, not on SePay's side. The two rows that do show up as a 400 there are
the two that write no row at all.

| Symptom                                                                                     | What it means                                                                                                                                                                                                                                                                                       | What to do                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| ------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `unverifiable` (a rejected delivery; the transport logs it and answers 400, writing no row) | The delivery did not authenticate — usually the **wrong signing secret**, or a clock skewed beyond the tolerance.                                                                                                                                                                                   | Recheck `CONSOLE_API_PAYMENTS_WEBHOOK_SIGNING_SECRET` is the _sandbox_ (or _live_) secret for this webhook. Re-run section 10 to confirm which side is wrong.                                                                                                                                                                                                                                                                                                                                      |
| `stale` (a rejected delivery; 400, writing no row)                                          | The delivery authenticated but arrived **outside the tolerance window** — a replay, or a badly skewed clock.                                                                                                                                                                                        | Compare the `X-SePay-Timestamp` with your host's clock. If the host is skewed beyond ±300 seconds, fix the clock; the deliveries are otherwise fine.                                                                                                                                                                                                                                                                                                                                               |
| `unknown_payment` (a quarantine row)                                                        | The delivery named a **virtual account this deployment never issued** — a transfer to the wrong account, or a payment whose instructions were never recorded.                                                                                                                                       | Check the delivery's `subAccount` against the payments on the console. A transfer to an account no payment holds cannot credit anything, by design.                                                                                                                                                                                                                                                                                                                                                |
| `amount_mismatch` (a quarantine row)                                                        | The customer sent a **different amount** than the payment asked for. The gateway refuses rather than guessing.                                                                                                                                                                                      | This is a customer conversation: reconcile or refund out of band. Do not try to make the gateway accept it — a credited figure the provider did not report is not a record of anything.                                                                                                                                                                                                                                                                                                            |
| `currency_mismatch` (a quarantine row)                                                      | The delivery's currency is not the currency the payment was opened in. This provider reports **every** delivery as VND — a domestic transfer settles in đồng and the payload names no currency at all — so in practice this means the payment was opened against an offer priced in something else. | There is no FX rate in this system and no authority to choose one. Price the top-up in `VND`: a payment opened in another currency can never be satisfied by this provider, and no amount of redelivering will change that.                                                                                                                                                                                                                                                                        |
| A simulated payment moves **nothing at all** — no status change, no balance change          | The **webhook URL is wrong or unreachable**. The delivery never landed.                                                                                                                                                                                                                             | Open the delivery history at `my.sepay.vn/webhooks` **first** and read what SePay recorded: a connection error or a non-2xx names the cause. A `404` means the path named a provider this deployment does not serve — check the URL ends in your configured provider name. A `400` sends you to the rows above.                                                                                                                                                                                    |
| Order creation returns **`409`**                                                            | The provider **already holds an order for that payment**. The gateway derives the order's code from the payment's identity precisely so a retry is the same order rather than a second one; a `409` means an earlier attempt opened an order whose answer was lost before it could be recorded.     | Nothing recovers that order — the provider's list cannot be searched by order code — so the way forward is a **new payment** with a fresh idempotency key, which opens a fresh destination. The gateway leaves that payment `cancelled` rather than `created`, deliberately: a payment left in `created` reads as one a retry can finish, and this one cannot. No money was sent to the unreachable destination, because a destination is only ever shown to a customer once it has been recorded. |

Two things that are _not_ failures and should not be chased: a delivery answered
200 with `{"success": true}` for a payment that was **already** credited (that is
the deduplication working — see section 9), and a payment that sits in a waiting
state after a customer says they paid (the customer's word is not evidence; only
a verified delivery moves it).

### One thing to know before you read a quarantine row

A quarantine row carries the **whole authenticated delivery body, verbatim**,
and that body is a notification about one person's bank transfer. One field it
certainly carries is `accountNumber` — the account the money moved from or to,
whichever the provider means by it — and the provider's payload is a bank
transfer notification, so the body may well also carry the payer's name and the
description they typed. **Treat the payload as personal data and confirm its
actual field list against a real delivery** rather than trusting this
paragraph; what is certain is that a record of a customer's financial activity
is sitting in this deployment's database.

It is there on purpose. The row exists so a human can reconstruct what arrived
and resolve the case, and a version of it with the identifying fields stripped
would be a version nobody could resolve anything with.

Handle it accordingly. The two consequences worth stating plainly:

- **Access to these tables is access to customer bank details.** Nothing in
  this guide narrows who that is, because the deployment's database access is
  whatever it already is; the point is that the payment tables should not be
  treated as a diagnostic surface any more casually than the ledger is.
- **There is deliberately no raw body for an unverified delivery**, and that
  asymmetry is not an inconsistency: a body that failed its signature is an
  attacker's free text, and recording it would make this table a place anyone on
  the internet can write into. Rows whose reason is `unverifiable` or `stale`
  therefore carry no payload at all — if you are looking for the bytes of a
  rejected delivery, they are in the transport's log line and nowhere else, and
  they are not the customer's data either way.

Retention for these rows is a real question and it is **not** answered by this
integration: the table carries an engine guard against deletion, and a retention
policy needs a cutoff, a configuration value and an operator surface to set them
— none of which exists yet.
