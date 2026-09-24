# Cross-plane protocols

Reference page for what crosses the boundary between the Control Plane and the
Data Plane, and how it is delivered. The decisions are
[ADR 0006](../adr/0006-control-plane-and-data-plane.md) §5 (the communication
rules) and §9 (the management surface and its service-auth boundary); that
record wins over this page wherever the two disagree. Which record belongs to
which plane is [planes and ownership](planes.md); how the calls are layered in
code is [ports and adapters](ports.md); the money the second direction feeds is
[accounting](accounting.md).

This page exists for the next cross-plane change. A new message between the
planes is either a configuration in the first direction or a fact in the
second, and the two have different obligations — a change that mixes them is
the mistake this page is written to make visible.

## The two directions

| Direction      | Term                       | What crosses                                                                                        | Keyed by                                 |
| -------------- | -------------------------- | --------------------------------------------------------------------------------------------------- | ---------------------------------------- |
| Control → Data | configuration / projection | API-key runtime projection, entitlement and quota grants, catalog and routing config, egress policy | the entity's own identifier — idempotent |
| Data → Control | fact / observation         | usage facts, request-outcome facts, quota-consumption facts, reconciliation state                   | `request_id` — idempotent                |

Neither direction is "data synchronization": one side states what the Data
Plane should hold, the other states what it observed. A projection in the first
row is a delivered copy the Data Plane reads on the hot path; a fact in the
second is not a copy of anything — it is the record.

## Control → Data: an idempotent command

A management call asks the Data Plane to hold something: a catalogue row, a
candidate order, a price revision, a key's runtime state, a capacity grant. The
call is request/response, it is keyed by the entity's own identifier, and it is
idempotent — applying the same instruction twice leaves the Data Plane in the
same state as applying it once, which is what makes a retry safe without a
distributed transaction.

Three properties hold for every call in this direction:

- **It is never on the LLM request path.** The runtime reads what it already
  holds; it does not wait for a management call to serve a completion
  (ADR 0006 §4).
- **It can be refused.** The Data Plane is the authority for what it serves, so
  a management call is a request the Data Plane may reject, and the caller
  handles the rejection rather than assuming the change landed.
- **Delivery to the runtime is a named, bounded property.** Where the
  instruction seeds a projection the hot path reads — a key's revocation, a
  capacity grant — the projection is stale by a bounded window rather than
  consistent on commit, and that bound is a deployment property rather than an
  accident (ADR 0006 §8).

The transport is the chain
`console-api application → dataplane.Management → HTTP adapter → dataplane-api → outbound port → HTTP adapter → dataplane private listener`
(ADR 0006 §9).

## Data → Control: facts, replayed

### The chain

```text
LLM → dataplane → UsageEvent committed durably
                        │
                        ▼
                 dataplane private listener        (internal, not the runtime surface)
                        ▲
                 dataplane-api management façade   (transport only, owns no state)
                        ▲
                 console-api poller                (owns its cursor)
                        │
                        ▼
                 Control Plane settlement
```

The runtime records the fact and stops there. It does not notify anyone, it has
no callback into the Control Plane, and it will serve the next request whether
or not the fact was ever read. The consumer arrives on its own schedule, over
the same authenticated management chain as the first direction, and pays its own
latency for the read (ADR 0006 §4, §5).

### The private hop is a protocol, not a contract

The hop from `dataplane-api` to the Data Plane's private listener is the one
message between the planes that nothing outside this repository reaches. It is
an implementation protocol between two processes of the same product, and it is
deliberately not a fourth OpenAPI document: `api/openapi/` holds the surfaces
something outside this repository talks to, and a document exists to state what
such a caller may rely on. What a caller does rely on is contracted where that
caller's surface is — `dataplane.yaml` declares `GET /internal/usage-events` on
the **façade**, and `shared/usage-facts.yaml` carries the page it may expect.

The distinction is the one thing about this hop that is easy to get wrong, so it
is worth stating flatly: **`dataplane.yaml` is dataplane-api's contract.** The
listener in `dataplane` is not that surface and does not implement that document.
A reader looking for "does this process serve what the contract says" is asking
about `apps/dataplane-api`; the question about `apps/dataplane`'s listener is
"does it serve what the protocol below declares", and it is answered by that
package's own route test and its own protocol test.

