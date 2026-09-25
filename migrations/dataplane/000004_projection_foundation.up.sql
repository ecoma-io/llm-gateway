-- Migrations lane: dataplane — the Data Plane runtime's `dataplane` database
-- (ADR 0006 §7). This file is the lane's third business schema: the
-- projection foundation (ADR 0007) — the credential mirror the Control Plane
-- writes through the private management listener, and the one state the
-- runtime's request path will verify credentials against. The Control Plane
-- is the authority for every row here; the runtime never writes them itself
-- and never questions them per request.
--
-- ADR 0005 fixes the aggregate ordering as identity → catalog → commerce →
-- accounting → events; the control lane's files carry identity, this lane's
-- 000002 carried the catalog, and this file carries the projection of
-- identity onto the plane that verifies it.
--
-- 000004 — projection foundation (ADR 0006 §5, §8; ADR 0007):
--
--   api_key_credentials  the credential half of the two-record API-key model
--                        (ADR 0006 §8): the key id, its account, the secret's
--                        SHA-256 digest — verification material, never the
--                        plaintext, which exists only inside the mint call —
--                        and the lifecycle state. The ownership record with
--                        the same identity lives in the Control Plane's
--                        database; that is the boundary, and no column here
--                        reaches across it.
--   account_states       the account lifecycle the runtime mirrors: admission
--                        refuses credentials of a suspended or closed account.
--                        Lifecycle only — no ownership edges, no billing.
--   projection_state     the consumer position, a singleton: whether a
--                        snapshot has been applied, the highest revision whose
--                        effects are committed, and the producer timeline
--                        (epoch) that position was earned on. The position is
--                        advanced in the same transaction as the rows it
--                        describes — that is what makes a crash between the
--                        two a replay rather than a loss.
--
-- There is deliberately NO foreign key between api_key_credentials and
-- account_states: the two rows arrive on independent feed entries, in either
-- order, and a credential must be applicable even while its account's row has
-- not arrived (the join is the admission path's business, not the schema's).
--
-- Conventions held across this schema (000002's, carried forward):
--   * explicit names on every constraint and index;
--   * timestamptz everywhere, application-supplied, NOT NULL;
--   * lifecycle states are checked text spelled exactly as the domain spells
--     them; revocation is a state, so there is no LIFECYCLE deletion — a
--     credential is never un-revoked by rewriting it away. The one deliberate
--     DELETE lives in the delivery path, not the lifecycle: a snapshot is
--     whole replacement, and its absence deletes (removing every row the
--     snapshot does not name) are how the mirror stops serving a key the
--     Control Plane no longer projects. Incremental delivery still only ever
--     upserts;
--   * no transaction control in this file: the runner wraps it in one
--     implicit transaction, and the migration fails or applies whole.
--
-- The inverse of this file is 000004_projection_foundation.down.sql, in
-- reverse creation order, no CASCADE.

-- ---------------------------------------------------------------------------
-- api_key_credentials — the credential record of the two-record model.
-- ---------------------------------------------------------------------------
CREATE TABLE api_key_credentials (
    key_id uuid PRIMARY KEY,
    account_id uuid NOT NULL,
    digest text NOT NULL
        CONSTRAINT api_key_credentials_digest_grammar
        CHECK (digest ~ '^[0-9a-f]{64}$'),
    state text NOT NULL
        CONSTRAINT api_key_credentials_state_valid
        CHECK (state IN ('active', 'revoked')),
    revoked_at timestamptz,
    source_revision bigint NOT NULL,
    updated_at timestamptz NOT NULL,
    CONSTRAINT api_key_credentials_revocation_consistency
        CHECK ((state = 'revoked') = (revoked_at IS NOT NULL)),
    CONSTRAINT api_key_credentials_source_revision_nonnegative
        CHECK (source_revision >= 0)
);

COMMENT ON TABLE api_key_credentials IS
    'Projection foundation (ADR 0007): the credential mirror the Control Plane writes and the runtime reads on the request path. The secret''s plaintext never exists here — only its SHA-256 digest.';
COMMENT ON COLUMN api_key_credentials.digest IS
    'Lowercase hex SHA-256 of the key''s secret. A presented credential is admitted by digesting it and comparing against this value; the digest confirms a secret, it cannot reconstruct one.';
COMMENT ON COLUMN api_key_credentials.source_revision IS
    'The producer revision this row''s content was written at. Incremental delivery updates a row only for a strictly higher revision; a snapshot updates it unconditionally (ADR 0007).';
COMMENT ON COLUMN api_key_credentials.state IS
    'Lifecycle: active -> revoked. Revoked is terminal — there is no un-revoke, on this plane or the other.';

-- ---------------------------------------------------------------------------
-- account_states — the account lifecycle the runtime mirrors.
-- ---------------------------------------------------------------------------
CREATE TABLE account_states (
    account_id uuid PRIMARY KEY,
    state text NOT NULL
        CONSTRAINT account_states_state_valid
        CHECK (state IN ('active', 'suspended', 'closed')),
    source_revision bigint NOT NULL,
    updated_at timestamptz NOT NULL,
    CONSTRAINT account_states_source_revision_nonnegative
        CHECK (source_revision >= 0)
);

COMMENT ON TABLE account_states IS
    'Projection foundation (ADR 0007): the mirrored account lifecycle — admission refuses credentials of a suspended or closed account. No ownership edges, no billing; the authority is the Control Plane''s account row.';
COMMENT ON COLUMN account_states.state IS
    'Lifecycle: active -> suspended -> closed. Closed is terminal; the row stays as history the mirror keeps.';

-- ---------------------------------------------------------------------------
-- projection_state — the consumer position, a singleton row.
-- ---------------------------------------------------------------------------
CREATE TABLE projection_state (
    id integer PRIMARY KEY,
    bootstrapped boolean NOT NULL,
    applied_revision bigint NOT NULL,
    producer_epoch uuid,
    CONSTRAINT projection_state_id_singleton
        CHECK (id = 1),
    CONSTRAINT projection_state_applied_revision_nonnegative
        CHECK (applied_revision >= 0)
);

COMMENT ON TABLE projection_state IS
    'Projection foundation (ADR 0007): the consumer position. Rows and this position commit in one transaction, so the position never names effects that are not on disk.';
COMMENT ON COLUMN projection_state.bootstrapped IS
    'False only before the first snapshot: an unbootstrapped consumer holds no projection and is offered one, never a batch.';
COMMENT ON COLUMN projection_state.applied_revision IS
    'The highest revision whose effects are committed. Incremental batches must join this position exactly; a snapshot may move it either way, because recovery may have to reach a timeline that rewound.';
COMMENT ON COLUMN projection_state.producer_epoch IS
    'The producer timeline the position was earned on; NULL only before the first snapshot. Entries naming any other epoch are refused and answered with a re-snapshot.';

-- The singleton is seeded here rather than tolerated as absent: every apply
-- reads and advances this row inside its transaction, and a row the schema
-- guarantees makes "no position" an unreachable state rather than a branch
-- every caller must remember to handle.
INSERT INTO projection_state (id, bootstrapped, applied_revision, producer_epoch)
VALUES (1, false, 0, NULL);
