# ADR 0011: The reaper and the reconciliation pass — a second door into an ending, and a second look at what the Control Plane derived

- Status: Accepted
- Date: 2026-09-26
- Issue: [#114](https://github.com/ecoma-io/llm-gateway/issues/114)

## Context

Two loops are missing from a system that otherwise closes every book it opens,
and they are missing for the same reason: every ending so far is written by the
process that performed it.

A request is admitted, routed, settled or released by the runtime that served
it. If that process dies between the moment it opens a hold and the moment it
closes it, the hold stays open, its capacity stays drawn, and its replay record
has no answer to give the caller who asks again. The reservation sweep has
existed since B7 — the statement, the both-clocks-lapsed predicate, the
`FOR UPDATE SKIP LOCKED` claim, the rows that come back whole. What has never
existed is the driver.

The second gap is the one ADR 0010's consequences section named as its own
successor: the ledger's lag behind the fact feed is bounded by one interval
plus one pass, and that bounded lag is exactly the window a reconciliation
worker exists to converge. The Control Plane holds effects derived from facts
across a plane boundary, by a pull, with replay — a design whose correctness
argument is about delivery, not about agreement. Nothing checks whether the
things it derived still agree with each other.

B13 adds both loops. Landing them raised the questions the design does not
answer until the code has to:

- **Does the reaper end a request, or clean up after one?** A closed hold with
  no fact behind it is the orphan the whole discipline exists to make
  impossible, and a returned leg for a request that is still executing is a
  customer charged for nothing. The two errors are symmetric and the sweep
  cannot tell them apart from a row alone.
- **What may reconciliation do about a divergence it finds?** A
  bucket's cached balances disagreeing with its legs is computable. So is a
  settlement's header disagreeing with the legs written beside it. Whether
  either is _correctable_ is a different question, and answering it wrongly
  turns a detection pass into a second accounting engine.
- **What is a pass's unit of work?** Full-table on every tick, or a window
  that advances? The scale answer is a cursor, and a cursor has to be resumable
  across a restart without skipping the rows a previous pass had not reached.
- **What does a duplicate mean when the reaper races the ending that got
  there first?** Both doors write the same request's fact and both close the
  same hold. The loser must not unwind the half that only it did.

## Decision

The reaper is `application.Reaper` in the Data Plane, driven by a loop in
`cmd/dataplane`; reconciliation is `application.Reconciliation` in the Control
Plane, driven by a loop in `cmd/console-api`, over two new tables
(`migrations/control/000009_reconciliation`).

### The reaper is a second door into an ending, not a cleanup pass

`Reap` drains the existing sweep, one victim's unit of work at a time: the
exclusive claim, the capacity's return, the request's failure, the replay
record's pointer, and the fact **last** — the same order the routing stage's
release writes, because the two doors are the same door opened twice.

- **Both clocks must lapse, and the predicate is the store's.** `expires_at`
  guards against holding capacity for a caller who has walked away;
  `lease_expires_at` guards against taking on a hold whose request is still
  executing. Neither alone is safe, and both are compared against
  `clock_timestamp()` — advancing, not frozen — so a hold that lapsed a
  microsecond ago is already a victim. The three boundary cases (just before,
  exactly at, just after) are tested against the engine rather than against a
  fake's clock.
- **The fact is last, and the append is one statement.** The sequence
  allocation lives in the same statement as the insert, and the dedup guard
  lives _inside_ that allocation. Both placements are load-bearing and both
  were found by a test rather than by reading: a data-modifying CTE runs to
  completion whether or not the main statement consumes its output, so a
  `WHERE NOT EXISTS` guard on the outer INSERT filters the row away while the
  CTE's increment commits anyway — a gap in `append_seq` that no consumer can
  distinguish from a lost fact. The guarded shape writes nothing on the
  refusal path, and the regression test asserts the feed's two ends agree.
- **A duplicate is a commit, not a rollback.** When the twin already wrote the
  fact, this unit's hold is closed and its capacity returned _inside_ the same
  transaction. Rolling back would un-return a leg for no end, so the sentinel
  is swallowed and the skip is counted. The same holds for a finalise that lost
  its compare-and-set.
- **A failure that is not a duplicate aborts the unit.** A closed hold with no
  fact behind it is the orphan, so the hold reopens and the next cycle is where
  it is picked up.

### Reconciliation detects and records; it never repairs

The pass sweeps a window of the applied-facts ledger and answers six questions
about what it finds. It writes findings and run rows. It moves no money, and
that is structural rather than a promise: the ports it holds are two-read
interfaces over the ledger, not the accounting use cases, so `Adjust` and
`Settle` are not one call away.

- **A missing settlement is a finding, never a silent "settled".** The check
  that finds one is F4, and it reads the settlement by the id the fact names.
- **A settlement without accounting is a finding, not a repair.** The
  invariant is checked against the settlement's own legs — the consume sum,
  the leg count, the per-kind multiset — and a disagreement opens a finding.
  The repair path, when the six auto-repair preconditions are all met, goes
  through the canonical B6 application port and is not written by this pass.
- **An amount mismatch is a finding, and neither record is overwritten.** A
  finalized settlement is immutable (ADR 0004); a correction is a new
  adjustment leg naming the entry it corrects, and this pass never writes one.
- **The current price never judges an old settlement.** The comparison is
  between what the settlement's legs say and what its header says — both
  frozen at the time it was written.

### The pass's unit of work is a window, and the window's cursor is a pair

The pass opens `[window_from, window_to)` where `window_from` is the previous
pass's `window_to` and `window_to` is the pass's own read of the store's
clock. A full lookback on the first pass after a cold start; every interval
thereafter.

- **Paging is a keyset on `(applied_at, request_id)`, never on the instant
  alone.** A whole ingestion page commits in one transaction, so a hundred
  facts share one `applied_at` to the microsecond; a keyset on the instant
  resumes past ninety-nine of them and the pass reports a clean window for
  rows it never read. The port says this and the read obeys it, and the
  regression test forms a real five-row tie through the port to hold it.
- **The window bounds are half-open** — inclusive at the floor, exclusive at
  the ceiling — so two adjacent passes see each row exactly once.
- **`started_at` is the engine's clock, not a window bound.** A first draft
  bound it to `window_from`, which is the _previous_ pass's end, and the
  consequence was a `running` row that made a wedged worker legible as
  exactly the opposite. The migration defaults it for the reason 000008
  recorded: the worker's own lifecycle instants are the engine's facts about
  its rows.
- **The latest run orders by id, never by finished_at.** A pass that died has
  no `finished_at` and is still where the sweep stopped; ordering by the
  completed one would re-sweep a window the crashed pass had half-covered.

### A finding is durable, keyed by identity, and never a message

`reconciliation_findings` is a table, not a log line. Its key is
`(check_kind, subject_kind, subject_id)` under a **partial** unique index over
`status = 'open'`, so a re-run over unchanged data converges on the row it
already holds and advances its `last_seen_at` without rewriting the evidence
that opened it. Resolution frees the key, so a recurrence is a new finding
rather than a re-opened one with stale evidence attached.

Duplicates are found by canonical identity — `usage_event_id`, `settlement_id`,
`accounting_reference`, `request_id` — never by counting rows, because a count
cannot distinguish "the same event twice" from "two events".

### A pass claims its window, and the claim is a row rather than a lock

Two replicas of the control plane read the same high-water mark, so they
compute the same window. Nothing in an id sequence arbitrates between them: both
open a run row, both sweep, and both write converging findings — no money at
risk, since the pass moves none — while **neither advances the mark**, so the
plane re-sweeps that one window for ever and never covers a window after it. A
pass that had stopped covering new ground would look exactly like a healthy one,
which is what makes this the one concurrency defect in the design that is silent
rather than loud.

The window is therefore claimed while the pass holds it, by a **partial unique
index on `(scope, window_from)` over running rows**. The claim is held by the
row, so it ends when the row does: a process that dies mid-pass leaves a
`running` row and a claimed window — the one outcome an operator has to look at —
rather than a lock that outlives its holder and blocks every pass behind it.
Finishing releases the key, because the release is the index's own predicate.

The refusal arrives as `persistence.ErrWindowClaimed`, and the losing pass
returns an empty summary and no error. That is not leniency: the sibling is
doing the work this pass would have done, and a log line every interval for the
life of a correct deployment is a signal an operator learns to ignore, which is
the worst thing a health signal can become. The refusal is a lifecycle event,
not a fault.

### The schedule is a dial, a backoff, and a spread

- **`DATAPLANE_REAPER_ENABLED`**, `DATAPLANE_REAPER_INTERVAL` (5s),
  `DATAPLANE_REAPER_TIMEOUT` (10s), `DATAPLANE_REAPER_BATCH_SIZE` — the
  reaper's cadence, its per-cycle budget, and the sweep's `LIMIT`.
- **`CONSOLE_API_RECONCILIATION_INTERVAL`** (60s),
  `CONSOLE_API_RECONCILIATION_TIMEOUT` (5m),
  `CONSOLE_API_RECONCILIATION_BATCH`, `CONSOLE_API_RECONCILIATION_LOOKBACK` —
  the pass's cadence, deadline, page size, and the width of the first window
  after a cold start.

The timeout exceeds the interval in both loops, and both loaders refuse a
configuration that breaks it, so a slow pass delays the next one rather than
running two concurrently. The first gap is spread like every other one, so N
processes started by the same deployment do not begin in lockstep.

Neither mechanism below is needed for correctness, which is the point: the
reaper's sweep is `FOR UPDATE SKIP LOCKED` and the pass's findings converge on
their unique key, so the worst a fleet of replicas does is duplicate work.

**A failure widens the gap, and every gap is spread.** Two mechanisms, kept
separate because they answer for different things: the backoff answers for the
**database** — a plane answering every pass slowly or not at all is being asked
more than it can give — and the jitter answers for the **replicas**, since N
processes configured with one interval are N processes that wake at the same
instant. What the two buy is that a plane scaled by replicas and pointed at one
database does not turn a database that is struggling into one that is being
hammered by a thundering herd at the moment it is struggling.

The backoff is a plain doubling with **no error classifier**, and that is a
deliberate refusal rather than an omission. A pass that failed because the
database is gone and one that failed because a single hold's unit kept erroring
are indistinguishable from a worker, and both are answered by waiting longer;
a classifier that separated them would be reading a distinction this loop
cannot make. The ceiling is what makes that acceptable, and it is measured
against the **configured** interval rather than the gap it produced, so it
stays a constant number of passes instead of drifting with every doubling. The
reaper's ceiling is the lower of the two (8 against 16), because its lateness is
not a detection delayed but capacity that stays drawn from a customer's bucket —
a reaper backing off harder than the control plane's is one widening the window
it exists to close.

A **cancellation is not a failure** on either loop. A stop signal is the loop
ending, so backing off for it would only change how long the goroutine sits
before its context returns it.

### A pass closes its run row on a context the signal cannot cancel

The pass writes its run row before the sweep and closes it after, so a pass
that died mid-sweep leaves a `running` row — and this ADR calls that row the
honest record of a worker that started and never finished. That record has to
survive the commonest ending there is, and a graceful shutdown is the
commonest ending: the signal cancels the context the pass carries, and a
`Finish` issued on a cancelled context is refused by the store for the reason it
should be.

Left there, the cost is not coverage — the next pass opens at the stranded row's
`window_to` and sweeps that window regardless — but **reporting**. A `running`
row is the signal this design gives an operator for a wedged worker, so a
control plane that leaves one behind on every ordinary restart has trained its
operator to ignore the one signal that means something. The close therefore runs
on a context derived from the caller's with `context.WithoutCancel`, under a
bounded grace.

Bounded, and not open, because the drain waits on this goroutine: a process that
will not stop is a worse failure than the row it was added to prevent, and the
grace that misses costs a reader one stale row rather than any coverage, since
the next pass re-claims and sweeps that window anyway. The reaper has no
equivalent obligation — a hold it did not close is closed by the next process's
sweep rather than by a row — so it keeps closing on the cancelled context and
loses nothing by it.

## Consequences

- A process that dies mid-request no longer strands capacity indefinitely. The
  cost is a latency bound (interval + timeout) on a caller who asks again
  before it, which is the same bound the sweep's own clock vocabulary
  already imposed on every hold.
- The reaper can now race a live release and a settle, and both races have a
  documented winner: the compare-and-set, then the dedup key. The tolerated
  skips are the two places the losing door still commits, and both are
  exercised against the engine.
- The Control Plane now checks its own work. What it finds is recorded with
  enough structure for an operator to act and none for the pass to act on: a
  reconciliation pass that could correct a divergence would be a second
  accounting engine, and the six auto-repair preconditions are written down so
  a future change has to argue against them rather than against nothing.
- `control.reconciliation_findings` and `control.reconciliation_runs` are
  retained forever. The findings table is bounded by the number of diverging
  subjects rather than by traffic, and pruning a run row would prune the
  high-water mark's history — the one thing that makes a pass's progress
  legible after the fact.
- **A second replica is correct.** Two replicas compute the same window and
  the engine awards it to one of them; the other sweeps nothing and says
  nothing. Coverage depends on no two passes colliding forever — a sibling that
  loses every interval would starve the mark, since the winner's own
  `window_to` is what advances it. That is not a correctness property and is
  bounded by the interval, not by the claim.

## Alternatives considered

- **Reaper auto-refund on cleanup.** Explicitly rejected by the mission
  boundary: runtime recovery is not financial correction. A reclaimed hold
  returns capacity to the bucket it was drawn from, which is a runtime fact;
  whether the customer is refunded for a request that never produced tokens is
  an accounting decision the pass must not make on its own.
- **A reaper that settles.** A reaper that priced and settled would be a
  second settlement engine with a different, worse vocabulary — one built on
  `expires_at` rather than on a reported provider result. The reaper writes
  `expired`, which carries no amount, and reconciliation reads that absence as
  the information it is.
- **Reconciliation repairing through raw SQL.** Every repair available to a
  pass that writes directly to the ledger is a repair the ledger's append-only
  guards were built to make impossible, and a pass that inserts a leg beside a
  settlement has become a second writer of the money tables. The port boundary
  makes the correct path the only path.
- **Full-table reconciliation every tick.** Simple, and it scales inversely to
  success: a plane with a full ledger spends the most on the pass that finds
  the least. The window and its cursor make the cost proportional to the
  change rather than to the history.
- **Keyset on `applied_at` alone.** Simpler, and it silently drops every row
  sharing an instant with the last row of a page — which is every row of an
  ingestion page, because an ingestion page commits whole.
- **An advisory lock around the pass.** It is the standard answer and it needs
  a pinned connection for the whole pass, which this store deliberately does not
  hand out — its rule is that the transaction comes from the context and never
  from the pool. A lock would also outlive a process that dies holding it, which
  is the failure this design can least afford in a worker whose entire job is to
  report honestly about state it does not control. The claim is held by a row
  instead, so it dies with the row and the evidence of a dead pass is the row
  itself.
- **A finding table keyed by a message digest.** Cheap to add, and it makes
  dedup a function of wording. Two passes describing the same divergence in
  slightly different words are the same finding, and a key that cannot say so
  is a table that grows with the number of passes rather than the number of
  problems.