The two hops do carry the same page and do not share a failure vocabulary. That
asymmetry is the reason the hop is defined rather than assumed, and the sections
below are the whole of it. Changing it is a change to the two protocol tests
(`apps/dataplane-api/internal/adapters/outbound/dataplane/protocol_test.go` and
`apps/dataplane/internal/adapters/inbound/management/protocol_test.go`) and to
this page, and no generated client moves (AGENTS.md rule 2).

### The private protocol, in full

|                |                                                                                                                                                                                                                                                                                                                                                                    |
| -------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Endpoint       | `GET /internal/usage-events`                                                                                                                                                                                                                                                                                                                                       |
| Methods        | `GET`, with `HEAD` answered from the same registration. Every other verb is `405` with `Allow: GET, HEAD` once the caller is authenticated — an unauthenticated one is `401` first, which is the note under this table                                                                                                                                             |
| Authentication | `Authorization: Bearer <credential>`, RFC 6750 shape — exactly one header, exactly two fields, case-insensitive scheme, token carrying no whitespace. The credential is a **separate shared secret** from the caller-facing one; each hop checks its own, and neither hop accepts the other's                                                                      |
| Parameters     | `after` — optional, opaque, at most 512 characters; omitted means "from the beginning of what is retained". `limit` — optional integer, `1`–`1000`, absent means `100`. Supplying either twice is refused. Any other parameter name is refused                                                                                                                     |
| Success        | `200`, body `{"events":[…],"next_cursor":"…","has_more":true\|false}`, the three keys and the five event keys spelled exactly as `shared/usage-facts.yaml` spells them — and all eight required: a `200` that omits or nulls any of them is not a page, and the façade's decoder refuses it rather than re-encoding it                                             |
| Failure        | `{"error":{"code":"…","message":"…"},"request_id":"…"}`, the same envelope shape the façade uses, with this listener's own code set: `invalid_request` (400), `unauthenticated` (401), `not_found` (404), `method_not_allowed` (405), `cursor_expired` (410), `internal` (500). Every response, including the ones produced before routing, carries `X-Request-Id` |

Two notes about the table, then the four properties that are load-bearing and
are stated here rather than derived from the code.

`405` is a statement about an authenticated caller. Authentication runs before
routing on both hops, so `DELETE /internal/usage-events` with no credential is
`401`, not `405`, and so is one carrying a credential this listener does not
accept — the answer to "may I speak here" is never dependent on which verb was
used. ADR 0006 §9 gives the reason: the surface is invisible to a caller the
deployment does not trust, and a method probe that answered differently would
map it for a caller with no business knowing it exists. The façade's inbound
adapter orders the two the same way, which is what makes the two hops
indistinguishable on this point rather than merely similar.

A read of the table also shows what the two hops do **not** share: the status
codes and the six-code failure vocabulary are this listener's own, and neither is
this listener's to pass along. The envelope is the one thing they do share — the
shape `{"error":{…},"request_id":"…"}` is the same on both — and even there each
hop writes it from its own values rather than copying the other's. The next
section is the mapping.

- **The path string is the same on both hops, on purpose.** The façade carries
  the caller's values across rather than forwarding the request object, so the
  path it calls is one it names itself — and it names the one it published. A
  path rewritten hop by hop would be a translation step the protocol has to
  describe and could get wrong. Both ends assert the literal against
  `dataplane.yaml`, so the sameness is pinned from two directions instead of
  being a coincidence someone eventually edits apart.
- **The cursor is minted here and nowhere else.** This listener is the only
  component that may produce one, compare two, or order by one. It accepts an
  absent `after` as "the beginning" and never an empty one: a caller that sends
  `after=` has a broken position, and answering it as a request for the whole
  retained history would hide that. A value this Data Plane cannot place is
  `410 cursor_expired` and is never silently rounded to a neighbouring position.
- **The listener refuses a page it cannot describe.** A fact source that answers
  without a position is this process's own failure and is reported as `500
