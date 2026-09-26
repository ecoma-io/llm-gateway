# Cross-plane protocols

Reference page for what crosses the boundary between the Control Plane and the
Data Plane, and how it is delivered. The decisions are
[ADR 0006](../adr/0006-control-plane-and-data-plane.md) §5 (the communication
rules) and §9 (the management surface and its service-auth boundary), and
[ADR 0007](../adr/0007-control-to-data-projection.md) (the credential
projection's protocol); those records win over this page wherever the two
disagree. Which record belongs to which plane is [planes and ownership](planes.md);
how the calls are layered in code is [ports and adapters](ports.md); the money
the second direction feeds is [accounting](accounting.md).

This page exists for the next cross-plane change. A new message between the
planes is a configuration in the first direction, a fact in the second, or —
since the commerce foundation — a **read** in the first: a question about what
the Data Plane already holds, documented with the other two because it rides
their chain and must keep their discipline. A change that mixes their
obligations is the mistake this page is written to make visible.

## The two directions

| Direction      | Term                       | What crosses                                                                                        | Keyed by                                                                      |
| -------------- | -------------------------- | --------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------- |
| Control → Data | configuration / projection | API-key runtime projection, entitlement and quota grants, catalog and routing config, egress policy | the entity's own identifier for a command; the projection's revision position |
| Data → Control | fact / observation         | usage facts, request-outcome facts, quota-consumption facts, reconciliation state                   | `request_id` — idempotent                                                     |

Neither direction is "data synchronization": one side states what the Data
Plane should hold, the other states what it observed. A projection in the first
row is a delivered copy the Data Plane reads on the hot path; a fact in the
second is not a copy of anything — it is the record.

## Control → Data: an idempotent command

A management call asks the Data Plane to hold something: a catalogue row, a
candidate order, a price revision, a capacity grant. The call is
request/response, it is keyed by the entity's own identifier, and it is
idempotent — applying the same instruction twice leaves the Data Plane in the
same state as applying it once, which is what makes a retry safe without a
distributed transaction. The key credential is deliberately **not** on this
list: it crosses as the projection below, because a mirror of thousands of
rows needs order, replay and a position, and a command has none of those
things to give it.

Three properties hold for every call in this direction:

- **It is never on the LLM request path.** The runtime reads what it already
  holds; it does not wait for a management call to serve a completion
  (ADR 0006 §4).
- **It can be refused.** The Data Plane is the authority for what it serves, so
  a management call is a request the Data Plane may reject, and the caller
  handles the rejection rather than assuming the change landed.
- **Delivery to the runtime is a named, bounded property.** Where the
  instruction seeds a projection the hot path reads — a capacity grant, or the
  entitlement that seeds one — the projection is stale by a bounded window
  rather than consistent on commit, and that bound is a deployment property
  rather than an accident (ADR 0006 §8).

The transport is the chain
`console-api application → dataplane.Management → HTTP adapter → dataplane-api → outbound port → HTTP adapter → dataplane private listener`
(ADR 0006 §9).

Everything above describes a command. The direction also carries its first
**read** — the alias-group current-version lookup,
`GET /internal/alias-groups/{group_name}/versions/current`, which the commerce
due-work lanes call to resolve grant-definition scopes before their units of
work open ([commerce](commerce.md)). It rides the same chain, the same
service-auth boundary and the same two-hop shape as the commands, and it keeps
their discipline:

- **The façade translates, never relays.** A `404` is the catalog's answer
  that the group has no version, carried to the caller as its port's
  not-found sentinel; every other status, and every unreadable answer, is
  `502 upstream_unavailable`. No byte of the private listener's body reaches
  a Control Plane caller, exactly as in the fact direction.
- **The group name is one path segment, escaped.** The wildcard group's `*`
  travels percent-encoded; the segment's form is part of the contract, not
  the caller's choice.
- **A route-absent `404` is indistinguishable from a catalog miss.** The
  façade translates every listener `404` the same way, so a private hop the
  deployment never routed — a version skew where the façade runs ahead of
  its listener — arrives as the port's not-found sentinel too, the same
  answer a group with no version gets. The skew is therefore fail-safe: the
  roll stops its pass and sells nothing, exactly as a real catalog defect
  makes it. What it is not is separable — telling "route missing" from
  "group missing" apart would need the listener to speak a second status
  code the contract does not give it, and the roll's operator-facing
  symptom ("a plan grants a scope that resolves to nothing") names the
  commercial truth either way.
- **A miss is an answer, not a transport failure.** The roll lane stops its
  pass on one on purpose — a plan granting a scope that resolves to nothing
  is a commercial catalog defect, and skipping it every pass would silently
  sell a plan that grants nothing.

`dataplane.yaml` declares the route on the façade; the listener side is this
hop's second private protocol, pinned by that package's own route and
protocol tests as the fact reader's is.

## Control → Data: the credential projection

The second protocol in this direction carries the one record whose authority is
unambiguously the Control Plane's and whose reader is the Data Plane's hot
path: the API-key credential and the account lifecycle that gates it
([ADR 0007](../adr/0007-control-to-data-projection.md), which is the decision;
this section is the protocol as the two ends speak it).

The producer is a stateless reconciliation loop in `console-api` — one cycle
per interval, no buffer, no cursor, no learned position of its own. Each cycle
reads the Control Plane's log head and the Data Plane's position and answers
the difference: deliver a snapshot when the position does not join this
timeline, otherwise drain the log in batches until the feed is empty. The
consumer is the Data Plane's mirror, and its half of the bargain is one
transaction: **the rows and the position that says they were applied commit
together**, which is the property every recovery story below stands on.

### The chain and the three operations

The same chain as every cross-plane call, three operations on it:

| Operation          | Contracted at the façade             | Behind it, on the private listener          |
| ------------------ | ------------------------------------ | ------------------------------------------- |
| Read the position  | `GET /internal/projection/position`  | same path, answered from `projection_state` |
| Deliver a snapshot | `POST /internal/projection/snapshot` | same path, replacing the mirror whole       |
| Deliver a batch    | `POST /internal/projection/changes`  | same path, judged against the position      |

The schemas are `api/openapi/shared/projection.yaml`; the operations are
declared in `dataplane.yaml` because the façade is the surface an outside
caller may rely on, and the listener behind it is the private protocol — the
same division the usage-fact direction keeps, pinned by one protocol test on
each side of the hop.

One asymmetry with the fact feed is deliberate and worth naming, because it is
the difference between the two protocols' middle hops. There, the **answer**
crosses as values the façade reads once and re-encodes from its own hand. Here,
the **request** crosses as bytes the façade refuses to touch: the delivery body
is forwarded verbatim, undecoded and unvalidated, because the message is the
producer's grammar, built and checked where it was assembled, and a middle hop
that re-encoded it would be a second grammar — one that would strip exactly the
unknown additive fields the version rule promises to carry. The version rides
inside those bytes for the same reason: the façade does not judge it, because
the two ends that must agree on a version are the producer that writes it and
the listener that refuses an unknown one, and a middle hop restating the check
would age at its own release cadence. What the façade
owns on this hop is the transport around the bytes — the credential check
before anything runs, the contract's ten-mebibyte body bound enforced before
anything is forwarded — and the failure vocabulary, which is the next table.

### The five rules

They are stated in full in the contract fragment's header; they are the whole
protocol, so they are named here too:

1. **Order is a revision, never a clock.** Revisions come from a gapless
   counter advanced inside the same transaction as the authoritative write
   (ADR 0007 §1); `recorded_at` rides along for operators and is never an
   ordering key. The ceiling is 2⁶³ − 1 — the wire's revisions are unsigned,
   but both planes store them in `bigint`, so the contract's `maximum` turns
   an oversized revision into a grammar refusal, not a storage error.
2. **The timeline is named.** Every message and every position carries the
   producer's `epoch` — how a database restored to an earlier instant is
   noticed instead of silently skipping every change after the restore
   (ADR 0007 §6). One operator step rides with this rule: the epoch is part
   of the data a restore puts back, so the restore procedure re-mints it
   (`UPDATE control.projection_revision SET epoch = gen_random_uuid()`) —
   a restore that skipped the step would rewind the counter under the old
   name, the one mismatch the epoch cannot see.
3. **Bootstrap is a snapshot; everything after is a batch.** The snapshot is
   unconditional — "be this state at this boundary" — and it sets the position
   in the same transaction as the rows it carries. The producer may re-snapshot
   at any time; zero is a legitimate boundary (ADR 0007 §4).
4. **Delivery is at-least-once; application is idempotent.** Every entry is a
   self-contained full-state row, never a delta, so a redelivery is harmless by
   construction — and cheap: a batch wholly behind the position is acknowledged
   without work.
5. **The position advances in the same transaction as the rows it describes.**
   A crash before the commit replays; a crash after it re-acknowledges. There
   is no state in which rows are applied and the position does not say so.

### The consumer's three-case rule

The consumer judges each batch against its own stored position — never against
the batch's `from_revision`, which names only what the producer believed — and
answers with exactly one of three outcomes:

| The batch's revisions against the stored position | Answer                                                                        | Why                                                                                            |
| ------------------------------------------------- | ----------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------- |
| `first == position + 1`                           | apply whole, under per-row `source_revision` guards, position = last revision | the ordinary case                                                                              |
| `last <= position`                                | acknowledge, no work                                                          | a lost acknowledgement, not an error — this is what makes at-least-once free                   |
| anything else (straddles or overshoots)           | refuse **whole** — `revision_gap`                                             | applying the entries that fit would strand the rest behind a position that has moved past them |

A gap is never waited for. There is no buffer and no "hold until revision N
shows up" — an aborted producer transaction releases its revision, so some gaps
are permanent, and a wait with no bound is a stall with no signal. The gap's
only answer is a re-snapshot, and the producer's own loop is what asks for it.

One refusal rides inside the ordinary case: a batch that joins the position
but delivers an entry moving a terminal row (`revoked`, `closed`) back to a
live state is refused **whole** as `invalid_request` — and deliberately
without a heal, because a snapshot would overwrite the terminal row the
refusal protects. A conforming producer cannot express such an entry
(ADR 0007 §5), so the drain wedges loudly every cycle until an operator
looks at the channel.

### What the refusals mean, and what the producer does

The listener's refusal vocabulary is four codes with two statuses, and each
names the producer's next move rather than describing a failure:

| Code                                                                      | The listener answers                    | The producer's next move                                                                                                 |
| ------------------------------------------------------------------------- | --------------------------------------- | ------------------------------------------------------------------------------------------------------------------------ |
| `unsupported_version`                                                     | `400`                                   | fail loud, every cycle, until both ends speak the same version — never skipped, never retried into working               |
| `invalid_request`                                                         | `400`                                   | fail loud — a message this process built cannot satisfy the grammar it speaks; no retry fixes that                       |
| `revision_gap`                                                            | `409`                                   | re-snapshot **in the same cycle**; resume draining from the boundary the snapshot committed                              |
| `snapshot_required`                                                       | `409`                                   | re-snapshot in the same cycle                                                                                            |
| any other code, any other status, a transport failure, an unreadable body | the façade's `502 upstream_unavailable` | fail the cycle; the next interval retries — at-least-once against a durable log costs one redelivery and nothing durable |

The rows split into two kinds of answer, and the split is the producer loop's
whole judgement: the first two are verdicts ("this build is wrong" — surfacing
them every cycle is the operator's signal), the next two are instructions
("deliver differently" — the protocol's own recovery), and the last is
everything unknown, which costs a log line and a wait. Nothing is ever skipped
past a refusal: the three-case rule's third answer exists so that a batch that
cannot join the position is refused loudly instead of applied partially, and
the loop's answer to loud is a snapshot, not silence.

Secrets stay out of the vocabulary by construction: the producer's errors are
built from status codes and sentinels — the transport's own error text quotes
the request URL and is dropped rather than wrapped, and a refusal body is read
for its `error.code` and nothing else, under a size cap — so no credential, no
DSN, no listener message text reaches a log line the loop writes every
interval.

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
the **façade**, `shared/usage-facts.yaml` carries the page it may expect, and
the same document declares the façade's group-version read
([above](#control--data-an-idempotent-command)).

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

That is one instance of a roster every new management command answers to, and
the roster is short enough to enumerate where the hop is defined. A command, a
read or a fact moves three files, in one change, and the first of them landing
alone is the stale page the list exists to prevent:

- [ ] the section of this page that states the protocol;
- [ ] the `dataplane.yaml` operation the façade publishes;
- [ ] the two pinned protocol tests
      (`apps/dataplane-api/internal/adapters/outbound/dataplane/protocol_test.go`,
      `apps/dataplane/internal/adapters/inbound/management/protocol_test.go`).

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

What the derivation reads, concretely, is the landed fact's two halves. The
**typed columns** are the settlement's inputs: `capture_method` (the three
values [accounting](accounting.md) prices confidence by), the normalized token
counts, the price snapshot (revision and both unit prices), the settled amount,
and `schema_version`. The **payload** is an opaque jsonb envelope, versioned by
that `schema_version` rather than by convention — today `v1`, carrying exactly
one thing, the allocation legs:

| Payload `v1` field                | Type    | What a Control-Plane settlement derives from it                                                           |
| --------------------------------- | ------- | --------------------------------------------------------------------------------------------------------- |
| `allocations[].funding_bucket_id` | string  | which ledger bucket each `consume`/`release` leg lands in                                                 |
| `allocations[].amount`            | integer | the held amount per bucket — the ceiling the consumed figure is subtracted from                           |
| `allocations[].ordinal`           | integer | the waterfall position, preserved so split-order legs reach the ledger in the order the runtime drew them |

A settled fact's consume legs are priced from the typed columns against each
leg's `amount`; the release legs are the unconsumed tails. Anything a future
envelope adds is an explicit field with a bumped `schema_version` — never a
convention hidden in untyped bytes, and never a second meaning for a `v1`
field.

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

The projection meets the same four lines in the first direction, with its own
vocabulary: a Control Plane outage pauses the projection cycles and nothing
else; a Data Plane outage costs one failed cycle per interval and one log line;
a delivery failure leaves the change durable in the log, replayed by the first
cycle that finds the mirror behind; and a duplicate is the three-case rule's
second answer — an acknowledgement without work (ADR 0007 §5, §7).

### The fact source, decided and landed

The one question this page used to leave open on this seam is closed:
`usage_events` (B7's `migrations/dataplane/000003_runtime_storage`) is the
table the facts are read from, and the requirements this page names are
properties of that schema rather than promises about a future one. Ordering is
**commit ordering**: each fact's `append_seq` is allocated from the single
`usage_events_stream` row under its row lock held to the writer's commit, so
the order a consumer reads is the order the appends became visible — no clock
participates. The stream row's `epoch` is a server-minted uuid from the first
append (never a migration), and the position a page carries is minted from
that pair — epoch and sequence — which is why a position from a lost and
re-seeded stream is refusable rather than silently reusable. The encoding
remains this Data Plane's private affair: still opaque to every consumer,
still changeable, and now with a landed fact order behind it and a landed
reader over it — the postgres adapter's `UsageFacts`
([ports and adapters](ports.md)) — rather than a promise
(ADR 0006 §5; [data implications](data-implications.md)).

### The fact consumer, decided and landed

The other half of the seam has landed the same way, and the pair of "future
tables" ADR 0006 §5 named is now two landed ones. The Control Plane's
position lives in `control.ingestion_cursor` (B12's
`migrations/control/000008_fact_ingestion`) — a singleton row holding one
opaque string, written verbatim from the page's `next_cursor` and read by
nothing outside the consumer module — behind the repository-level port that
predates the table ([ports and adapters](ports.md)). The consumer that drives
it is the replay use case and the process loop that calls it: one page per
pass, fetched outside any transaction, applied whole — every fact, then the
position's advance in the same unit of work, last. Duplicate delivery is
answered by the applier's `(request_id, kind class)` idempotency rather than
by any protocol acknowledgement; a fact the consumer refuses to interpret is
recorded verbatim in the quarantine and the page still advances; a fact the
plane cannot apply — an accounting refusal, a payload past the column's
bound — stops the page where it stands, so the position never claims work
that did not commit ([accounting](accounting.md);
[data implications](data-implications.md)). What remains of
[issue #63](https://github.com/ecoma-io/llm-gateway/issues/63) is the
accepted deferral it arrived with — one settlement currency, provenance
beyond the revision id — tracked rather than blocking.

## What this page does not decide

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
  failure this page spends its length avoiding. What has changed with the
  landed fact schema is that the number is now **derivable** rather than
  unknowable: the source caps `payload` at 32768 octets by schema CHECK
  ([data implications](data-implications.md)) and the contract caps a page at
  1000 events, so the largest honest page is on the order of tens of MiB
  (~32 MiB — 33 MB decimal) — an `io.LimitReader` set just above it can
  be written the day the contract carries the number, and not before. The
  change is small and belongs with the fact schema: a limit at each decode,
  one above the largest page the contract permits, and a test that feeds a
  body past it and watches the read fail instead of the process grow.
  Recorded rather than fixed because writing the limit ahead of the contract
  would be the guess this bullet exists to prevent — the derivation above is
  the migration's word, and the contract's `maxLength` is still owed.
- **`X-Request-Id` forwarding across the hops.** Every surface here issues and
  returns one, and a caller may supply a well-formed one, but no outbound
  adapter sends the header onward — so the identifier in the façade's error
  envelope is the façade's own and does not identify the request in the
  listener's logs. Spanning the hops is a small change with a real question
  attached, since a caller-supplied string would then cross a second trust
  boundary, and it belongs with the consumer loop. That loop has landed and
  does not send the header either — its passes are already the boundary-crossing
  calls the question is about — so the change remains owed, still small, and
  still recorded rather than made.
