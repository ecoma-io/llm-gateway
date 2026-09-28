# Ports and adapters

Reference page for the layering every Go application in this repository keeps —
inbound and outbound ports, inbound and outbound adapters, and `cmd` as the only
composition root — and for the one chain that exercises all of them today. The
rule is [ADR 0006](../adr/0006-control-plane-and-data-plane.md) §6 and the
architecture tests that enforce it; that record wins over this page wherever the
two disagree. What actually crosses the plane boundary is in
[cross-plane protocols](cross-plane-protocols.md).

## The shape

```text
cmd/<app>/                        the composition root: the only place a concrete
   │                              adapter is constructed and handed to the application
   ├── adapters/inbound/<transport>      ← a request arrives (HTTP, on a listener)
   │        │
   │        ▼
   │   application/ ──────────────► domain/    (where an application declares one)
   │        │
   │        ▼
   └── ports/outbound/<name>             the interface the application calls
            ▲
            │ implements
       adapters/outbound/<impl>         ← infrastructure: a database, a cache,
                                            another application's management surface
```

The dependency arrow is the whole rule: adapters depend on ports, the
application depends on ports, and nothing depends on an adapter except the
composition root. Four consequences are worth stating, because each one is a
decision a later change can undo by accident.

- **`cmd/<app>/` is the only composition root.** It reads configuration once,
  constructs the concrete clients, repositories and adapters, wires them behind
  the ports the application expects, and owns the process lifecycle and
  shutdown. No business logic lives in `main.go`, and no package other than
  `cmd` reads the environment — a package that reads configuration has
  behaviour its callers cannot see in its arguments and cannot vary in a test.
- **An outbound port is an interface declared where it is consumed**, in
  `internal/ports/outbound/`, and named for what it does rather than for what it
  is (`persistence`, not `postgres`; `usagefacts`, not `usage_events_table`).
  The adapter that implements it lives in `internal/adapters/outbound/`. A
  change that reaches for an infrastructure client from application code widens
  the boundary, and it has to widen it in the diff or not at all.
- **An inbound port is the application itself.** Go interfaces are satisfied
  implicitly, the inbound adapter is the application's only caller for a given
  use case, and a declared interface with one implementation and no substitute
  is a shape invented twice. `internal/ports/inbound` is therefore sanctioned by
  the console-api's and the runtime's architecture tests and is empty on
  purpose: it exists so that a second inbound adapter — a CLI, a queue consumer —
  arrives as a deliberate decision rather than as a new directory.
- **An adapter never imports another adapter.** The composition root decides
  which one answers, and an inbound adapter that imports an outbound one has put
  concrete infrastructure behind a route with no port to substitute; an outbound
  adapter that imports an inbound one has made a transport part of a use case.
  The runtime's rule table states both arrows, and neither exists.

Where an application has nothing to own, the rule table says so with an empty
allow-list rather than with a convention. `dataplane-api` is the case: a
persistence or cache client there would be the management transport acquiring
state of its own, and the only thing that may appear under its outbound trees is
the seam through which it reaches the Data Plane ([planes and
ownership](planes.md); ADR 0006 §9, §11).

## The fact-feed chain, end to end

The usage-fact feed is the one flow that runs through every layer on this page,
in two applications and one runtime, so it is the worked example. Read it
downward: each line is a package, and each indent is a call.

```text
console-api application                     FactIngestion.Replay — read the stored
   │                                        position, fetch one page, apply it and
   │                                        advance the cursor in one transaction
   ▼
console-api ports/outbound/dataplane        UsageFacts — the port the use case is
   │                                        written against
   ▼
console-api adapters/outbound/dataplane     Client — HTTP GET /internal/usage-events,
   │                                        presenting the service credential
   ▼
dataplane-api adapters/inbound/http         the management surface's fingerprint:
   │                                        verify the caller, then run the use case
   ▼
dataplane-api application                   UsageEvents — a pass-through; the cursor,
   │                                        the order and the page are not touched
   ▼
dataplane-api ports/outbound/dataplane      UsageFacts — this application's own copy
   │                                        of the seam, not an import of the runtime
   ▼
dataplane-api adapters/outbound/dataplane   Client — sends after and limit and
   │                                        returns what the Data Plane said
   ▼
dataplane adapters/inbound/management       the Data Plane's private listener:
   │                                        the service-credential check runs before
   │                                        any use-case code, and the contract's page
   │                                        size is defaulted and bounded here
   ▼
dataplane application                       ReadUsageEvents — returns the page and the
   │                                        page size and does nothing else to either
   ▼
dataplane ports/outbound/usagefacts         Reader — the port to the runtime's own
   │                                        recorded facts
   ▼
dataplane adapters/outbound/postgres        UsageFacts — the production reader, and
                                            the durable source: pages read from
                                            usage_events in append order, never an
                                            in-memory feed that would lose every
                                            fact on restart
```

Three things this chain demonstrates, and each is the reason a layer is shaped
the way it is:

