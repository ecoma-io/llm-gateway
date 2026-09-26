-- Migrations lane: control — the Control Plane's `control` database
-- (ADR 0006 §7). This file lands the three tables the usage-fact consumer
-- stands on: the position it applies through, the effects it has derived,
-- and the facts it refused.
--
-- 000008 — fact ingestion:
--
--   `ingestion_cursor` is the Control Plane's own position in the fact
--   feed — the one thing about this flow the Data Plane never sees (ADR 0006
--   §5's no-acknowledgement rule). It is a singleton by construction: the
--   migration seeds row 1 and the engine refuses any other id, so "two
--   consumers advancing one position independently" is not a race this
--   schema can even spell. The empty string is the seeded position and the
--   only meaning the cursor assigns to a string: never applied anything.
--   The position's grammar beyond that belongs to the Data Plane; the
--   column stores it verbatim and validates only its size.
--
--   `applied_facts` is the consumer's idempotency ledger, one row per
--   (request_id, kind class) pair the feed has been *derived from*. The
--   pair is the fact contract's identity for idempotency: a request
--   travels to exactly one terminal state, so its settled, released and
--   expired facts are one class — the first to arrive books the effect and
--   the others are replays — while an unbillable orphan is separate
--   bookkeeping a request may carry beside its settlement. The primary key
--   is the exactly-once boundary, the same guarantee the settlement header
--   draws by request_id, held one level up so a replayed fact is a read
--   before it is ever a second write. The row carries the lineage the
--   settlement cannot: the append_seq it arrived on, the amount and the
--   capture method it charged from, and the settlement of record it
--   produced — the seam a reconciliation pass (B13) will walk backwards
--   from the ledger to the fact.
--
--   `quarantined_facts` is where a fact the consumer cannot act on is
--   recorded instead of applied. The feed's whole-page rule says a fact a
--   consumer cannot decode or apply stops the consumer at that fact; the
--   dispositions that are not retryable — an unknown kind, a payload that
--   is not the version-1 envelope, a correction path this build has not
--   built, figures that do not cohere — are recorded here verbatim, with
--   the reason, inside the same transaction that advances past them.
--   Nothing is silently skipped: the row is the operator-visible state and
--   the reconciliation surface, and under-recording a quarantined fact is
--   the conservative direction (a fact that is never applied never
--   charges).
--
--   The two effect tables are deliberately separate. An applied fact and a
--   quarantined fact are different sentences about the same feed, and one
--   row cannot be both; the schema does not force them into one table with
--   a disposition flag a partial index would then have to unpick.
--
--   The quarantine's bounds bound storage, not the feed. Every refusal the
--   consumer classifies as quarantinable must be recordable — a CHECK that
--   restates the feed's grammar would turn the promised record-and-advance
--   into a constraint failure and a wedged feed, which is the outcome the
--   table exists to prevent. So the identity columns carry bounds above
--   anything the consumer can hand them (the adapter clamps at the record
--   boundary the way the reason is truncated), the payload keeps only the
--   size the interpreter already enforces before recording (an empty
--   payload is admissible evidence — it records as empty), and
--   schema_version carries no shape at all: a version this build does not
--   know is precisely what the column exists to carry whole. append_seq
--   stays >= 1 because that shape never reaches this table as a fact
--   disposition — the feed adapter refuses it at the page boundary
--   upstream.
--
--   Lane-convention deviation, recorded: 000006 supplies timestamps from
--   the application; this migration defaults applied_at, quarantined_at
--   and updated_at from the clock, because the consumer's own writes are
--   the only writers and the instant of recording is the engine's fact
--   about the row, not the caller's claim.

CREATE TABLE control.ingestion_cursor (
    id integer NOT NULL,
    position text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT ingestion_cursor_pkey PRIMARY KEY (id),
    CONSTRAINT ingestion_cursor_singleton CHECK (id = 1),
    CONSTRAINT ingestion_cursor_position_grammar
        CHECK (octet_length(position) <= 4096)
);

-- The singleton's seed row: the position at which a consumer that has never
-- applied anything starts. The empty string is the port's own convention
-- (the replay use case reads it and the adapter turns it into an absent
-- `after`), and the seed is what makes the first Position() read a real row
-- rather than a NOT FOUND the caller would have to mistake for a position.
INSERT INTO control.ingestion_cursor (id, position)
VALUES (1, '');

CREATE TABLE control.applied_facts (
    request_id text NOT NULL,
    kind_class text NOT NULL,
    kind text NOT NULL,
    append_seq bigint NOT NULL,
    settled_amount bigint,
    capture_method text,
    settlement_id uuid,
    applied_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT applied_facts_pkey PRIMARY KEY (request_id, kind_class),
    CONSTRAINT applied_facts_request_id_grammar
        CHECK (char_length(request_id) BETWEEN 1 AND 256),
    CONSTRAINT applied_facts_kind_class_valid
        CHECK (kind_class IN ('settlement', 'unbillable_orphaned')),
    CONSTRAINT applied_facts_kind_grammar
        CHECK (char_length(kind) BETWEEN 1 AND 64),
    CONSTRAINT applied_facts_append_seq_positive CHECK (append_seq >= 1),
    -- The settled fact is the only kind that carries a settled amount and
    -- the only kind that produces a settlement of record; a released or
    -- expired fact moved held capacity only, and an orphan moved nothing.
    -- Each pairing is one predicate so a future kind cannot arrive with
    -- half of the shape.
    CONSTRAINT applied_facts_settled_shape CHECK (
        (kind = 'settled') = (settled_amount IS NOT NULL)
        AND (kind = 'settled') = (settlement_id IS NOT NULL)
        AND (settled_amount IS NULL OR settled_amount >= 0)
    ),
    -- Capture provenance travels on the facts that priced or disclaimed
    -- usage (settled, unbillable_orphaned) and on nothing else: a release
    -- of held capacity captured nothing, and a capture method on one would
    -- be provenance for a figure that does not exist.
    CONSTRAINT applied_facts_capture_shape CHECK (
        (kind IN ('settled', 'unbillable_orphaned')) = (capture_method IS NOT NULL)
    ),
    -- The kind's class is derivable, not free: the pairing the idempotency
    -- contract rests on is a schema promise here, not application
    -- discipline — a row filing a settlement's kind under the orphan class
    -- (or the reverse) would slip the exactly-once boundary this table
    -- exists to enforce.
    CONSTRAINT applied_facts_kind_class_pairing CHECK (
        (kind_class = 'settlement') = (kind IN ('settled', 'released', 'expired'))
        AND (kind_class = 'unbillable_orphaned') = (kind = 'unbillable_orphaned')
    ),
    CONSTRAINT applied_facts_settlement_fkey
        FOREIGN KEY (settlement_id) REFERENCES control.settlements (id)
);

CREATE TABLE control.quarantined_facts (
    id bigint GENERATED ALWAYS AS IDENTITY,
    request_id text NOT NULL,
    append_seq bigint NOT NULL,
    kind text NOT NULL,
    schema_version integer NOT NULL,
    occurred_at timestamptz NOT NULL,
    payload text NOT NULL,
    capture_method text,
    committed_attempt_id text,
    provider_input_tokens bigint,
    provider_output_tokens bigint,
    delivery_tokens bigint,
    price_revision_id text,
    input_unit_price bigint,
    output_unit_price bigint,
    settled_amount bigint,
    corrects_append_seq bigint,
    reason text NOT NULL,
    quarantined_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT quarantined_facts_pkey PRIMARY KEY (id),
    -- One quarantined row per feed fact: the identity is the feed's own —
    -- (request_id, append_seq) is unique on the stream, and the kind names
    -- which fact at that position it was — so a page replayed after a
    -- crash finds its quarantine already recorded and records nothing
    -- twice.
    CONSTRAINT quarantined_facts_fact_unique UNIQUE (request_id, append_seq, kind),
    -- Bounds of evidence, not of the feed grammar: an empty request_id, an
    -- unknown kind of any length this column can hold, a schema version
    -- this build has never heard of and an empty payload are exactly the
    -- refusals this table records, so none may be refused here in turn.
    -- The adapter clamps the two identity columns to these bounds at the
    -- record boundary, the way it truncates the reason to its bound.
    CONSTRAINT quarantined_facts_request_id_evidence
        CHECK (char_length(request_id) <= 4096),
    CONSTRAINT quarantined_facts_append_seq_positive CHECK (append_seq >= 1),
    CONSTRAINT quarantined_facts_kind_evidence
        CHECK (char_length(kind) <= 256),
    CONSTRAINT quarantined_facts_payload_evidence
        CHECK (octet_length(payload) <= 32768),
    CONSTRAINT quarantined_facts_reason_grammar
        CHECK (char_length(reason) BETWEEN 1 AND 512)
);

COMMENT ON TABLE control.ingestion_cursor IS
'The Control Plane''s own position in the usage-fact feed: one singleton
row, advanced in the same transaction that applies the page it names.
Never acknowledged to the Data Plane (ADR 0006 §5).';
COMMENT ON COLUMN control.ingestion_cursor.position IS
'The opaque cursor the consumer has applied through; empty means it has
never applied anything.';
COMMENT ON TABLE control.applied_facts IS
'The usage-fact consumer''s idempotency ledger: one row per (request_id,
kind class) derived from, the exactly-once boundary of fact application.
Kind class, not kind alone: settled, released and expired are one class
per the fact contract, an unbillable orphan is the other.';
COMMENT ON COLUMN control.applied_facts.kind_class IS
'Either settlement (the request''s one terminal-state fact — settled,
released or expired) or unbillable_orphaned (separate bookkeeping that
coexists with the settlement).';
COMMENT ON COLUMN control.applied_facts.settlement_id IS
'The settlement of record this fact produced, present exactly when the
applied fact was a settled fact.';
COMMENT ON TABLE control.quarantined_facts IS
'Facts the consumer refused, recorded verbatim with the refusal reason in
the transaction that advanced past them — the explicit, operator-visible
disposition for a fact it cannot decode or apply. Never a silent skip.';
COMMENT ON COLUMN control.quarantined_facts.reason IS
'The consumer''s own refusal vocabulary: why this fact was not applied.';

