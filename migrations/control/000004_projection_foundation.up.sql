-- Migrations lane: control — the Control Plane's `control` database
-- (ADR 0006 §7). This file is the lane's third business schema: the
-- Control → Data projection foundation (ADR 0007) — the durable change log
-- and materialized tables the Control Plane delivers to the Data Plane's
-- credential mirror, written in the same transaction as the authoritative
-- identity and commerce changes they describe.
--
-- 000004 — projection foundation (ADR 0006 §5, §8; ADR 0007):
--
--   projection_revision   the gapless revision counter, a singleton row.
--                         Every recorded change allocates `last_revision + 1`
--                         by UPDATE ... RETURNING inside the writer's own
--                         transaction: the row lock held to commit serializes
--                         management-paced writes, which makes allocation
--                         order equal commit order — the fact the projection's
--                         snapshot consistency rests on. `epoch` names the
--                         producer timeline: a restored database rewinds this
--                         counter, and re-minting the epoch is the restore
--                         procedure's step that makes the rewind detectable
--                         instead of silent (ADR 0007 §6) — the restored row
--                         carries the old epoch, so a restore alone mints
--                         nothing.
--   projection_changes    the durable change log — one self-contained,
--                         full-state entry per change, keyed by revision.
--                         Replay is the delivery model's answer to every
--                         crash and lost acknowledgement, and replay needs
--                         the entry to describe the row's whole state, never
--                         a delta.
--   projection_api_keys   the materialized credential projection — the
--                         snapshot source. Written by the same transaction
--                         that appends the log entry, so a snapshot cut is
--                         always a consistent superset of everything committed
--                         before it (ADR 0007's proof).
--   projection_accounts   the materialized account-state projection — the
--                         same shape, for the account half of the mirror.
--
-- A correction to the lane's applied history, carried here because the
-- identity foundation cannot be edited (applied migrations are history):
-- 000002's header and table comments state that no digest column may ever
-- exist in this lane — "the day one appears in this lane, the two-record
-- model is broken". That rule binds the OWNERSHIP records, and it still
-- binds them: `control.api_keys` gains no secret material here and never
-- will. The projection artifacts above are a different thing — the delivery
-- pipeline's own source-of-record — and ADR 0007 amends ADR 0006 §8 to name
-- them the one place the secret's DIGEST (verification material, never the
-- plaintext) transits the Control Plane's database on its way to the plane
-- that verifies it. The payload copy below carries the same hex-form CHECK
-- as the materialized column, so both stored copies of a digest are pinned
-- to their strongest form.
--
-- Conventions held across this schema:
--   * explicit names on every constraint and index;
--   * timestamptz everywhere, application-supplied, NOT NULL;
--   * lifecycle states are checked text spelled exactly as the domain spells
--     them; no DELETE path — revocation is a state, and the log is history;
--   * no transaction control in this file: the runner wraps it in one
--     implicit transaction, and the migration fails or applies whole.
--
-- The file also backfills what can be backfilled: one log entry and one
-- mirror row per existing account (revisions 1..N, in id order), with the
-- counter seeded at N — the three statements reading one consistent state of
-- control.accounts, held against concurrent writers by the SHARE lock the
-- backfill takes. API keys are deliberately not backfilled — their
-- digests have never existed in this database, which is the two-record
-- boundary working as designed; the projection's epoch begins with this
-- migration, and ADR 0007 records the recovery for keys that predate it.
-- The inverse of this file is 000004_projection_foundation.down.sql, in
-- reverse creation order, no CASCADE.

-- ---------------------------------------------------------------------------
-- projection_revision — the gapless counter, a singleton row.
-- ---------------------------------------------------------------------------
CREATE TABLE control.projection_revision (
    id integer PRIMARY KEY,
    epoch uuid NOT NULL,
    last_revision bigint NOT NULL,
    CONSTRAINT projection_revision_id_singleton
        CHECK (id = 1),
    CONSTRAINT projection_revision_last_revision_nonnegative
        CHECK (last_revision >= 0)
);

COMMENT ON TABLE control.projection_revision IS
    'Projection foundation (ADR 0007): the gapless revision counter — one row, ever. Allocation happens inside the writer''s transaction, so revision order is commit order.';
COMMENT ON COLUMN control.projection_revision.epoch IS
    'The producer timeline''s identity. A database restore rewinds last_revision while restoring this same epoch with it, so the restore procedure re-mints the epoch — without that step the rewind is silent (ADR 0007 §6).';
COMMENT ON COLUMN control.projection_revision.last_revision IS
    'The highest revision allocated. Gapless by construction: an aborted transaction releases the row lock and its number is reused by the next writer.';

-- ---------------------------------------------------------------------------
-- projection_changes — the durable change log.
-- ---------------------------------------------------------------------------
CREATE TABLE control.projection_changes (
    revision bigint PRIMARY KEY
        CONSTRAINT projection_changes_revision_positive
        CHECK (revision >= 1),
    resource_kind text NOT NULL
        CONSTRAINT projection_changes_resource_kind_valid
        CHECK (resource_kind IN ('api_key', 'account')),
    resource_id uuid NOT NULL,
    recorded_at timestamptz NOT NULL,
    payload jsonb NOT NULL
        CONSTRAINT projection_changes_payload_object
        CHECK (jsonb_typeof(payload) = 'object')
        CONSTRAINT projection_changes_payload_digest_shape
        CHECK (resource_kind <> 'api_key'
               OR (payload ? 'digest'
                   AND payload->>'digest' ~ '^[0-9a-f]{64}$'))
);