- **The plane boundary is a port, not an import.** `dataplane-api` and
  `console-api` each declare their own copy of the seam, because the three Go
  applications are separate modules (ADR 0006 §1) and no import path connects
  them. What crosses is a call over an interface, and the day the Data Plane
  changes its own port, nothing on the Control Plane's side moves until someone
  decides it should.
- **The cursor travels verbatim.** Every package on the chain passes it through
  without looking inside — including the two that could parse it and must not.
  `console-api` stores it, `dataplane-api` carries it across without reading it,
  and the Data Plane both issues it and reads it.
- **No layer holds state it was not given.** `dataplane-api` has no cursor, no
  cache and no last page; `console-api`'s adapter has no position of its own;
  the runtime's application adds no rule to the feed. A cached page or a
  locally-computed position anywhere on this chain would be a second, silent
  owner of the protocol.

The same shape holds for the management direction, with one difference: the
Control Plane's call asks the Data Plane to change something the Data Plane
owns, so it is a command the Data Plane can refuse rather than a read it
answers from what it already recorded ([cross-plane
protocols](cross-plane-protocols.md)).

## The payment chain, end to end

The payment integration (ADR [0013](../adr/0013-payment-integration.md)) is the
second flow that crosses every layer on this page, and it is worth reading
downward for a different reason than the fact feed: the fact feed shows how a
plane boundary is kept, and this shows how a layer boundary is kept when the
other end of the wire is a third party that does not share this repository's
vocabulary, its database, or its trust.

It has two directions, and which one carries financial authority is the whole
design.

**Outbound — a customer decides to fund their account.**

```text
console (browser)                          POST /payment-intents with an OFFER;
   │                                       never an amount — the server prices it
   ▼
console-api adapters/inbound/http          the handler — the account comes from the
   │                                       SESSION, never from the body or the path
   ▼
console-api application                    Payments.BeginCheckout — writes the intent
   │                                       FIRST, then calls the provider OUTSIDE the
   │                                       transaction, then records the checkout
   ▼
console-api ports/outbound/payments        Checkout.OpenCheckout — the interface the
   │                                       use case is written against
   ▼
console-api adapters/outbound/paymentprovider   the provider's endpoint shape, its
                                           headers, its idempotency key, its amount
                                           encoding — all facts about THEM, none of
                                           which appears above this line
```

**Inbound — the provider says what happened. This direction, and only this
direction, may credit an account.**

```text
payment provider's servers                POST /payment-webhooks/{provider}
   │
   ▼
console-api adapters/inbound/http          the handler: reads the body as BYTES, never
   │                                       parses it, and verifies the signature over
   │                                       those exact bytes. A bad signature is a
   │                                       400 and nothing is written anywhere
   ▼
console-api ports/outbound/payments        WebhookVerifier — Verify(headers, rawBody).
   │                                       This package does not import net/http, so no
   │                                       implementation can be handed a parsed body
   ▼
console-api adapters/outbound/paymentprovider   the provider's header name, its HMAC
   │                                       scheme and its event vocabulary live here
   │                                       and are normalized into the port's two kinds
   ▼
console-api application                    Payments.ApplyProviderEvent — ONE unit of work:
   │                                       record the delivery, resolve the payment from
   │                                       the STORED reference, check the claim against
   │                                       the domain, CAS the status, credit last
   ▼
console-api ports/outbound/persistence     PaymentEvents / PaymentIntents / PaymentQuarantine
   │                                       — every write refuses to run outside a unit
   │                                       of work
   ▼
console-api adapters/outbound/postgres     the three tables in `control`, and the unique
                                           key that makes a redelivery a duplicate
                                           rather than a second credit
```

Four things this chain demonstrates, and each is why a layer is shaped the way
it is:

- **The provider is local infrastructure, not a second seam.**
  `ports/outbound/payments` is reached over the public internet, and its peer
  owns no row of ours — which is exactly what distinguishes it from `dataplane`
  and is asserted in `internal/arch/packages_test.go` rather than argued. The
  first direction may fail for a week without the inference path noticing.
- **A verification is not a decision.** The adapter answers one question — "is
  this ours, and what does it claim?" — and knows nothing about intents,
  accounts or buckets. If the adapter could resolve an event to an account, it
  would be an adapter with the authority to move a customer's money. The
  resolution is the application's, against a row this plane wrote.
- **The bytes that are verified are the bytes that are interpreted.** This is a
  property of the port's signature rather than a rule in a comment: `Verify`
  takes `[]byte`, and `net/http` is not imported here at all. A decoded body has
  lost its whitespace, key order and number spellings, and an HMAC over a
  re-serialisation authenticates a message that was never sent.
- **The two directions are joined by a row, not by a return value.** The
  customer's browser returning from the hosted checkout reaches the first
  direction's list and re-reads a status. It cannot set one. The only writer of
  a status from `checkout_open` to `succeeded` is the second direction, which is
  the whole of why a redirect is not financial authority.
