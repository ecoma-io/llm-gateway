# ADR 0007: The Control → Data Credential Projection

- Status: Accepted
- Date: 2026-09-25
- Issue: [#51](https://github.com/ecoma-io/llm-gateway/issues/51)
- Amends: [ADR 0006](0006-control-plane-and-data-plane.md) (§5, §8)

## Context

ADR 0006 §8 declared the property — the Data Plane holds a _projection_ of key
state, minted and revoked in the Control Plane, "delivered to the runtime so
that authentication costs no cross-plane call", with a revocation staleness
that is "a named deployment property rather than an accidental one". It named
no mechanism. For four words of the projection ("key projections", §5's first
table row) there was nothing to deliver with: no table, no message, no ordering
rule, no recovery story. The rows did not exist in either database, and the
only thing on the wire was the plan.

The mechanism had to satisfy constraints that pull against each other:

- **No cross-plane transaction** (ADR 0006 §5): the Control Plane's decision
  and the Data Plane's application are two commits in two databases. The
  protocol's whole job is making that pair converge without ever pretending
  they are one.
- **The request path never waits on it** (ADR 0006 §4): delivery may not ride
  the mint call, because a mint that cannot complete when the Data Plane is
  down would put the management plane back on the hot path it was split off.
- **At-least-once delivery is inevitable**: no cross-plane acknowledgement is
  transactional, so every message may arrive twice. Application has to be a
  no-op the second time by construction, not by care.
- **Ordering cannot be a clock**: `recorded_at` crosses time zones, NTP steps
  and restore boundaries; a wall clock cannot decide which of two states is
  the later one.
- **The consumer may be down for a week.** Whatever carries the changes must
  be durable on the producer's side and replayable from before the outage —
  which excludes every in-memory queue and makes a broker's durability
  guarantees necessary but its operational cost gratuitous. The task's own
  constraint settles the trade: no Kafka, no NATS, no RabbitMQ.
- **One direction only.** The Data Plane never writes credentials back, never
  acknowledges out of band, and never asks the Control Plane to mark anything
  delivered — the same shape ADR 0006 §5 already fixed for the reverse
  direction, pointed the other way.

The scope is identity only. What projects is the credential record a runtime
verifies against — key id, account id, digest, lifecycle state — and the
account lifecycle state that gates admission. Catalog configuration stays what
ADR 0006 §5 says it is, a management command the Data Plane can refuse. The
runtime authentication that will _read_ this projection is task B8; the routing
that will read the rest of the mirror is B9. This ADR builds the pipeline and
stops there.

## Decision

The projection is a durable, ordered, self-contained change log in the Control
Plane's own database, a reconciliation loop that drains it, and a mirror in the
Data Plane's database whose position advances in the same transaction as the
rows it describes. No broker, no in-memory queue, no shared table, no fourth
process.

```text
console-api use case (mint / revoke / suspend / close)
        │  one transaction: authority write + log entry + mirror row + counter
        ▼
control DB ── projection_revision · projection_changes · projection_api_keys · projection_accounts
        │
        ▼  reconciliation loop (console-api), one Reconcile per interval
ports/outbound/dataplane → HTTP adapter → dataplane-api (façade, contracted)
        │  body forwarded verbatim; failures translated, never relayed
        ▼
dataplane private management listener → dataplane application → mirror apply
        │  one transaction: rows + position
        ▼
dataplane DB ── projection_api_keys · projection_accounts · projection_state
```

### 1. The producer's durable state

Four tables in the control database
(`migrations/control/000004_projection_foundation.up.sql`) carry the pipeline:

- **`control.projection_revision`** — the revision counter, a singleton row:
  an `epoch` UUID assigned once by the migration and a `last_revision`. A
  revision is allocated by `UPDATE … SET last_revision = last_revision + 1
RETURNING last_revision` **inside the writer's transaction**. The row lock
  the update takes is held until commit, so allocation order is commit order:
  two authorities recording concurrently get distinct revisions ordered by
  which commit landed first, and an aborted transaction releases its revision
  together with its change — which is what keeps the sequence **gapless**. A
  gapless sequence is not cosmetic: the three-case rule in §5 below detects a
  missing revision only because every revision is either delivered or was
  never issued.
- **`control.projection_changes`** — the log. Each entry: `revision` (primary
  key), `resource_kind`, `resource_id`, `recorded_at`, and a `payload` of
  jsonb carrying the row's **full state at that revision — never a delta**.
  The entry is self-contained, so replay needs no surrounding context and a
  redelivery is idempotent by construction. A database CHECK requires an
  `api_key` payload to carry a digest and pins it to 64 lowercase hex
  characters — required in the CHECK itself, not merely by the writer — so
  the log cannot hold what the contract would refuse.
- **`control.projection_api_keys` / `control.projection_accounts`** — the
  materialized current state, one row per resource, each carrying the
  `source_revision` it was written from. This is the snapshot's source: cutting
  a snapshot is a read of these tables, not a replay of the log.

### 2. Recording is part of the authority transaction

The recording hooks live inside the use cases' existing unit of work, and the
order inside it is fixed: **the authority write first, the record last**. A
compare-and-swap loser describes nothing — `RecordCredentialChange` runs only
on the path that moved a row. The authority write's precedence is not
conventional politeness; it is a rule with a test: a malformed projected value
must never be able to fail a mint before the authority checks have run, so the
projection value is built at the point of recording, after `keys.Create`, never
before the unit of work opens. The record's failure rolls the whole transaction
back — authority write included — so there is no state in which the Control
Plane decided something its own log does not say.

The counter advance, the log entry and the mirror row join that same
transaction, which makes the producer's crash story one line: **a crash
mid-transaction is a decision that was never made.** Nothing is delivered,
because nothing was durably decided.

The recorder enforces that premise instead of trusting each caller to know
it: both Record methods refuse a context that carries no unit of work. A
pool-backed call would commit the counter, the log entry and the mirror row
as three autocommits — exactly the interleaving that breaks gaplessness and
log/mirror atomicity — so the persistence port declares `InUnitOfWork` and
the recorder refuses rather than silently degrading.

Revocation is the one case whose projected value cannot be built from the
write at hand. The ownership record is digest-free (ADR 0006 §8) and the
digest exists in the control database only inside the projection pipeline's
own tables — the change log's payload and the mirror row cut from it — so
`RevokeAPIKey` reads
`control.projection_api_keys` **inside the same transaction**, builds the
revoked full state from the digest it finds, and records it. A key with no
mirror row is a key minted before the projection foundation existed: the
migration backfills accounts deliberately and keys deliberately not, because
their digests have never existed in this lane. The recovery for such a key is
the one §8 already named — revoke commits the ownership change, nothing is
projected, and re-mint is the path to a working credential.

The backfill itself carries one rule of its own: it takes a `SHARE` lock on
`control.accounts` for its duration. The migration runner's implicit
transaction holds one snapshot per statement, so an identity write landing
between the backfill's statements could seed the counter with a revision no
log entry names — a wedged projection the three-case rule reads as permanent,
not healable. The lock makes the backfill's statements one consistent read
while leaving readers untouched.

### 3. Order is a revision; identity is five names

The revision is the only ordering key anywhere in the protocol.
`recorded_at` is carried for operators and is never compared against any
clock. Five distinct names are kept distinct, because conflating any two of
them is how projection systems grow race conditions:

| Name              | What it is                                                  | Where it lives                                    |
| ----------------- | ----------------------------------------------------------- | ------------------------------------------------- |
| Resource identity | which row: `key_id`, `account_id`                           | both planes                                       |
| Resource version  | the revision at which this row's current state was recorded | `source_revision` on the mirror rows, both planes |
| Projection batch  | one delivery: `from_revision` plus its contiguous entries   | the wire, transitively                            |
| Consumer position | the highest revision whose effects are committed            | `dataplane.projection_state`, one singleton       |
| Producer head     | the highest revision ever allocated                         | `control.projection_revision.last_revision`       |

A per-row `source_revision` guard rides every incremental apply: a redelivered
entry for a row the mirror has already moved past updates nothing, even inside
a batch that is otherwise fresh.

The revision's ceiling is grammar, not storage arithmetic. The wire carries
unsigned integers, but every revision eventually rests in a `bigint` column on
both planes, so the protocol pins the highest revision at 2⁶³ − 1
(`projection.MaxRevision`) and states it in the contract's `maximum` fields: a
revision above the ceiling is refused as a malformed message at every hop — a
400 the producer surfaces every cycle — rather than as a storage overflow
discovered by one database after another hop already accepted it.

### 4. Bootstrap is a snapshot, and the snapshot's filter is exact

A consumer that has never applied a snapshot (`bootstrapped = false`) is
offered the whole projection: every key and account row, cut at the producer's
head revision. Applying it is **unconditional** and **whole** — a snapshot
means "be exactly this state at this boundary"; it consults no per-row
history, upserts every row it names, and deletes every mirror row it does
not, so a re-snapshot onto a mirror holding rows this timeline no longer
projects leaves nothing behind. It sets the consumer's position to the
boundary **in the same transaction** as the rows it carries.

The cut is exact rather than approximate because the mirror rows carry their
`source_revision`: the snapshot filters `source_revision <= head`, where `head`
is the revision this cycle read before cutting. Under READ COMMITTED a row
recorded after that read is excluded by the filter — and its change remains in
the log past the boundary, so the drain delivers it. No row is lost by the
filter and none appears twice: the boundary names a real instant in the log's
order, which is the whole reason the counter, the log and the mirror live in
one transaction (§2).

The producer may re-snapshot at any time, and three situations call for it:
a wiped consumer, a dead timeline (§6), and a position that has run past the
head — which is what a Control Plane database restored to an earlier instant
looks like from the consumer's side. Zero is a legitimate boundary: an empty
projection is a snapshot too.

### 5. Incremental delivery: the three-case rule

After bootstrap, the log is drained in batches of at most 200 contiguous
revisions. The consumer judges each batch against its own stored position —
never against the batch's `from_revision`, which names only what the producer
believed — and answers with exactly one of three outcomes:

1. **`first == position + 1`** → the batch joins: apply every entry under its
   per-row guard, advance the position to the batch's last revision, commit
   rows and position in one transaction.
2. **`last <= position`** → the batch is wholly behind: a lost acknowledgement,
   not an error. Answer with the unchanged position and do no work. This is
   the case that makes at-least-once delivery free.
3. **Anything else** — a batch that straddles the position or overshoots it →
   refuse **whole** with `revision_gap` (HTTP 409). No partial application,
   ever: applying the entries that happen to fit would strand the rest behind
   a position that has moved past them.

Terminality rides on top of the three cases. `revoked` and `closed` are
terminal states on both planes, and an incremental entry that would move a
row standing at one back to a live state refuses the **whole batch** with
`invalid_request` (HTTP 400): the consumer reads the applied row's state
inside the apply transaction and refuses before the upsert. A conforming
producer cannot express such an entry — the log replays the authority's own
decisions in order, and the authority never un-revokes — so an entry that
tries is not from this timeline's producer, and applying it would admit a
credential the Control Plane revoked. The refusal deliberately does **not**
trigger the heal: a snapshot would overwrite the terminal row the refusal
exists to protect. The drain wedges on it every cycle, which is the point —
the loudest possible signal that the channel is delivering something the
protocol cannot say. The snapshot path is exempt: applying the authority's
current word entire, terminal rows included, is what "be exactly this state
at this boundary" (§4) means.

A gap is never waited for. There is no buffer, no "hold the batch until
revision N shows up" — an unbounded wait on a revision that an aborted
transaction (§1) proved will never be re-issued. The gap's only answer is a
re-snapshot, and in this build the producer asks for it itself: a
`revision_gap` or `snapshot_required` refusal during a drain is answered by a
snapshot **in the same cycle**, and the drain resumes from the boundary that
snapshot committed.

### 6. The epoch: naming the timeline

The counter's `epoch` UUID travels on every message and every position. A
Control Plane database restored from an earlier backup rewinds its counter
without changing a single value the consumer holds; without a timeline
identity, the consumer's position would silently name revisions that will
never be re-issued, and every credential change after the restore would be
skipped forever. With it, the mismatch is a data point: any position whose
epoch does not match the head's does not join this timeline, and the answer is
a re-snapshot (§4), not a guess. An unbootstrapped consumer reports an empty
epoch and a zero revision, which are data, not defects.

One property of the epoch follows from where it is stored and must be stated
as an operator step: **the epoch is part of the database the restore puts
back, so a restore alone does not mint a new one** — the restored row carries
the same `epoch` the pre-restore timeline used, and the rewound counter would
run silently under the old name. Re-minting it is therefore part of the
restore procedure itself, not an optional cleanup:

```sql
UPDATE control.projection_revision
SET epoch = gen_random_uuid()
WHERE id = 1;
```

Run once, after the restore commits and before the producer resumes. Every
consumer's position then fails the epoch match on the producer's next cycle
and is re-snapshotted onto the new timeline whole — the pair's answer to a
rewind, executed rather than hoped for. A deployment that restores and skips
this step has the one failure mode the epoch cannot see: same name, earlier
world.

### 7. Crash safety, both sides

- **Producer**: authority write + log entry + mirror row + counter advance are
  one transaction (§2). Crash before commit → the change does not exist. The
  reconciliation loop holds no state of its own — no buffer, no cursor, no
  learned position — so a crash or redeploy recovers by asking both databases
  where things stand.
- **Consumer**: rows + position are one transaction (§5). Crash before commit
  → the position is unchanged, the same batch is delivered again, the apply is
  a no-op. Crash after commit → the next delivery is wholly behind and
  answered without work. There is no state in which rows are applied and the
  position does not say so, and that property is what makes every other rule's
  recovery story safe.

The history is durable and consumption-blind: nothing trims
`projection_changes` when a consumer catches up, so a consumer down for a week
drains a week of history on return. Retention — how old a delivered revision
may become before it is archived — is a named open property, deferred until
there is data to size it against. Whichever policy eventually trims the log
carries one obligation with it: a consumer whose position sits behind the
oldest retained revision can no longer drain what was trimmed, so the
producer must answer `oldest_retained > position + 1` with a snapshot, not a
batch — the gap rule of §5 extended to a gap the log itself made.

### 8. Transport: the existing chain, the façade contracted, the listener private

Delivery rides the one cross-plane chain ADR 0006 §9 fixed:
`console-api application → ports/outbound/dataplane → HTTP adapter →
dataplane-api → outbound port → HTTP adapter → the Data Plane's private
management listener`. Three operations: read the position, deliver a snapshot,
deliver a batch. The façade's half is contracted —
`api/openapi/shared/projection.yaml` declares the schemas, `dataplane.yaml`
the operations — because the façade is where an outside caller arrives. The
listener half is the private protocol, pinned by a protocol test on each side
and deliberately not a fourth OpenAPI document (AGENTS.md rule 2).

The façade forwards the message body **byte for byte**: the projection grammar
is built and validated at the producer, and a transport that re-encoded it
would be a second grammar — and one that would strip exactly the unknown
additive fields the version rule promises to carry. What the façade decides is
the failure: a private refusal is classified at the façade into its own
vocabulary and its own status, and no byte of the listener's body reaches the
Control Plane caller beyond the classified answer (ADR 0006 §9).

### 9. Versioning and evolution

`protocol_version` is `1` and **every hop fails closed** against a version it
does not know: the message is refused whole, the position and the rows are
untouched, and the producer's loop surfaces the refusal every cycle until the
fleet agrees. Within a version, unknown additive fields are tolerated and
ignored by both ends — and nothing ordering- or state-bearing may arrive that
way. A new resource kind, a new state value or a new required field is a new
protocol version and a reviewed migration of every hop, **consumer first**,
so an upgraded producer meets consumers that refuse it loudly instead of
consumers that accept it brokenly.

### 10. The closed failure vocabularies

Two small, closed sets cover everything that can go wrong, and neither
carries a DSN, a credential, a SQL fragment or a peer's message text:

- **Wire refusals** (the listener's, relayed by the façade as envelope codes):
  `unsupported_version`, `invalid_request` (a grammar violation),
  `revision_gap`, `snapshot_required`. The status codes are the façade's
  translation; the codes are the protocol's.
- **Producer sentinels** (console-api's outbound port):
  `ErrProjectionUnavailable` (unknown answer — transport down, status the
  protocol does not name), `ErrProjectionUnsupportedVersion`,
  `ErrProjectionShape`, `ErrProjectionGap`, `ErrProjectionSnapshotRequired`.

The producer's loop acts on exactly two of them by re-snapshotting
(`Gap`, `SnapshotRequired`) and fails the cycle on the rest — but the two
"this build is wrong" sentinels (`UnsupportedVersion`, `Shape`) are
distinguished by being wrong in a way no retry fixes, so they are surfaced
every cycle for an operator and **never skipped past**. An unavailable peer
costs one log line and a retry at the next interval; that is the bounded
staleness §8 of ADR 0006 promised, now a named default (5 s interval, 30 s
per-cycle deadline, both configurable).

### 11. Secrets

The plaintext exists only inside the mint call and is shown once to the user.
What projects is the SHA-256 digest — 64 lowercase hex characters, validated
by the domain grammar, enforced by database CHECKs on both stored copies, and
carried by no other table in either plane's schema. The three transport hops
keep the service-credential rules ADR 0006 §9 already fixed: one credential
per hop, the hop's two ends reading their own variable, and the projection
consumer's credential never shared with either other hop. Where two of those
variables meet in one process — the façade reads its caller-facing credential
and its listener-facing credential together — their difference is a startup
check, not a documentation sentence: a deployment that set both to one string
is refused at boot, with both variable names in the error. The pairing no
single process sees — the producer's outbound credential against the
listener's own token — is the one boot cannot check, because its two ends
never meet in one process; it rests on the deploy minting three distinct
credentials.
is refused at boot, with both variable names in the error. The producer's
errors are built from status codes and sentinels — a `*url.Error` is dropped
rather than wrapped, because it quotes the full request URL, and the
listener's body is reduced to its `error.code` under a size limit before
anything reads it.

## Consequences

- A mint or revoke now writes four things in one transaction: the authority
  row, a log entry, a mirror row, and the counter. The console's write path
  grew one internal dependency — on its **own** database — and none on the
  Data Plane's availability, which is the property §4 of ADR 0006 required.
- Revocation staleness is now the producer's interval plus one failed cycle,
  by construction, instead of an aspiration. The defaults make the bound
  `≈ 5–10 s` and an operator can read the current lag as the head minus the
  position.
- The Data Plane's mirror tables are the only credentials its runtime will
  ever authenticate against (B8), and the Control Plane's projection tables
  are the only place the digest exists on the Control side. Both facts are
  enforced by tests, not comments.
- Two schemas gained tables (four in the control database counting the
  counter, three in the Data Plane's counting the position singleton); both
  migrations carry their own down files and their own backfill story — the
  accounts backfilled, the keys deliberately not (§2).
- The reconciliation loop is a new background goroutine in console-api with a
  log line per failed cycle. It is deliberately not a crash: a mirror a few
  seconds stale is the design working, not an incident.
- Snapshot bounds (5000 keys, 5000 accounts) and batch bound (200) are
  contract constants pinned by tests on both ends. Past them, bootstrap fails
  by design until the bound is raised — a reviewed contract change, not a
  deployment surprise.
- Catalog configuration, quota grants and entitlements do **not** ride this
  pipeline yet. The log's `resource_kind` set is closed at two values on
  purpose: adding a third is a protocol version, reviewed as one.

## Alternatives considered

- **A broker (Kafka/NATS/RabbitMQ)**: rejected. It would buy durability the
  log already provides and pay a fourth deployable, a second operational
  surface, and a coupling of the planes' release cadence for it. The task
  forbids it; the architecture would refuse it anyway (ADR 0006 §2's release
  cadence argument, pointed at infrastructure this time).
- **Synchronous delivery from the mint**: rejected — it puts the Data Plane's
  availability back inside the console's write path (§4 of ADR 0006), and it
  converts every delivery failure into an authority failure.
- **Deltas instead of full state**: rejected — a delta replayed twice is a
  delta applied twice; idempotency would need per-entry sequence bookkeeping
  at the consumer to reconstruct what full state gives for free.
- **Wall-clock ordering (`occurred_at`, `recorded_at`)**: rejected — clocks
  step, skew and restore; a revision is monotonic because a transaction made
  it so, not because an operator calibrated something.
- **Consumer-side gap buffering**: rejected — waiting for a missing revision
  is an unbounded stall with no signal, against a revision the §1 counter
  proves may never exist. Refuse-and-resnapshot is bounded and loud.
- **The Data Plane reading the Control Plane's database directly**: rejected —
  it inverts ADR 0006 §7's ownership rule and makes the runtime's admission
  path depend on a foreign schema it must not know.
- **Backfilling the pre-projection keys' digests**: rejected — the digests do
  not exist in this lane and cannot be derived; the honest recovery is
  revoke-and-re-mint (§2), which §8 of ADR 0006 already named as the recovery
  for a lost secret.
