-- Migrations lane: dataplane — the Data Plane runtime's `dataplane` database
-- (ADR 0006 §7). This file is the lane's fourth business schema: the client
-- price list (ADR 0003) — the operator-managed prices the runtime prices a
-- request under, alias-exact, in integer minor units per 1M tokens. The rows
-- live where the hot path reads them, which is why a price-list change is a
-- dataplane-lane migration and not a control-lane one, exactly as 000002
-- argues for the catalog it belongs to.
--
-- ADR 0005 fixes the aggregate ordering as identity → catalog → commerce →
-- accounting → events; the control lane's files carry identity and commerce,
-- this lane's 000002 carried the catalog, and this file carries the piece of
-- commerce the serving plane cannot serve without.
--
-- 000005 — client price list (ADR 0003):
--
--   client_price_list_revisions  the versioned price list: one revision is a
--                                whole operator-drafted price list, a draft
--                                until activated, and effective from a chosen
--                                instant (NOT from the activation instant —
--                                an operator schedules a price change to
--                                begin when they say, not when they click).
--                                Activated revisions are never edited; a
--                                change is a new revision.
--   client_price_list_entries    one revision's prices: one row per alias the
--                                revision prices, input and output unit prices
--                                in integer minor units per 1M tokens — never
--                                a float, never per-token arithmetic in the
--                                schema.
--
-- THE SELECTION IS TWO-STEP, and the shape is pinned here because the read
-- port and every future caller must agree on it: the effective revision is
-- the single activated revision with the greatest effective_from not after
-- transaction_timestamp() (ADR 0003 reads the database's clock, never a
-- caller-supplied one), and the effective price for an alias is that
-- revision's entry for the alias. A missing revision (none activated yet, or
-- none effective yet) and a missing entry (the effective revision does not
-- price this alias) are configuration states the caller refuses — an
-- admission with no price is a refusal, never a price of zero. Refusing is
-- the schema's contribution too: entries are alias-EXACT, so an alias without
-- an entry falls through to no row, not to a neighbouring alias's price.
--
-- The partial unique index on activated effective_from is what makes that
-- selection TOTAL: two activated revisions may not share an effective_from,
-- or "the greatest effective_from not after the instant" could return two rows
-- and the selection would be undefined. The index is also the selection's
-- lookup — the read filters `state = 'activated' AND effective_from <=
-- transaction_timestamp()` and takes the greatest, which this index serves.
-- Drafts may share effective_from values freely: only activation spends the
-- instant.
--
-- PAYG SCOPE TWIN. The last statement adds
-- quota_projections_payg_scope_wildcard to the runtime's projection table,
-- the CHECK twin of the waterfall ordering the drawdown walks by: a
-- payg_balance row is never a named scope (named_scope = false), because a
-- pay-as-you-go balance is the account's own money and cannot be scoped to an
-- alias group. The existing quota_projections_payg_scope CHECK pins the
-- row's other half (a PAYG row has no entitlement, no cycle, no period_end);
-- this one pins the scope half beside it. The drawdown's ORDER BY still
-- states `(scope_kind = 'payg_balance')` as its leading key explicitly rather
-- than borrowing this implication — the constraint and the order are the same
-- rule stated where each belongs.
--
-- Conventions held across this schema (000002's and 000004's, carried
-- forward):
--   * explicit names on every constraint and index, spelled in the
--     vocabulary docs/architecture/persistence.md records (the _state_valid,
--     _uuid_v7, _non_negative, _consistency CHECK suffixes, the
--     _key/_fkey/_idx defaults), so a future migration can name what it
--     alters and the verify suite can grep what it asserts;
--   * timestamptz everywhere, application-supplied, NOT NULL — there is no
--     clock this schema trusts more than the writing transaction's. The one
--     clock a read may consult is the same one: selection reads
--     transaction_timestamp(), never a caller's;
--   * lifecycle states are checked text spelled exactly as the domain spells
--     them (draft, activated); activation is a state, so there is no DELETE
--     path here at all — a price list revision is history once activated, and
--     reservations carry its revision id;
--   * primary keys are application-minted UUIDv7 (persistence.md's rule for
--     any row whose identity crosses a plane or reaches a client: revision
--     ids travel in usage facts and in reservations.price_revision_id);
--   * no ON DELETE CASCADE anywhere — both foreign keys say ON DELETE
--     RESTRICT in so many words: a revision with entries on file is not
--     silently orphaned, and an alias a price list references is not
--     silently detached.
--
-- The inverse of this file is 000005_client_price_list.down.sql, in reverse
-- creation order, no CASCADE.

-- ---------------------------------------------------------------------------
-- client_price_list_revisions — the versioned price list.
-- ---------------------------------------------------------------------------
CREATE TABLE client_price_list_revisions (
    id uuid PRIMARY KEY
        CONSTRAINT client_price_list_revisions_id_uuid_v7
        CHECK (id::text ~ '^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'),
    version integer NOT NULL
        CONSTRAINT client_price_list_revisions_version_valid
        CHECK (version >= 1),
    state text NOT NULL
        CONSTRAINT client_price_list_revisions_state_valid
        CHECK (state IN ('draft', 'activated')),
    effective_from timestamptz NOT NULL,
    activated_at timestamptz,
    created_at timestamptz NOT NULL,
    CONSTRAINT client_price_list_revisions_state_consistency
        CHECK ((state = 'activated') = (activated_at IS NOT NULL))
);

