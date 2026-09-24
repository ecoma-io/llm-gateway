-- Migrations lane: control — the Control Plane's `control` database
-- (ADR 0006 §7). The lane's first file built the `control` namespace this
-- file's tables live in; this file is the lane's first business schema:
-- everything identity, and therefore the ownership root every later
-- control-side migration stands on. ADR 0005 fixes the ordering as identity
-- → catalog → commerce → accounting → events; this file is the first link
-- of that chain.
--
-- 000001 — identity foundation (ADR 0001; issue #45):
--
--   accounts  the ownership root. Billing state is deliberately absent:
--             pay-as-you-go buckets are Accounting's own aggregate (ADR 0001),
--             and creating an account creates exactly this record.
--   users     console identities; each belongs to exactly one account.
--   api_keys  the API key's OWNERSHIP record only. Ownership edges, display
--             metadata, lifecycle. No column below stores anything secret —
--             not the plaintext, which exists only inside the mint call and
--             is shown once, and not the secret's digest, which is the Data
--             Plane's credential record's column (ADR 0006 §8). The absence
--             of a digest column here is a designed boundary, not a gap: the
--             day one appears in this lane, the two-record model is broken.
--
-- Conventions held across the lane:
--   * explicit names on every constraint and index, so a future migration can
--     name what it alters and error messages name what fired;
--   * timestamptz everywhere, application-supplied, NOT NULL — there is no
--     clock this schema trusts more than the writing transaction's;
--   * lifecycle states are checked text, spelled exactly as the domain spells
--     them (identity.AccountState et al.), one-way exits included;
--   * no DELETE path and no ON DELETE CASCADE anywhere: every lifecycle exit
--     is a state, and the rows are history the Control Plane is the authority
--     for. The foreign keys below are RESTRICT by default on purpose.
--
-- The inverse of this file is 000002_identity_foundation.down.sql, in
-- reverse creation order, no CASCADE.

BEGIN;

-- ---------------------------------------------------------------------------
-- accounts — the ownership root.
-- ---------------------------------------------------------------------------
CREATE TABLE control.accounts (
    id uuid PRIMARY KEY,
    name text NOT NULL
        CONSTRAINT accounts_name_length
        CHECK (char_length(name) BETWEEN 1 AND 256),
    state text NOT NULL
        CONSTRAINT accounts_state_valid
        CHECK (state IN ('active', 'suspended', 'closed')),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

COMMENT ON TABLE control.accounts IS
    'Identity ownership root (ADR 0001): the account users and API keys belong to. Billing lives in Accounting, not here.';
COMMENT ON COLUMN control.accounts.state IS
    'Lifecycle: active -> suspended -> closed. Closed is terminal; no transition may leave it.';
COMMENT ON COLUMN control.accounts.created_at IS
    'Application-supplied creation instant (timestamptz).';
COMMENT ON COLUMN control.accounts.updated_at IS
    'Application-supplied last-transition instant (timestamptz).';

-- ---------------------------------------------------------------------------
-- users — console identities, each belonging to exactly one account.
-- ---------------------------------------------------------------------------
CREATE TABLE control.users (
    id uuid PRIMARY KEY,
    account_id uuid NOT NULL
        CONSTRAINT users_account_id_fkey
        REFERENCES control.accounts (id),
    email text NOT NULL
        CONSTRAINT users_email_length
        CHECK (char_length(email) BETWEEN 3 AND 254)
        CONSTRAINT users_email_shape
        CHECK (email ~ '^[^@[:space:]]+@[^@[:space:]]+$'),
    state text NOT NULL
        CONSTRAINT users_state_valid
        CHECK (state IN ('invited', 'active', 'removed')),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

-- Uniqueness is (account_id, email) across LIVE rows only: a removed user's
-- address may be invited again, while an invited or active one holds it. A
-- partial unique index is the exact shape of that rule; a plain unique
-- constraint would let removal hold addresses hostage forever.
CREATE UNIQUE INDEX users_account_live_email_key
    ON control.users (account_id, email)
    WHERE state <> 'removed';

-- The foreign key is the lookup the console actually runs (list an account's
-- users); the index is not free, and this is what it is for.
CREATE INDEX users_account_id_idx ON control.users (account_id);

COMMENT ON TABLE control.users IS
    'Console identity (ADR 0001): exactly one account per user, no secondary membership, no cross-account identity.';
COMMENT ON COLUMN control.users.email IS
    'Lowercase normalised by the domain before insert; uniqueness compares this column directly.';
COMMENT ON COLUMN control.users.state IS
    'Lifecycle: invited -> active -> removed. Removed is terminal; the row stays as history.';

-- ---------------------------------------------------------------------------
-- api_keys — the OWNERSHIP half of the two-record API-key model.
-- ---------------------------------------------------------------------------
CREATE TABLE control.api_keys (
    id uuid PRIMARY KEY,
    account_id uuid NOT NULL
        CONSTRAINT api_keys_account_id_fkey
        REFERENCES control.accounts (id),
    created_by uuid
        CONSTRAINT api_keys_created_by_fkey
        REFERENCES control.users (id),
    display_name text NOT NULL
        CONSTRAINT api_keys_display_name_length
        CHECK (char_length(display_name) BETWEEN 1 AND 256),
    prefix text NOT NULL
        CONSTRAINT api_keys_prefix_shape
        CHECK (prefix ~ '^gw_[0-9a-f]{8}-([0-9a-f]{4}-){3}[0-9a-f]{12}_$'),
    state text NOT NULL
        CONSTRAINT api_keys_state_valid
        CHECK (state IN ('active', 'revoked')),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    revoked_at timestamptz,
    CONSTRAINT api_keys_revocation_consistency
        CHECK ((state = 'revoked') = (revoked_at IS NOT NULL))
);

-- A key's id IS its public prefix material, so prefix carries no secret and
-- needs no uniqueness of its own — it is a deterministic rendering of the
-- primary key (gw_<id>_); the shape check above pins that rendering.

-- Ownership and creator are the lookups the console runs (an account's keys,
-- a user's keys). The creator edge is nullable on purpose: a key minted
-- without a creating user is legitimate, and NULL is the domain's own "no
-- creator" (identity.APIKey.CreatedBy).
CREATE INDEX api_keys_account_id_idx ON control.api_keys (account_id);
CREATE INDEX api_keys_created_by_idx ON control.api_keys (created_by);

COMMENT ON TABLE control.api_keys IS
    'API-key ownership record (ADR 0006 §8): who a key belongs to and whether it is revoked. No secret material — not the plaintext, not the digest. The credential record with the digest is the Data Plane''s.';
COMMENT ON COLUMN control.api_keys.created_by IS
    'Creating user, when a user minted the key. NULL when none; the minting use case rejects cross-account creators.';
COMMENT ON COLUMN control.api_keys.prefix IS
    'Public token prefix gw_<key-id>_. Derived from the id, carries no secret, safe to display.';
COMMENT ON COLUMN control.api_keys.state IS
    'Lifecycle: active -> revoked. Revoked is terminal; there is no un-revoke.';
COMMENT ON COLUMN control.api_keys.revoked_at IS
    'Revocation instant; non-nil exactly when state = revoked (api_keys_revocation_consistency).';

COMMIT;
