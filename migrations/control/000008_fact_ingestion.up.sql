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
    CONSTRAINT quarantined_facts_request_id_grammar
        CHECK (char_length(request_id) BETWEEN 1 AND 256),
    CONSTRAINT quarantined_facts_append_seq_positive CHECK (append_seq >= 1),
    CONSTRAINT quarantined_facts_kind_grammar
        CHECK (char_length(kind) BETWEEN 1 AND 64),
    CONSTRAINT quarantined_facts_schema_version_shape
        CHECK (schema_version >= 1),
    CONSTRAINT quarantined_facts_payload_grammar
        CHECK (octet_length(payload) BETWEEN 1 AND 32768),
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