internal` — not as a page, because a consumer that stored an empty position
  would re-read the same facts forever behind one that never moves.
- **Its failures are its own.** `upstream_unavailable` is the façade's word for
  "the Data Plane did not answer", which is not a sentence this process could say
  about itself; it does not appear in the listener's vocabulary at all. The
  protocol test on that side asserts the vocabulary is closed, so the code cannot
  be introduced by accident.

### What the façade does with those failures

`dataplane-api` translates; it does not relay. The table below is the whole
mapping, and the rule behind it is that the façade answers its caller in the
caller's vocabulary:

| The private listener answered                                                                                                                                                                      | The façade answers the caller                                     | Why                                                                                                                                                                                                                                                                                                                                     |
| -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `200` with a page                                                                                                                                                                                  | `200` with **that page, re-encoded** from the façade's own values | Success. The façade writes the body; it does not copy one                                                                                                                                                                                                                                                                               |
| `410 cursor_expired`                                                                                                                                                                               | `410 cursor_expired`                                              | The one refusal a consumer must act on rather than retry — it has to re-establish a position, and skipping forward would lose settlement. The façade decides this from the status class and builds its own envelope; the two hops happening to spell it the same way is a consequence of the translation, not a substitute for it       |
| `400`, `401`, `404`, `405`, `500`, any other status                                                                                                                                                | `502 upstream_unavailable`                                        | The façade validated its caller's request at its own boundary, so every one of these is the façade's problem or the deployment's, and never the caller's. `401` here means _this process's_ credential was refused — reporting it to a caller that sent no credential would send an operator to rotate the wrong secret                 |
| A transport failure, or a body the façade cannot read                                                                                                                                              | `502 upstream_unavailable`                                        | The answer is unknown and the façade holds nothing it could answer with instead                                                                                                                                                                                                                                                         |
| A `200` whose required fields do not arrive — no `events`, no `has_more`, a fact missing one of its five fields, or a position that is missing, empty or longer than the contract's 512 characters | `502 upstream_unavailable`                                        | The façade contracts the page's envelope and each fact's five fields as required and the position as bounded, so answering with such a body would contradict the façade's own document. Refusing here is the façade keeping _its_ promise; the consumer keeps its own, separately, about the page it applies and the position it stores |
| A failure the façade's own port does not describe                                                                                                                                                  | `500 internal`                                                    | This process's bug, reported as this process's bug                                                                                                                                                                                                                                                                                      |

The consequence to hold on to: **no byte of the private listener's body reaches
a Control Plane caller.** The façade's 502 means it produced no answer from the
Data Plane — unreachable, or reachable and unusable — and the caller cannot tell
which. It keeps meaning that because the façade has no other status to answer
these cases with: the one other refusal in the table, `500 internal`, is the
façade reporting its own bug rather than the hop.

It also means something else, and the cost is worth stating where the mapping is
written rather than discovered by an operator. A `502` covers both a transient
outage and a mismatch between the façade and the process behind it — a page
whose required fields are absent or null, whose position is missing, empty or
longer than the contract's 512 characters, or shaped in a way this build's
decoder cannot read. The first is retried and the second
is not, and a caller cannot tell them apart from the answer. The consumer
therefore has to bound its own retries: a permanent mismatch retried forever
looks exactly like an outage retried forever, and the distinguishing signal —
how many times the same page has now failed — lives in the loop that reads the
position, which is the loop this page defers to the schema PR ("What this page
does not decide"). Nothing in the façade can produce that signal, because
producing it would mean the façade remembering how many times it had answered
the same way, which is state it may not hold.

### The two protocol tests

Each side carries one, and neither can import the other — the applications are
separate Go modules (ADR 0006 §1) — so each states the same literals and each
fails when its own side moves away from them. The listener's asserts the path
against `dataplane.yaml`, the envelope's key set, and that its failure
vocabulary is exactly the six codes above and contains no façade-only word. The
façade's asserts the path against the same document, that the request carries
exactly the protocol's two parameters, and that no body the listener could send
survives into the façade's error text.

They also pin the numbers of the table, by reading them out of
`api/openapi/shared/usage-facts.yaml` rather than repeating them, and the
division is worth spelling out because it is not symmetrical. The listener's
protocol test and the façade's inbound route test each read all five out of the
document — the page size's minimum, maximum and default, and the cursor's
maximum and minimum length — because each of those surfaces enforces all five.
The façade's **outbound** adapter pins only the cursor's 512, which is the one
of them it reads; and the Control Plane's outbound adapter pins the five as
well, so the bound it refuses a stored position against and the bounds its own
requested page size has to sit inside are both tied to the file. Four packages
across three modules hold copies of these numbers, and each is checked against
the document by the package that owns it; a pin that compared a constant to
itself would let one hop widen its bound and quietly disagree with the hop in
front of it, which is the failure the numbers are there to prevent.

The limits are `1`, `1000` and `100`, and the cursor is 512 characters rather
than 512 bytes: `maxLength` counts code points, so every hop counts them with
`utf8.RuneCountInString` and each has a test that fails if it stops. The page
size a consumer **asks for** is not one of the five — it is the consumer's own
choice, which the contract only bounds — so the Control Plane's is held to
`1..1000` rather than to the default, by a test in the flow that asks.

### The cursor is opaque

The cursor names a position in the Data Plane's fact order, and it is issued by
the Data Plane alone. No consumer may parse it, synthesise one, compare two, or
order by it; `dataplane-api` forwards it verbatim even though it never uses it.
The encoding is the Data Plane's to change, and treating the value as
meaningful is what would make that change breaking
(`api/openapi/shared/usage-facts.yaml`).

One property of the position is fixed even though the value is not, and the two
are not in tension: **`next_cursor` is the position of the last fact in the
page, and a read resumes strictly after the position it is given.** An empty
page therefore returns the position it was read from, and a first read that
carried no `after` still returns one.

The convention matters because the alternative — a cursor naming the position
_immediately after_ the last fact delivered — round-trips differently at the one
boundary that is hard to see. Under that reading a read of the position that is
also the first unread fact's own position yields that fact; under this one it
yields the facts after it. Both are self-consistent, and the two differ only for
a consumer that constructs a position itself instead of storing one it was
given — which is forbidden anyway. What is not a matter of taste is which way a
mistake fails: this convention re-delivers a fact at worst, and re-delivery is a
no-op the applier already handles, whereas the other can drop one, and a dropped
fact is settlement that never happens.

The Control Plane stores the `next_cursor` of the last page it applied. An
empty stored position means "from the beginning of what is retained" — that is
the consumer's own convention for having no position, and it is why the Data
Plane may never answer with one. The two would collide: a consumer that stored
an empty `next_cursor` would read it back as "start again", ask for the whole
retained history on every cycle, and never advance, with nothing anywhere saying
so. Both hops refuse such a page rather than serve it — the listener as `500
internal`, the façade as `502 upstream_unavailable` — and the contract states
the requirement the two are enforcing.

### Replay and retries

Replay is normal, not an error path. The same range may be requested any number
of times and returns the same facts, because reading is side-effect free: there
is no acknowledgement endpoint, no consume-and-delete, and no way for a consumer
to tell the Data Plane it has finished with anything. A Control Plane outage
therefore loses no facts, and a redelivered fact costs nothing.

The retry rules follow from that:

- **The consumer advances only after applying.** The `next_cursor` is stored in
  the same local transaction that durably records the effects of the facts it
  covers, so a crash between applying and advancing replays the page instead of
  skipping it. The page is also the unit of belief, and it is believed only
  once it is whole: **a usage-fact page is acknowledged by the consumer only
  after its required envelope and event fields have been validated, and a
  malformed page is rejected before anything is applied and before the position
  moves.** Decoding alone proves nothing — a body with a position and no facts
  would otherwise apply nothing and advance anyway, which is the one way this
  flow could skip facts permanently. A rejected page leaves the position where
  it was, so the range is read again rather than crossed.
- **Applying a fact twice is a no-op.** `request_id` is the fact's immutable
  business identity and the logical idempotency key for everything derived from
  it — one settlement, one consume leg and one release leg per `request_id`,
  however many times the fact is delivered. It is not the HTTP `X-Request-Id`,
  which correlates a call and means nothing across a retry.
- **A transport failure is a retry; a refused position is not.** An unreadable
  or unreachable Data Plane is retried. A cursor past the retained history fails
  explicitly with `cursor_expired` (HTTP 410) and is never resumed from a newer
  position: silently skipping facts turns a retention decision into lost
  settlement, and it needs a human rather than another attempt.

### Reconciliation

The two planes converge rather than synchronise. The Control Plane derives its
records — the settlement, its consume and release legs, the bucket projections —
from the facts it has applied, idempotently by `request_id`, and it must be able
to do so **without a synchronous call back into the Data Plane**. That is why
the fact carries the allocation and bucket identities and the immutable
reservation figures needed to derive those legs and the settled total, and why
the fact feed carries at most one settlement-relevant terminal fact per runtime
request (ADR 0006 §5 and its amendments to
[ADR 0004](../adr/0004-reserve-and-settle-accounting.md); the contract's
requirement is stated in `api/openapi/shared/usage-facts.yaml`).

What reconciliation cannot repair is a fact that was never written or never
read within retention; it repairs a **lag**, not an absence. A reconciliation
path that needed to ask the Data Plane a question mid-settlement would be a
synchronous cross-plane call wearing the fact model's clothes, and it is the one
thing this protocol does not permit.

## The failure model

Stated once, here. Every cross-plane behaviour reduces to one of these four
lines:

```text
Control unavailable  → Data continues serving
Data unavailable     → management operations fail or defer
Delivery failure     → the fact stays durable, replay later
Duplicate delivery   → idempotent no-op
```

The first line is what the split exists for (ADR 0006 §2, §4); the second is
why a management call is request/response with a defined failure rather than a
two-phase commit; the third is why the consumer, not the producer, holds the
position; the fourth is why nothing in the derivation needs a lock.

## What this page does not decide

- **The durable fact source.** `usage_events` is the future table the facts are
  read from; its columns, its ordering sequence and its retention are the schema
  PR's, and this page names the requirements they have to satisfy rather than
  the shape (ADR 0006 §5; [data implications](data-implications.md)).
- **The ingestion-cursor schema.** `control.usage_ingestion_cursor` is the
  future table the Control Plane stores its position in. Today the position is a
  repository-level port in `apps/console-api/internal/ports/outbound/persistence`
  with the future table documented beside it and test fakes behind it; a
  production implementation arrives with the schema rather than before it. On
  the other side of the same seam, the Data Plane's production fact reader
  refuses with a source-unavailable error instead of serving an in-memory feed
  that would lose every fact on restart.
- **The settlement consumer loop.** Nothing schedules the replay: there is no
  worker, no ticker and no background process. The loop that calls the ingestion
  use case belongs to the pull the schema PR builds, beside the table the cursor
  is stored in.
- **mTLS or signed service credentials.** The mechanism today is a shared secret
  per hop, and either replacement proves the same identity cryptographically
  without changing application semantics (ADR 0006 §9).
- **A bound on how much of an answer either hop will read.** Both outbound
  adapters decode the peer's body with `json.NewDecoder(response.Body)` and no
  size cap, so a peer — or anything able to answer in a peer's place — decides
  how much memory a read costs. It is left as it is because the number has to
  come from the contract rather than from an adapter: neither `events` nor
  `payload` carries a `maxItems` or a `maxLength` today, so any cap written now
  would be a guess that refuses pages the contract admits, which is the one
  failure this page spends its length avoiding. The change is small and belongs
  with the fact schema, when `payload`'s columns and their sizes are decided:
  an `io.LimitReader` at each decode, one above the largest page the contract
  permits, and a test that feeds a body past it and watches the read fail
  instead of the process grow. Recorded rather than fixed because guessing the
  number would be worse than not having one.
- **`X-Request-Id` forwarding across the hops.** Every surface here issues and
  returns one, and a caller may supply a well-formed one, but no outbound
  adapter sends the header onward — so the identifier in the façade's error
  envelope is the façade's own and does not identify the request in the
  listener's logs. Spanning the hops is a small change with a real question
  attached, since a caller-supplied string would then cross a second trust
  boundary, and it belongs with the consumer loop that will have a request worth
  correlating.
