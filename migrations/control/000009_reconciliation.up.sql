-- Migrations lane: control — the Control Plane's `control` database
-- (ADR 0006 §7). This file lands the reconciliation worker's durable surface:
-- the findings a pass records and the runs that say when a pass happened.
--
-- 000009 — reconciliation:
--
--   B13 is DETECT AND REPORT. This migration is the whole of what it ships
--   durably, and the absence is the design, not an omission: there is no
--   repair here, no adjustment leg, no balance write of any kind, and no
--   column that names an operator to authorise one. Seven adversarial
--   reviews established independently that automatic repair is structurally
--   impossible against the merged accounting foundation rather than merely
--   undesirable, and the two reasons are worth recording here because they
--   are the reasons this table has no write path at all:
--
--     * a correction to an append-only ledger is a NEW adjustment leg, and
--       that leg's schema requires an `operator_id` — a machine principal
--       would be a grammar decision about operators that the founder has not
--       made, and the ledger's own schema comment says the ledger "must not
--       guess at its grammar". This migration does not guess either.
--     * the leg algebra (ledger_entries_leg_algebra) makes an adjustment
--       SINGLE-axis: exactly one of settled_delta / held_delta is non-zero.
--       A full reversal of a consume leg needs BOTH axes, so it is not
--       representable as one adjustment, and the two-adjustment workaround
--       is refused by the same no-credit rule. An over-settlement therefore
--       escalates to a human. It does not auto-repair, and a worker that
--       pretended otherwise would be inventing a credit nobody authorised.
--
--   `reconciliation_runs` is one row per pass: what scope it covered, the
--   half-open window of wall-clock instants the pass swept, when it started
--   and finished, and three counters. The window is half-open by construction
--   (window_from inclusive, window_to exclusive) so two consecutive passes
--   tile the timeline with no overlap and no gap — an inclusive upper bound
--   would make the instant a pass ends at the same row twice, and a
--   recomputed lower bound would skip the band between two windows whenever a
--   pass outran its own interval.
--
--   The high-water mark is the LAST run's window_to, and it is persisted here
--   rather than recomputed as `now() - lookback` on every pass: a recomputed
--   bound is a pass that outruns its own interval re-deriving its own tail
--   forever and never advancing, which is the silent-stall failure this
--   repository's doctrine keeps naming. It is deliberately NOT a separate
--   singleton table: one column on the newest run row is the mark, Latest is
--   its only reader, and a separate table would be a second place to forget
--   to move it.
--
--   `reconciliation_findings` is one row per OPEN divergence. The dedup key
--   is (check_kind, subject_kind, subject_id) and it is enforced as a
--   PARTIAL UNIQUE INDEX over status = 'open'. This is the single most
--   important constraint on this table, and the partial predicate is what
--   makes it the right shape:
--
--     * a re-run that sees the same divergence must CONVERGE, not duplicate.
--       Without the index, every pass would write another row for the same
--       defect and the table would grow without bound while failing its only
--       question — "what is open right now".
--     * resolving or ignoring a finding frees the key, so a genuine later
--       RECURRENCE of the same check on the same subject opens a fresh row
--       with fresh evidence rather than disappearing into a closed one. An
--       unqualified unique index would make the second occurrence
--       unrecordable, which is the wrong answer in the other direction.
--     * the key carries NO timestamp and NO run id, and that is load-bearing:
--       any clock value in the key would make every re-run a distinct row
--       and reintroduce exactly the unbounded growth the index exists to
--       prevent. detected_at, last_seen_at and the run that found it are
--       recorded columns; none of them is part of identity.
--
--   Findings are append-only FOR THEIR IDENTITY, with exactly one sanctioned
--   UPDATE. The trigger below raises unless the only columns that changed are
--   status, resolved_at and last_seen_at — the lifecycle columns, which is
--   what an operator acknowledging or a worker re-seeing a divergence is
--   allowed to move. Everything else (the subject, the severity, the
--   evidence, the detail) is frozen at insert, because a finding whose
--   evidence can be rewritten in place is not evidence. The DELETE guard is
--   unconditional: nothing here is ever removed, and a table that can only
--   grow is a table whose answers stay auditable.
--
--   Why this table has a blanket-append-only guard while its dataplane-lane
--   sibling (request_attempts) deliberately has none: request_attempts has
--   exactly one sanctioned UPDATE too, and it is permitted because the
--   provider reports usage telemetry after a disconnect — a real write path
--   that arrives later. A blanket trigger there would refuse a legitimate
--   write, so the narrow trigger is the only honest shape. Here the sanctioned
--   UPDATE is a lifecycle transition the worker itself performs, and the
--   narrow trigger still refuses everything else; the two tables are the same
--   decision, reached for the same reason.
--
--   Bounds of evidence, not of grammar. The subject columns are the entity a
--   check is about, and a check that cannot record its subject is not a
--   check — so their bounds sit far above anything this plane mints (bucket
--   and settlement ids are uuid v7, 36 characters; request ids are opaque
--   text bounded at 256 by the feed's own grammar) and the adapter clamps
--   rather than refusing. `observed` is the one column left UNBOUNDED, and
--   that is a deliberate asymmetry: it is structured jsonb this plane's own
--   checks write (the cached and derived balances, the leg multiset, the
--   settlement header), never a provider blob, never a raw prompt, never a
--   fact payload. The one place a payload could reach it is the quarantine
--   evidence, and that evidence is the refusal's own `reason` text bounded by
--   the quarantine's reason column — a sentence the consumer already
--   truncated to 512 runes at record time, not the undecoded fact body.
--   A size CHECK on a column whose every writer is this plane's own checks
--   would be a constraint against a future, and this lane's quarantine
--   precedent is explicit that refusing a record is the wrong direction.
--
--   Lane-convention deviations, recorded:
--
--     * 000006 supplies timestamps from the application; this migration
--       defaults started_at, finished_at, detected_at, last_seen_at and
--       resolved_at from the clock, for the reason 000008 recorded — the
--       worker's own lifecycle instants are the engine's facts about the
--       rows, not the caller's claims. window_from / window_to are NOT
--       defaulted: they are the pass's decision about what it swept, and a
--       defaulted window would be a window nobody chose.
--     * unlike 000008, `detected_at` and `last_seen_at` are supplied by the
--       application from the database's clock (the same dbNow read that
--       names the pass's window), so the three lifecycle instants a pass
--       writes agree with each other to the microsecond. The DEFAULTs above
--       remain for a row written by any other hand (verify.sh's probes), and
--       the two paths can never disagree inside one pass because the pass
--       supplies its own.
--     * `reconciliation_runs` is a new table in the control schema, so
--       `id bigint GENERATED ALWAYS AS IDENTITY` is the lane's own precedent
--       (quarantined_facts, 000008) rather than the uuid v7 the identity and
--       accounting foundations use: a run is an infrastructure record with no
--       cross-plane reference and no domain identity, and nothing mints ids
--       for it.

CREATE TABLE control.reconciliation_runs (
    id bigint GENERATED ALWAYS AS IDENTITY,
    scope text NOT NULL,
    started_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    status text NOT NULL DEFAULT 'running',
    window_from timestamptz NOT NULL,
    window_to timestamptz NOT NULL,
    buckets_scanned bigint NOT NULL DEFAULT 0,
    findings_opened bigint NOT NULL DEFAULT 0,
    findings_unchanged bigint NOT NULL DEFAULT 0,

    CONSTRAINT reconciliation_runs_pkey PRIMARY KEY (id),
    -- The scope names what the pass swept, and it is a non-empty bounded
    -- string rather than a foreign key: the vocabulary is this worker's, it
    -- is one value today (the whole lane), and a new value is a code change
    -- that arrives with its own CHECK revision rather than a silent insert.
    CONSTRAINT reconciliation_runs_scope_grammar
        CHECK (char_length(scope) BETWEEN 1 AND 128),
    CONSTRAINT reconciliation_runs_status_valid
        CHECK (status IN ('running', 'completed', 'failed')),
    -- Counters are counts. A negative count is a bug in the worker that
    -- wrote it, and refusing it here is the difference between a loud error
    -- at the statement and a number nobody can read later.
    CONSTRAINT reconciliation_runs_counters_nonnegative CHECK (
        buckets_scanned >= 0
        AND findings_opened >= 0
        AND findings_unchanged >= 0
    ),
    -- The half-open window, stated: a window is empty or positive, never
    -- reversed. The upper bound is NOT the upper bound minus a tick — a
    -- degenerate window of exactly one instant is a legitimate answer from a
    -- pass that outran its own lookback, and it is the caller that decides
    -- whether that is a configuration worth refusing, not the schema.
    CONSTRAINT reconciliation_runs_window_order
        CHECK (window_to > window_from),
    -- A finished pass is a finished pass: status and finished_at are the same
    -- sentence told twice, and a row where they disagree is a worker that
    -- crashed between the two writes. Both directions are pinned — a running
    -- row has no finish, and a finished one has both.
    CONSTRAINT reconciliation_runs_finish_shape CHECK (
        (status = 'running') = (finished_at IS NULL)
    )
);

COMMENT ON TABLE control.reconciliation_runs IS
    'One row per reconciliation pass (B13): the half-open window it swept, when it ran, how it ended, and what it found. Detect-and-report only — no pass in this design moves money, and this table is where that fact is visible. The high-water mark a later pass opens its window from is the newest run''s window_to, read through Latest; a recomputed now()-minus-lookback bound would re-derive its own tail forever whenever a pass outruns its own interval.';
COMMENT ON COLUMN control.reconciliation_runs.scope IS
    'What the pass swept. One value today — the whole lane — recorded rather than inferred so a future partial scope is a row a reader can tell apart from a full pass.';
COMMENT ON COLUMN control.reconciliation_runs.window_from IS
    'Inclusive lower bound of the swept window: the previous pass''s window_to, or the database clock minus the lookback on a first run. Half-open, so the two passes tile the timeline with no overlap and no gap.';
COMMENT ON COLUMN control.reconciliation_runs.window_to IS
    'Exclusive upper bound: the database clock read at the start of this pass, the same instant the bucket sweep and the windowed fact reads are bounded by.';
COMMENT ON COLUMN control.reconciliation_runs.findings_opened IS
    'Divergences this pass opened a finding for. A divergence an open finding already covers is counted as unchanged instead, which is what makes a re-run over unchanged data open zero.';

CREATE TABLE control.reconciliation_findings (
    id bigint GENERATED ALWAYS AS IDENTITY,
    check_kind text NOT NULL,
    subject_kind text NOT NULL,
    subject_id text NOT NULL,
    severity text NOT NULL,
    detected_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    status text NOT NULL DEFAULT 'open',
    resolved_at timestamptz,
    observed jsonb NOT NULL DEFAULT '{}'::jsonb,
    detail text,

    CONSTRAINT reconciliation_findings_pkey PRIMARY KEY (id),
    -- check_kind names the invariant that was violated; subject_kind and
    -- subject_id name the entity it was violated on. Together they are the
    -- finding's canonical identity and the dedup key the partial index below
    -- arbitrates. The bounds are evidence bounds, not a grammar: a check that
    -- cannot name its subject is not a check, and the adapter clamps rather
    -- than refusing (reconciliation_findings_subject_id_evidence).
    CONSTRAINT reconciliation_findings_check_kind_grammar
        CHECK (char_length(check_kind) BETWEEN 1 AND 64),
    CONSTRAINT reconciliation_findings_subject_kind_grammar
        CHECK (char_length(subject_kind) BETWEEN 1 AND 64),
    CONSTRAINT reconciliation_findings_subject_id_evidence
        CHECK (char_length(subject_id) <= 4096),
    CONSTRAINT reconciliation_findings_severity_valid
        CHECK (severity IN ('info', 'warning', 'critical')),
    CONSTRAINT reconciliation_findings_status_valid
        CHECK (status IN ('open', 'acknowledged', 'resolved', 'ignored')),
    -- The same pairing rule the runs table states, on the finding's own
    -- lifecycle: a resolution is a decision that has a time, an
    -- acknowledgement is not (an operator has seen it; nobody has decided),
    -- and an open finding has no resolution at all. Two directions pinned,
    -- because "a resolved finding with no resolved_at" and "an open one with
    -- a resolved_at" are both states a bug in the transition would produce.
    CONSTRAINT reconciliation_findings_resolution_shape CHECK (
        (status IN ('open', 'acknowledged')) = (resolved_at IS NULL)
    ),
    -- A finding is not a hypothesis: it is a row that exists only while an
    -- invariant is false. An absent observation is a finding nobody can act
    -- on, so the column is NOT NULL; the empty object is the one honest
    -- spelling of "the check fired and the evidence is structural" (a
    -- singleton check with one subject and no figures to compare).
    CONSTRAINT reconciliation_findings_observed_is_object
        CHECK (jsonb_typeof(observed) = 'object'),
    -- `detail` is a sentence for the human who resolves the finding, so it is
    -- bounded the way the quarantine's `reason` is: the evidence column
    -- carries the figures, and this is prose that must not become a place
    -- where a payload rides past every other guard.
    CONSTRAINT reconciliation_findings_detail_grammar
        CHECK (detail IS NULL OR char_length(detail) <= 2048)
);

-- The dedup key, and the reason it is partial. See the header: a re-run must
-- converge on the open finding rather than write another row, and a resolution
-- must free the key so a genuine recurrence is recordable. The key carries no
-- timestamp and no run id — detected_at, last_seen_at and the discovering run
-- are columns, never identity, because any clock value in the key would make
-- every pass a fresh row and the table would grow without bound while failing
-- its only question.
CREATE UNIQUE INDEX reconciliation_findings_open_key
    ON control.reconciliation_findings (check_kind, subject_kind, subject_id)
    WHERE status = 'open';

-- The window claim, and it is the reason a second replica is correct rather
-- than merely lucky. A pass reads the high-water mark, opens a window and
-- sweeps it; nothing in the SQL above stops a second pass from reading the
-- SAME mark, opening the SAME window, and sweeping it again — and a sequence
-- is not a mutual-exclusion primitive, so ordering the high-water mark by id
-- does not help. The two passes would find the same divergences (which
-- converge, so no money is at risk) while neither advanced the mark, and the
-- plane would re-sweep that one window for ever and never cover the next.
--
-- So the window is claimed, by the engine, while it is in flight: a partial
-- unique index on (scope, window_from) over running rows. The second pass
-- computing the same window loses the insert and is told so, and a pass that
-- finished or failed releases the key for the next one. The claim is
-- narrower than a lock on purpose — it is held by the ROW, so it ends when the
-- row does, and a process that dies mid-pass cannot strand it: its row stays
-- 'running' and its window stays claimed, which is the one outcome an
-- operator has to look at, rather than a lock that outlives its holder and
-- blocks every pass behind it.
CREATE UNIQUE INDEX reconciliation_runs_window_claim
    ON control.reconciliation_runs (scope, window_from)
    WHERE status = 'running';

-- Two indexes for the questions an operator asks, and neither is the key: how
-- many findings are open (the pass's own headline, and the only number the
-- worker logs), and what is still open for one subject (the shape a resolving
-- operator reads). The partial predicate on the second keeps resolved history
-- out of it: a resolved finding is answered, and only the open set is ever
-- swept for one subject.
CREATE INDEX reconciliation_findings_open_status_idx
    ON control.reconciliation_findings (status);
CREATE INDEX reconciliation_findings_open_subject_idx
    ON control.reconciliation_findings (subject_kind, subject_id)
    WHERE status = 'open';

-- The windowed sweep's access path on applied_facts, which until now carried
-- no index beyond its primary key. It is the fastest-growing table in this lane
-- (one row per fact the consumer derives from) and B13 is the first reader to
-- sweep it by time, so without this the windowed pass sequential-scans the
-- hottest table in the plane on every tick.
--
--   (applied_at, request_id): the windowed read's own shape — a half-open
--   range on applied_at, ordered, with request_id carried so the sweep is an
--   index walk rather than a sort. One index for one access path, and no
--   second: the other question the sweep asks of this table — "which applied
--   facts name settlement X" — the sweep does not ask, because it walks
--   settlements to their facts (the direction the primary key already answers
--   by (request_id, kind_class)), never facts to their settlements. And the
--   settlement-ledger read, which does group by settlement_id, runs against
--   control.ledger_entries, whose own (settlement_id, funding_bucket_id, kind)
--   unique index (000006) already leads with that column.
--
-- A second index here would be paid for on every settled fact's insert and
-- read by no plan — on the table in this lane that grows fastest, which is the
-- wrong place to be generous with an index nothing asks for.
--
-- Plain CREATE INDEX, never CONCURRENTLY: this lane's transaction rule
-- forbids non-transactional DDL, and a CONCURRENTLY that failed would leave
-- an invalid index behind rather than nothing.
CREATE INDEX applied_facts_applied_at_idx
    ON control.applied_facts (applied_at, request_id);

COMMENT ON TABLE control.reconciliation_findings IS
    'Divergences a reconciliation pass (B13) found and refused to repair: one row per OPEN finding, keyed by (check_kind, subject_kind, subject_id) through a partial unique index over status = ''open''. Detect-and-report only — nothing here moves money, and a correction the ledger could accept is a new operator-authored adjustment leg that this design deliberately does not write.';
COMMENT ON COLUMN control.reconciliation_findings.observed IS
    'The evidence the check compared: the cached and derived balances, the leg multiset, the settlement header, the refusal reason quoted verbatim. Structured jsonb written by this plane''s own checks — never a provider blob, a raw prompt or a fact payload, and the one column left unbounded on purpose (see the header).';
COMMENT ON COLUMN control.reconciliation_findings.subject_id IS
    'The entity the invariant was violated on: a bucket id, a settlement id, a request id, or the literal control_plane for a feed-wide signal. Bounded as evidence rather than as a grammar, because a check that cannot record its subject is not a check.';
COMMENT ON COLUMN control.reconciliation_findings.status IS
    'open (the check still fires), acknowledged (a human has seen it and nothing is decided), resolved or ignored (a human closed it, which frees the dedup key for a genuine later recurrence). Identity is append-only; the transition between these is the single sanctioned UPDATE.';

-- ---------------------------------------------------------------------------
-- Engine guards: findings are evidence, and evidence is never rewritten.
-- ---------------------------------------------------------------------------

-- The narrow update guard. It fires BEFORE UPDATE FOR EACH ROW and refuses
-- any change to a column outside the lifecycle triple (status, resolved_at,
-- last_seen_at), comparing them row-wise with IS DISTINCT FROM so a NULL on
-- either side is a difference rather than a comparison that never fires. The
-- comparison is spelled as a tuple of row constructors so the whole identity
-- is one expression: adding a column to this table later means adding it
-- here too, and a column that is not named here is frozen.
--
-- The search path is pinned: a trigger runs inside the writing session, and
-- an unqualified name in the body would resolve against whatever search_path
-- the caller set rather than against this schema's intent.
CREATE FUNCTION control.reject_finding_identity_rewrite() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
    AS $reject_finding_identity_rewrite$ BEGIN
    IF (NEW.check_kind, NEW.subject_kind, NEW.subject_id, NEW.severity,
        NEW.detected_at, NEW.observed, NEW.detail)
       IS DISTINCT FROM
       (OLD.check_kind, OLD.subject_kind, OLD.subject_id, OLD.severity,
        OLD.detected_at, OLD.observed, OLD.detail) THEN
        RAISE EXCEPTION USING
            MESSAGE = 'control.reconciliation_findings is append-only for its identity: only status, resolved_at and last_seen_at may change',
            DETAIL = 'the refused change would have rewritten the check_kind / subject_kind / subject_id / severity / detected_at / observed / detail of an existing finding, and those are frozen: a finding whose evidence can be rewritten in place is not evidence. The finding''s own subject is in the table; the error deliberately does not restate it, because a subject id is the one column here an operator-facing log line has no business quoting',
            ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$reject_finding_identity_rewrite$;

CREATE TRIGGER reconciliation_findings_identity_immutability
    BEFORE UPDATE ON control.reconciliation_findings
    FOR EACH ROW
    EXECUTE FUNCTION control.reject_finding_identity_rewrite();

-- The delete guard, and it is unconditional — there is no sanctioned delete
-- on this table, so the trigger is one statement with no condition. It fires
-- BEFORE DELETE FOR EACH STATEMENT: a statement-level guard costs nothing on
-- the path that matters (a passing UPDATE fires no DELETE trigger at all) and
-- cannot be defeated by a delete of zero rows, which a row-level guard with
-- a WHEN clause would let through on the same false-positive rate this
-- table's whole design is about.
CREATE FUNCTION control.reject_finding_delete() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
    AS $reject_finding_delete$ BEGIN
    RAISE EXCEPTION USING
        MESSAGE = 'control.reconciliation_findings rows are never removed',
        DETAIL = 'the refused operation was a ' || TG_OP || ': a divergence that was recorded and then deleted is a defect with no evidence left, and a finding is closed by its status rather than dropped',
        ERRCODE = 'integrity_constraint_violation';
    RETURN NULL;
END;
$reject_finding_delete$;

CREATE TRIGGER reconciliation_findings_no_delete
    BEFORE DELETE ON control.reconciliation_findings
    FOR EACH STATEMENT
    EXECUTE FUNCTION control.reject_finding_delete();

COMMENT ON FUNCTION control.reject_finding_identity_rewrite() IS
    'Reconciliation engine guard: a finding''s identity and evidence are written once. The only sanctioned UPDATE moves status, resolved_at and last_seen_at — the lifecycle columns a re-seeing pass and a resolving operator own.';
COMMENT ON FUNCTION control.reject_finding_delete() IS
    'Reconciliation engine guard: findings are never removed. A closed finding is a status, not an absence, and a table that could only lose rows could not answer "what was open, and when".';