-- Total uniqueness across the whole table, not per anything: a revision's
-- version is the operator-facing ordinal of the price list itself ("price
-- list v4"), and usage facts and reservations will carry it next to the
-- revision id. There is no (group, version) parent here to scope it under —
-- the client price list is one list, gateway-global like the catalog's
-- aliases.
CREATE UNIQUE INDEX client_price_list_revisions_version_key
    ON client_price_list_revisions (version);

-- The selection's totality and the selection's lookup, one index: two
-- activated revisions may not share an effective_from, so "the single
-- activated revision with the greatest effective_from not after the instant"
-- always answers with exactly one row or none. Drafts are excluded — only
-- activation spends the instant, and two drafts aimed at the same instant is
-- an operator decision one of them must lose at activation, not at drafting.
CREATE UNIQUE INDEX client_price_list_revisions_activated_effective_from_key
    ON client_price_list_revisions (effective_from)
    WHERE state = 'activated';

COMMENT ON TABLE client_price_list_revisions IS
    'Client price list (ADR 0003): one versioned whole of the operator''s prices. The effective revision is the single activated revision with the greatest effective_from not after transaction_timestamp(); a missing revision is a configuration state the caller refuses, never a price of zero.';
COMMENT ON COLUMN client_price_list_revisions.version IS
    'The operator-facing ordinal of the list, unique across revisions and 1-based. Travelled beside the revision id so an operator can read a usage fact''s price basis without a join.';
COMMENT ON COLUMN client_price_list_revisions.state IS
    'Lifecycle: draft -> activated. Activation is the one-way step that spends the revision''s effective_from (the partial unique below), and an activated revision is never edited — a change is a new revision.';
COMMENT ON COLUMN client_price_list_revisions.effective_from IS
    'The instant the revision''s prices begin to answer, chosen by the operator at draft time — activation does not date it. Selection compares this against transaction_timestamp(): the database''s clock, never a caller''s (ADR 0003).';
COMMENT ON COLUMN client_price_list_revisions.activated_at IS
    'Activation instant; non-null exactly when state = activated (client_price_list_revisions_state_consistency). Informational — selection reads effective_from, not this.';

-- ---------------------------------------------------------------------------
-- client_price_list_entries — one revision's alias-exact prices.
-- ---------------------------------------------------------------------------
CREATE TABLE client_price_list_entries (
    revision_id uuid NOT NULL
        CONSTRAINT client_price_list_entries_revision_id_fkey
        REFERENCES client_price_list_revisions (id)
        ON DELETE RESTRICT,
    alias_id uuid NOT NULL
        CONSTRAINT client_price_list_entries_alias_id_fkey
        REFERENCES model_aliases (id)
        ON DELETE RESTRICT,
    input_unit_price bigint NOT NULL
        CONSTRAINT client_price_list_entries_input_unit_price_non_negative
        CHECK (input_unit_price >= 0),
    output_unit_price bigint NOT NULL
        CONSTRAINT client_price_list_entries_output_unit_price_non_negative
        CHECK (output_unit_price >= 0),
    CONSTRAINT client_price_list_entries_revision_alias_key
        UNIQUE (revision_id, alias_id)
);

-- The alias-exact read — "this revision's price for this alias" — is served
-- by the unique constraint above it; this index is the other direction the
-- operator's tooling and any future "which revisions price this alias" read
-- walk, and it mirrors model_candidates_alias_id_idx in 000002 for the same
-- reason.
CREATE INDEX client_price_list_entries_alias_id_idx
    ON client_price_list_entries (alias_id);

COMMENT ON TABLE client_price_list_entries IS
    'Client price list (ADR 0003): one revision''s price for one alias, alias-exact. An alias the revision does not price has no row — which selection reads as "no effective price", a refusal, never a neighbouring alias''s price and never zero.';
COMMENT ON COLUMN client_price_list_entries.revision_id IS
    'The revision whose prices these are. RESTRICT: a revision with entries on file is history usage facts still point at.';
COMMENT ON COLUMN client_price_list_entries.alias_id IS
    'The alias this entry prices, by id alone. Entries are per alias, not per candidate: every candidate under the alias is priced by the same entry, because the client chose the alias and the alias is what the price list answers for.';
COMMENT ON COLUMN client_price_list_entries.input_unit_price IS
    'Input price in integer minor units per 1M tokens — never a float. Zero is a legitimate price (a free tier is a row, not an absence).';
COMMENT ON COLUMN client_price_list_entries.output_unit_price IS
    'Output price in integer minor units per 1M tokens — never a float, same rules as input_unit_price.';

-- ---------------------------------------------------------------------------
-- quota_projections — pin the PAYG row's scope half.
-- ---------------------------------------------------------------------------
-- The CHECK twin of the waterfall ordering: a pay-as-you-go balance is the
-- account's own money, so it is never a named scope — a payg_balance row
-- must carry named_scope = false, exactly as its sibling
-- quota_projections_payg_scope already pins the row's other half (no
-- entitlement, no cycle, no period_end). Vacuous on every row that exists
-- today (admission's publication algebra already seeds PAYG rows with
-- named_scope = false); stated here so a future writer cannot seed a scoped
-- PAYG balance and put an alias group between an account and its own money.
ALTER TABLE quota_projections
    ADD CONSTRAINT quota_projections_payg_scope_wildcard
    CHECK (scope_kind <> 'payg_balance' OR named_scope = false);
