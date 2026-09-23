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

| ADR                                                           | Decision                                                                   |
| ------------------------------------------------------------- | -------------------------------------------------------------------------- |
| [0001](adr/0001-bounded-contexts-and-aggregates.md)           | Bounded contexts and aggregate boundaries                                  |
| [0002](adr/0002-routing-and-fallback-ownership.md)            | Routing model — who owns fallback, translation, and egress                 |
| [0003](adr/0003-concurrent-subscriptions-and-entitlements.md) | Commerce — concurrent subscriptions, scoped entitlements, and PAYG         |
| [0004](adr/0004-reserve-and-settle-accounting.md)             | Accounting — reserve, execute, settle                                      |
| [0005](adr/0005-relational-and-event-storage-split.md)        | Storage — PostgreSQL relational state and Timescale-oriented event history |

## Architecture pages

Reference pages for the designed domain model the ADRs produce — the picture
an engineer needs before writing migrations or the API contract. They describe
the design, not shipped behaviour: the domains they cover are not built yet.

- [Overview](architecture/overview.md) — the five bounded contexts; what
  exists, what owns what, and what words mean
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

These pages link; they do not restate. Where a page and an ADR disagree, the
ADR wins and the page is wrong.
