# Documentation

The repository's longer-form documentation. The repository map is
[README.md](../README.md), the engineering process is
[CONTRIBUTING.md](../CONTRIBUTING.md), and the rules contributors and agents
are held to are in [AGENTS.md](../AGENTS.md).

## Architecture decision records

Accepted decisions live in [`adr/`](adr/), one numbered file each. Future
architecture decisions land the same way: an ADR is written and accepted
first, and the implementation follows the decision — an architecture decision
that is not recorded as an ADR has not been made.

| ADR                                                           | Decision                                                                                                                   |
| ------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------- |
| [0001](adr/0001-bounded-contexts-and-aggregates.md)           | Bounded contexts and aggregate boundaries                                                                                  |
| [0002](adr/0002-routing-and-fallback-ownership.md)            | Routing model — who owns fallback, translation, and egress                                                                 |
| [0003](adr/0003-concurrent-subscriptions-and-entitlements.md) | Commerce — concurrent subscriptions, scoped entitlements, and PAYG                                                         |
| [0004](adr/0004-reserve-and-settle-accounting.md)             | Accounting — reserve, execute, settle                                                                                      |
| [0005](adr/0005-relational-and-event-storage-split.md)        | Storage — PostgreSQL relational state and Timescale-oriented event history                                                 |
| [0006](adr/0006-control-plane-and-data-plane.md)              | Control Plane and Data Plane — the four applications, their boundaries and their data                                      |
| [0007](adr/0007-control-to-data-projection.md)                | The Control → Data credential projection — durable log, reconciliation loop, and mirror                                    |
| [0008](adr/0008-console-sign-in-identity.md)                  | Console sign-in — account-scoped resolution and the live-filtered email lookup rule                                        |
| [0009](adr/0009-provider-adapters-and-egress.md)              | Provider adapters and egress — the executor boundary, the settled claim, and the renewed lease                             |
| [0010](adr/0010-usage-fact-ingestion-and-settlement.md)       | Usage fact ingestion and settlement — the pull consumer, the kind-classed ledger, and the page that commits whole          |
| [0011](adr/0011-reaper-and-reconciliation.md)                 | The reaper and the reconciliation pass — a second door into an ending, and a second look at what the Control Plane derived |
| [0012](adr/0012-management-console.md)                        | The management console — its surface, its read model, and what it does not have                                            |
| [0013](adr/0013-payment-integration.md)                       | Payment integration — what has financial authority, and what merely has a browser                                          |
| [0014](adr/0014-sepay-bank-transfer.md)                       | Payment integration, revised — the bank-transfer instrument and the destination that is the key                            |

## Architecture pages

Reference pages for the designed domain model the ADRs produce — the picture
an engineer needs before writing migrations or a contract under `api/openapi/`.
They describe the design; where a slice of it has shipped, the page says so and
grounds the claim in the schema or package that carries it — the runtime
storage foundation (`migrations/dataplane/000003_runtime_storage` and the
`apps/dataplane` domain, ports and adapters behind it) is the first such slice,
and the serving domains are still design.

- [Overview](architecture/overview.md) — the five bounded contexts; what
  exists, what owns what, and what words mean
- [Planes and ownership](architecture/planes.md) — the four applications, and
  the record-by-record ownership matrix the two planes are built from
- [Cross-plane protocols](architecture/cross-plane-protocols.md) — what crosses
  the plane boundary in each direction, and how a fact reaches its consumer
- [Ports and adapters](architecture/ports.md) — the layering every Go
  application keeps, and the two chains that run through it end to end: the
  fact feed and the payment webhook
- [Routing](architecture/routing.md) — the routing pipeline, its policy inputs
  and egress ownership
- [Request lifecycle](architecture/request-lifecycle.md) — what happens to one
  request, step by step, and what is deliberately not on that path
- [Commerce](architecture/commerce.md) — plans, subscriptions, entitlements,
  and PAYG
- [Accounting](architecture/accounting.md) — reservations, settlements, usage
  events, funding buckets, and the ledger
- [Data implications](architecture/data-implications.md) — how the model maps
  onto the relational / time-series storage split
- [Persistence](architecture/persistence.md) — the two databases, the two
  migration lanes, and the conventions future schema work keeps

These pages link; they do not restate. Where a page and an ADR disagree, the
ADR wins and the page is wrong.

## Operations

Runbooks for what an operator does to a deployment rather than to its code —
written to be followed in one direction, from an empty account to a verified
end-to-end payment, with each refusal the step can produce explained beside the
step that produces it. They describe the deployment as it ships; where a
runbook and an ADR disagree about what the system does, the ADR is right and
the runbook is a defect.

- [SePay onboarding and sandbox testing](operations/sepay-onboarding.md) —
  registering the merchant account, minting the API token, wiring the webhook,
  mapping the `CONSOLE_API_PAYMENTS_*` variables, reproducing a real transfer in
  SePay's own sandbox, and reading a rejected delivery or a quarantine row when
  one appears.
