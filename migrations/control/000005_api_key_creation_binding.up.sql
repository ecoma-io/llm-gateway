-- Migrations lane: control — the Control Plane's `control` database
-- (ADR 0006 §7). This file adds no table: it tightens the identity
-- foundation (000002) where the adversarial review found the schema
-- trusting the application alone (issue #53, item 2). Two invariants of
-- api_keys were documented but not enforced:
--
--   same-account creation  a key's creator, when it has one, must be a user
--                           of the key's own account. The single-column FK
--                           000002 shipped proves the creator exists
--                           somewhere; a user id alone cannot pin the
--                           account, because user ids are unique across the
--                           table and one from any account satisfies the
--                           reference exactly as well.
--   the prefix binding      prefix must be this row's own id rendered
--                           gw_<id>_. 000002 pinned the SHAPE
--                           (api_keys_prefix_shape, the UUIDv4 grammar) and
--                           documented the rendering; nothing pinned that
--                           the shaped id inside the prefix is THIS row's.
--
-- Both are defense in depth: the application is the sole writer today — the
-- mint use case rejects cross-account creators, and the domain derives the
-- prefix from the id it mints (identity.TokenPrefix). A guard that has never
-- refused a row is a comment, not a constraint; these make the ownership
-- tree's shape survive the day a second writer exists.
--
-- Conventions held across the lane: explicit names on every constraint, in
-- the default-derived vocabulary the identity foundation uses; no
-- transaction control in this file — the runner delivers it as one implicit
-- transaction; no DELETE path and no CASCADE anywhere, as before.
--
-- Both guards validate existing rows when they apply. Every existing row was
-- written by the application that already enforced both invariants, so this
-- file migrates data it does not touch: either every row passes, or the
-- migration fails loudly on the row that proves the application's
-- enforcement was never real.
--
-- The inverse of this file is 000005_api_key_creation_binding.down.sql, in
-- reverse creation order.

-- ---------------------------------------------------------------------------
-- users — the referenced key the composite creator edge needs.
-- ---------------------------------------------------------------------------
-- A composite foreign key may only reference a column set the engine holds
-- as unique, so the creator edge below references (id, account_id) through
-- this constraint. It states no new business rule — id is already the
-- primary key, so the pair was unique before this file — it makes the pair
-- visible as a key so the engine can pin the account beside the id, the
-- thing an id alone cannot do.
ALTER TABLE control.users
    ADD CONSTRAINT users_id_account_id_key UNIQUE (id, account_id);

-- ---------------------------------------------------------------------------
-- api_keys — the creator edge, composite.
-- ---------------------------------------------------------------------------
-- The single-column edge is dropped and re-added composite: the pair
-- (created_by, account_id) must exist in users (id, account_id), which is
-- the same-account rule the mint use case has enforced in the application.
-- MATCH SIMPLE — the default, kept — preserves the nullable creator: a key
-- minted without a creating user is legitimate, and NULL created_by passes
-- exactly as it did before. The new name keeps the lane's default-derived
-- vocabulary, now over the wider column set.
ALTER TABLE control.api_keys
    DROP CONSTRAINT api_keys_created_by_fkey;

ALTER TABLE control.api_keys
    ADD CONSTRAINT api_keys_created_by_account_id_fkey
    FOREIGN KEY (created_by, account_id)
    REFERENCES control.users (id, account_id);

-- ---------------------------------------------------------------------------
-- api_keys — the prefix is this row's own rendering, or nothing.
-- ---------------------------------------------------------------------------
-- The documented invariant becomes load-bearing: 'gw_' || id::text || '_' is
-- byte-identical to what the domain mints (identity.TokenPrefix — the brand,
-- the id, the closing underscore; PostgreSQL renders a uuid as the same
-- canonical lowercase text validateUUIDForm demands, so the casing cannot
-- disagree), and a syntactically well-formed prefix naming another key is
-- refused rather than merely never written. api_keys_prefix_shape keeps its
-- own work — the v4 grammar — and the two remain separate checks because
-- applied history is immutable and because each names a different refusal.
ALTER TABLE control.api_keys
    ADD CONSTRAINT api_keys_prefix_id_consistency
    CHECK (prefix = 'gw_' || id::text || '_');