CREATE INDEX projection_changes_resource_idx
    ON control.projection_changes (resource_kind, resource_id);

COMMENT ON TABLE control.projection_changes IS
    'Projection foundation (ADR 0007): the durable change log the delivery loop replays. Entries are self-contained full-state rows — never deltas — so replay is safe under any crash and any lost acknowledgement.';
COMMENT ON COLUMN control.projection_changes.revision IS
    'Allocated from projection_revision inside the writing transaction. The primary key is the ordering, the guard and the idempotency key at once.';
COMMENT ON COLUMN control.projection_changes.resource_id IS
    'The projected row''s identity: the key id for api_key, the account id for account.';
COMMENT ON COLUMN control.projection_changes.payload IS
    'Full state at this revision: {account_id, digest, state, revoked_at} for api_key; {state} for account. For an api_key entry the digest is required and pinned to the same 64-hex form as the materialized column — required in the CHECK itself, not merely by the writer (ADR 0007''s amendment of ADR 0006 §8).';

-- ---------------------------------------------------------------------------
-- projection_api_keys — the materialized credential projection.
-- ---------------------------------------------------------------------------
CREATE TABLE control.projection_api_keys (
    key_id uuid PRIMARY KEY,
    account_id uuid NOT NULL,
    digest text NOT NULL
        CONSTRAINT projection_api_keys_digest_shape
        CHECK (digest ~ '^[0-9a-f]{64}$'),
    state text NOT NULL
        CONSTRAINT projection_api_keys_state_valid
        CHECK (state IN ('active', 'revoked')),
    revoked_at timestamptz,
    source_revision bigint NOT NULL,
    updated_at timestamptz NOT NULL,
    CONSTRAINT projection_api_keys_revocation_consistency
        CHECK ((state = 'revoked') = (revoked_at IS NOT NULL)),
    CONSTRAINT projection_api_keys_source_revision_nonnegative
        CHECK (source_revision >= 0)
);

COMMENT ON TABLE control.projection_api_keys IS
    'Projection foundation (ADR 0007): the materialized credential projection — the snapshot source. Written in the same transaction as the authoritative change and its log entry; every credential here is delivered to the Data Plane''s mirror, and nothing here is the ownership record (control.api_keys stays digest-free).';
COMMENT ON COLUMN control.projection_api_keys.digest IS
    'The lowercase hex SHA-256 of the key''s secret — verification material, never the plaintext. The delivery pipeline''s reason to hold it is stated in ADR 0007.';
COMMENT ON COLUMN control.projection_api_keys.source_revision IS
    'The revision of the log entry last written into this row. A snapshot cut at revision R is every row here with source_revision <= R, plus the committed rows past it that later redelivery guard-skips.';

-- ---------------------------------------------------------------------------
-- projection_accounts — the materialized account-state projection.
-- ---------------------------------------------------------------------------
CREATE TABLE control.projection_accounts (
    account_id uuid PRIMARY KEY,
    state text NOT NULL
        CONSTRAINT projection_accounts_state_valid
        CHECK (state IN ('active', 'suspended', 'closed')),
    source_revision bigint NOT NULL,
    updated_at timestamptz NOT NULL,
    CONSTRAINT projection_accounts_source_revision_nonnegative
        CHECK (source_revision >= 0)
);

COMMENT ON TABLE control.projection_accounts IS
    'Projection foundation (ADR 0007): the materialized account-state projection — lifecycle only, no ownership edges and no billing data. Same write discipline as projection_api_keys.';

-- ---------------------------------------------------------------------------
-- Backfill — every existing account enters the projection at migration time.
-- ---------------------------------------------------------------------------
-- One log entry and one mirror row per account, revisions 1..N in id order,
-- the counter seeded at N. Keys are not backfilled: their digests have never
-- existed in this database (see the header's correction paragraph).
--
-- The three statements below must describe ONE state of control.accounts,
-- and under READ COMMITTED every statement takes its own snapshot: a
-- zero-downtime deploy's old binary can commit an INSERT or a lifecycle
-- UPDATE between them, which would give the log N entries, the mirror N+1
-- rows and the counter a value with no log entry behind it — a permanently
-- wedged projection whose first delivered batch would fail the consumer's
-- contiguity check forever. The SHARE lock takes the table away from
-- concurrent writers (readers are untouched) for the moment this transaction
-- is open, which makes the three statements one consistent read. It is a
-- table lock inside the runner's transaction, not transaction control: the
-- migration still fails or applies whole.

LOCK TABLE control.accounts IN SHARE MODE;

INSERT INTO control.projection_changes (revision, resource_kind, resource_id,
                                        recorded_at, payload)
SELECT row_number() OVER (ORDER BY id),
       'account',
       id,
       created_at,
       jsonb_build_object('state', state)
FROM control.accounts;

INSERT INTO control.projection_accounts (account_id, state, source_revision,
                                         updated_at)
SELECT id,
       state,
       row_number() OVER (ORDER BY id),
       created_at
FROM control.accounts;

INSERT INTO control.projection_revision (id, epoch, last_revision)
SELECT 1,
       gen_random_uuid(),
       (SELECT count(*) FROM control.accounts);
