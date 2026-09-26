# ADR 0010: Usage fact ingestion and settlement — the pull consumer, the kind-classed ledger, and the page that commits whole

- Status: Accepted
- Date: 2026-09-26
- Issue: [#96](https://github.com/ecoma-io/llm-gateway/issues/96)

## Context

ADR 0006 §5 designed the Data → Control seam as a durable pull with replay,
and left both of its tables as futures. B7 landed the source side —
`usage_events`, commit-ordered by the stream's `append_seq`, a position
encoding only the Data Plane can name. B11 landed the contract the facts
travel under — typed settlement columns, a versioned payload envelope, the
allocation tail that names where money was held. What remained was the
consumer: the code that turns a redeliverable feed of facts into money moved
exactly once, in a database the fact-writer never touches.

Landing it raised the questions a design names but does not answer until the
code has to:

- **What is the exactly-once key?** The feed is at-least-once, so a replayed
  page is a certainty and not an error path. The obvious answers are each
  wrong in a way only the kind structure exposes: `request_id` alone, and the
  feed's own `append_seq`.
- **Who prices, and from what?** The runtime priced the fact at its close and
  wrote the amount into it. A consumer could copy that figure, re-quote a
  price of its own, or re-derive — and each choice says something different
  about which plane money answers to.
- **What does a fact the consumer cannot interpret mean?** A feed carries
  whatever its writer legal writes, including kinds this build has never
  heard of. The answers available — skip it, crash on it, record it — are
  not variants of one another; they decide whether a poison fact becomes
  silent loss, an outage, or a record.
- **What is atomic?** The fact, the page, the cursor advance: the
  combinations have different crash stories, and only one of them leaves no
  state that a replay cannot explain.
- **Who schedules the work?** ADR 0006 decided the pull but not the puller.

## Decision

The consumer is B12's: the replay use case (`application.FactIngestion`) and
the applier it drives (`application.FactApplier`), scheduled by a loop in
`cmd/console-api`, with three tables of its own
(`migrations/control/000008_fact_ingestion`): the applied record, the
quarantine, and the position.

### The consumer is a pull loop, and the position is a Control-Plane row

`cmd/console-api` runs one replay pass per interval —
`CONSOLE_API_INGESTION_INTERVAL`, five seconds by default — each pass under
its own deadline, the first immediately. A pass that reads a page announcing
`has_more` keeps reading under that same deadline — the deadline, not the
interval, is what bounds how far a burst may lag — and the interval times only
the idle feed. The cadence is a freshness dial and
nothing else: the feed is durable, replay is the delivery model, so a failed
pass is one log line and the next pass re-reads the same page. Two failures
are not the loop's to solve: a position the Data Plane can no longer replay
(`cursor_expired`, HTTP 410) gets its own line and holds the feed until an
operator resolves it, and a context cancellation is the stop signal, not a
failure.

The position lives in `control.ingestion_cursor` — a singleton row holding
one opaque string, stored verbatim from the page's `next_cursor`, parsed by
nothing on this side of the seam, revealed to no one. It is read outside any
transaction and advanced only inside the one that did the work, and the
advance refuses to run autocommitted: the adapter enforces the ordering the
crash story depends on, rather than trusting caller discipline. The advance
itself is a compare-and-set on the position the pass read, so a pass that
arrives to move a position no longer held is refused and its unit of work
rolls back with it — the position cannot slip underneath a page unnoticed,
and the loser re-applies idempotently on its next pass.

### The exactly-once key is (request_id, kind class) — not request_id, not append_seq

The fact kinds split into two **idempotency classes**: `settled`,
`released` and `expired` share the settlement class — they are alternative
endings of one reservation's money story, and a request ends in exactly one
of them — while `unbillable_orphaned` stands alone, because the orphan tail
is a different record about a different money story: a completion whose
commitment the close did not earn, written _beside_ the fact that settled the
request, not against it.

- **`request_id` alone is rejected** because it would force the settlement
  and the orphan tail to compete for one slot — the coexistence is the
  point — and because it would bind a future correction kind to the record
  it corrects before that kind has a grammar.
- **`append_seq` is rejected** because it is the feed's order, not the
  ledger's: keying effects on it would make the Control Plane's dedup stand
  or fall with a position it also stores, and a lost cursor would turn every
  replayed fact into a second application. The applied record answers
  "have I already applied this fact" in the Control Plane's own vocabulary,
  from its own table.

What the key calls a replay is decided by kind, not by sequence. A redelivery
of a kind the applied record already carries — appended later by the runtime,
or replayed after a crash; indistinguishable on this wire — is the same
logical outcome and a no-op, whatever `append_seq` it rides, because the seq
identifies the wire row, not the fact. A _different_ kind claiming a class
the record has closed is another thing altogether: two terminal states for
one request's money story, which no delivery order can make true. That
cross-claim is quarantined as incoherent rather than swallowed as a replay,
and the orphan class stays open beside a settled record for the same reason
the classes are separate keys.

- **The key is the primary key**, not a check that precedes an insert. The
  applier reads before it writes — replay is answered by `Find` and no-ops —
  but two consumers racing one fact are settled by the constraint itself:
  exactly one commit lands, the loser's unit of work is refused, and the
  next pass finds the winner's row. No lock spans the check, because the
  check is not what makes it safe.

### A settlement is verified by re-derivation, never copied

The settled amount the consumer books is the formula's, not the fact's word:
`accounting.SettledAmount` re-derives `ceil((input·p_in + output·p_out) /
1_000_000)` over the fact's own counts and captured unit prices — one
ceiling over the whole sum, in checked 128-bit arithmetic — and the
interpretation refuses a fact whose stated `settled_amount` disagrees with
it. The runtime binds the figure at write time; the consumer re-derives it
at read time; a disagreement is not a number to book but a fact to quarantine.
The captured pricing identity (revision id and both unit prices) travels with
the fact and is settled as captured: the consumer never looks up, re-quotes
or revises a price, because the settlement of record is a derivation from
evidence, not a fresh opinion.

The derived effect books through B6's primitives — `Settle` for a settled
fact, the release path for `released`/`expired` — never through a second
implementation of the ledger move. A settled fact's hold legs are booked
first, from the payload's allocation tail in its stored waterfall order,
because this plane's hold rows are derived from the fact and the fact is the
first thing to carry the tail; the settle then consumes greedy down the
waterfall and releases each tail. The settlement row, its legs, the bucket
moves and the applied record are one unit of work. A finalized settlement is
immutable; a replayed fact finds its applied record and moves nothing. One
divergence refuses to apply at all: a settle that returns converged — the
ledger already holds the settlement the derivation would book — with no
applied record on file. Neither the fact's word nor a no-op can explain that
state, so it is a state of the plane, not a disposition of the fact: the page
stops and the pass fails loudly until someone reconciles the books.

### The disposition split: six refusals quarantine, everything else stops the page

A fact this build cannot apply is one of two things, and the answer differs
by what the refusal is _about_:

- **A refusal about the fact** — unknown kind, unknown `schema_version`,
  columns off the feed grammar, a payload that is not the versioned envelope,
  a correction kind this build does not implement, figures that do not
  cohere — is **quarantined**: recorded verbatim in `control.quarantined_facts`
  (kind, version, payload, price revision, provider tokens, and the refusal's
  own words), after which the page advances. The quarantine's bounds bound
  what a _record_ may hold, never what the feed may carry: identity strings
  longer or stranger than the evidence columns are clamped to those bounds at
  the record's edge — rune-safe, heads preserved — so even a fact built to
  defeat its own recording is recorded.
  These are facts the feed may legitimately carry and this build cannot
  interpret; skipping one silently would lose the only evidence that it ever
  existed, and stopping the feed for one would hand a single poison fact a
  veto over every fact behind it. The quarantine is the third answer: an
  operator-visible record, kept whole, that loses nothing and blocks nothing.
- **A refusal about the plane** — a payload too large for a quarantine to
  record verbatim, a store failure, an accounting refusal (a bucket the fact
  names that does not exist, a settlement conflict, a movement that does not
  converge) — **stops the page**: the unit of work rolls back whole, the
  position stays, the pass fails as one log line, and the next pass re-reads
  the same page. An oversize payload stops rather than truncates because a
  truncated copy of the evidence is not a record — record-versus-apply
  becomes record-versus-lose, and losing is the one thing the quarantine
  exists to prevent. The accounting refusals stop because they are states of
  the plane, never dispositions of a fact: quarantining one would file a
  plane's defect under the fact that tripped over it.

Nothing is skipped-and-advanced, ever. The cursor moves only inside the
transaction that applied or quarantined every fact on the page, so a replay
after a crash at any point re-reads the page the crash interrupted, and the
applier's idempotency makes the facts that did land free to deliver again. A
page that stops names its cause in a log line every interval until someone
resolves it — a permanent block is possible, an _unexplained_ one is not.

### What the consumer refuses to trust

The applier's inputs are the fact's typed columns and its allocation tail,
and nothing else. It cannot mint an arbitrary ledger entry: every leg names
a bucket the tail carries, every amount is the formula's or the tail's, and a
fact naming a bucket that does not exist stops the page rather than fabricating
a row. The fact arrives only over the management façade's authenticated
channel, carrying the process's own credential; no credential, API key or
secret travels into an applied row, a quarantine row or a settlement, and the
applier's errors are built from sentinels and ids, never from request bytes.

#### What the consumer trusts the writer to have bound

One property the fact carries cannot be re-derived on this side of the seam:
that the allocation tail's buckets belong to the account the request was
admitted under. The envelope names no account, and the control database holds
no request→account mapping — that binding lives where admission lives, in the
Data Plane. The consumer therefore trusts the writer's binding where it
refuses to trust the writer's arithmetic, and bounds that trust the way it
bounds every other one: the fact arrives only over the authenticated
management channel, and every effect books against buckets the tail itself
names — so the worst a mis-bound fact can do is move money between buckets
the writer named, and the applied record, the quarantine and the ledger keep
the forensic trail an operator needs to find it. Closing the gap in-boundary
— account identity on the envelope, validated against bucket ownership — is
filed as [#110](https://github.com/ecoma-io/llm-gateway/issues/110); the
writer-side amount↔tail pairing gap the consumer's refusals surfaced is
[#111](https://github.com/ecoma-io/llm-gateway/issues/111).

## Consequences

- Duplicate delivery produces one logical and one financial effect, on the
  primary key's word, with no lock and no cross-plane acknowledgement — the
  failure model's fourth line, now with a table behind it.
- The ledger's lag behind the feed is bounded by one interval plus one pass,
  and a pass drains a `has_more` burst under its deadline rather than moving
  one page per interval. That bound is the named, bounded property of
  ADR 0006 — the window B13's reconciliation exists to converge. The consumer
  is not that worker and implements none of it.
- The quarantine is an operator surface with no operator yet: rows
  accumulate until someone looks, and nothing ages them out — retention is
  forever for the same reason the ledger's is.
- Correction kinds are recorded-and-refused in this build
  (`ErrCorrectionUnsupported`): landing them is a `schema_version` bump and a
  new interpretation path, not a change to the page contract or the
  disposition split.
- What remains of
  [issue #63](https://github.com/ecoma-io/llm-gateway/issues/63) is the
  accepted deferral it arrived with — one settlement currency, provenance
  beyond the revision id — tracked rather than blocking.

## Alternatives considered

- **Dedup by `request_id` alone.** One applied record per request reads
  cleaner until the first kind pair that must coexist: the orphan tail beside
  the settlement, then corrections. The class is the smallest key that keeps
  those pairs from competing, and it is already the shape the kinds themselves
  take.
- **Dedup by `append_seq`.** Cheapest to check and the wrong owner: it
  dedups the feed against itself, so every effect's safety rests on a
  position that a restore, a re-seed or a lost row invalidates. The applied
  table costs one row per fact and answers the question in the plane that
  owns the effects.
- **Per-fact transactions with per-fact cursor advances.** Finer-grained
  crash recovery, at the price of a page that can end half-applied with its
  position moved — every intermediate state is then a state a replay must be
  taught to explain. Whole-page atomicity has one story — the page committed
  or it did not — and the page is already the wire's unit.
- **Skip-and-advance on uninterpretable facts.** The feed never blocks and
  the evidence never existed. Rejected without a second reader: silent loss
  is the one outcome the contract's vocabulary exists to make impossible.
- **Dead-letter queues for refusals.** The quarantine _is_ the dead letter,
  in the plane that owns the effects, written in the same transaction as the
  page's effects — a separate queue would reintroduce the second system whose
  delivery, retention and observability would each need their own answer.