-- ---------------------------------------------------------------------------
-- Engine guards: the two effect tables are history, like their lane
-- siblings. The consumer appends rows and nothing else — a correction to
-- the idempotency ledger or to a quarantine is a new row's business (or a
-- human operator's), never an edit to the record the disposition made.
-- ---------------------------------------------------------------------------

CREATE FUNCTION control.applied_facts_append_only() RETURNS trigger
LANGUAGE plpgsql AS $applied_facts_append_only$ BEGIN
    RAISE EXCEPTION 'control.applied_facts is append-only: % is refused (the idempotency ledger records applications; it does not revise them)', TG_OP;
END;
$applied_facts_append_only$;

CREATE TRIGGER applied_facts_append_only_guard
    BEFORE UPDATE OR DELETE ON control.applied_facts
    FOR EACH STATEMENT EXECUTE FUNCTION control.applied_facts_append_only();

CREATE FUNCTION control.quarantined_facts_append_only() RETURNS trigger
LANGUAGE plpgsql AS $quarantined_facts_append_only$ BEGIN
    RAISE EXCEPTION 'control.quarantined_facts is append-only: % is refused (a quarantine is the evidence a refusal produced; it is kept, not edited)', TG_OP;
END;
$quarantined_facts_append_only$;

CREATE TRIGGER quarantined_facts_append_only_guard
    BEFORE UPDATE OR DELETE ON control.quarantined_facts
    FOR EACH STATEMENT EXECUTE FUNCTION control.quarantined_facts_append_only();

COMMENT ON FUNCTION control.applied_facts_append_only() IS
    'Fact-ingestion engine guard: the idempotency ledger is history. A replay is answered by reading the row, never by rewriting it.';
COMMENT ON FUNCTION control.quarantined_facts_append_only() IS
    'Fact-ingestion engine guard: a quarantined fact is the verbatim evidence of a refusal. Resolving it is an operator''s act on the feed, not an edit to the record.';
