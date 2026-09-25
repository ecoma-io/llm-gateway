-- Migrations lane: dataplane — the Data Plane runtime's `dataplane` database
-- (ADR 0006 §7). This file is the lane's first business schema: the model
-- catalog, the configured serving state the runtime resolves on every request
-- and the Control Plane never reads (ADR 0006 §3, §5; ADR 0001 rule 8 keeps
-- the catalog on the Data Plane side of the split — the rows live where the
-- hot path reads them, which is also why a catalog change is a dataplane-lane
-- migration and not a control-lane one).
--
-- ADR 0005 fixes the aggregate ordering as identity → catalog → commerce →
-- accounting → events; the control lane's files carry identity, and this file
-- is catalog, the chain's second link.
--
-- One correction to the lane's applied history, carried here because the
-- bootstrap file cannot be edited (applied migrations are history): its
-- comments describe the image's extension install surviving the initdb mount
-- as the reason IF NOT EXISTS is honest. That stopped being true when the
-- fixture began mounting ./initdb over /docker-entrypoint-initdb.d, shadowing
-- the image's script — the bootstrap's CREATE EXTENSION genuinely creates the
-- extension, exactly as the control lane's 000001 comment already states and
-- as verify.sh asserts. The stale sentences are kept as written; this file is
-- where the correction is written down.
--
-- 000002 — catalog foundation (ADR 0001, ADR 0002; issue #49):
--
--   backends             one configured instance of an adapter (ADR 0002):
--                        an endpoint, the adapter type that picks a driver,
--                        and two opaque references the runtime is expected
--                        to resolve when it calls out. The references are
--                        deliberately uninterpreted here — where provider
--                        credentials and egress policies actually live is the
--                        provider phase's decision (a missing decision the
--                        conventions require raised, not filled: issue #49
--                        records the choice to carry nullable reference
--                        strings and decide nothing else).
--   model_aliases        the only model identifier a client ever sees
--                        (ADR 0002), gateway-global and operator-managed,
--                        with the bounds admission enforces before a
--                        candidate is chosen.
--   model_candidates     the alias's ordered fallback list — position 1 is
--                        primary (ADR 0002). Rows of the alias's aggregate,
--                        written and replaced only inside the alias's
--                        transaction; a candidate references its backend by
--                        id alone (ADR 0001, rule 3).
--   alias_group_versions an immutable membership snapshot of a named alias
--                        group (ADR 0003); entitlements in the other plane
--                        reference a version by id alone (ADR 0006 §7), so
--                        editing membership means a new version and never an
--                        edit. `*` is the wildcard group's reserved name and
--                        its membership snapshot is the empty set — it
--                        contains every alias by definition, which is why it
--                        has no member rows and exactly one version, ever.
--   alias_group_members  the stored snapshot itself — the version's member
--                        alias set. Inserted when the version is created,
--                        never updated, never deleted: immutability is the
--                        version's whole point.
--
-- Conventions held across this schema:
--   * explicit names on every constraint and index, spelled in the
--     vocabulary docs/architecture/persistence.md records (the _state_valid,
--     _grammar, _positive, _consistency CHECK suffixes, the _key/_fkey/_idx
--     defaults), so a future migration can name what it alters and the
--     verify suite can grep what it asserts;
--   * timestamptz everywhere, application-supplied, NOT NULL — there is no
--     clock this schema trusts more than the writing transaction's;
--   * lifecycle states are checked text, spelled exactly as the domain
--     spells them (catalog.AliasState, catalog.BackendState); unlike the
--     control identity schema, this one has a DELETE path: replacing an
--     alias's candidate set deletes the outgoing candidates, because the
--     aggregate's candidate list is mutable configuration — request_attempts
--     snapshot the position, backend and provider model they used, so the
--     history the ledger settles from does not depend on these rows. Alias
--     and backend rows themselves have none: every lifecycle exit is a state;
--   * primary keys are application-minted UUIDv7 (persistence.md's rule for
--     any row whose identity crosses a plane or reaches a client: group
--     versions are referenced from the Control Plane's entitlements, alias
--     and backend ids travel in usage facts);
--   * no ON DELETE CASCADE anywhere — the foreign keys below say ON DELETE
--     RESTRICT in so many words: a backend with candidates on file is not
--     silently orphaned by a delete this schema does not offer, and a
--     version's snapshot does not silently shrink. RESTRICT rather than the
--     default NO ACTION is deliberate even though they behave identically
--     while a constraint stays non-deferrable — the keyword pins the intent
--     against a future deferrable conversion NO ACTION would silently
--     reinterpret.
--
-- Two invariants the application owns rather than the schema, because a
-- CHECK cannot see across tables: a non-wildcard group version has at least
-- one member, and an alias always has at least one candidate. Both hold by
-- construction — alias and candidates are written in one transaction, group
-- version and members in one transaction — and the domain refuses the empty
-- forms before any statement runs.
--
-- The inverse of this file is 000002_catalog_foundation.down.sql, in reverse
-- creation order, no CASCADE.

-- ---------------------------------------------------------------------------
-- backends — one configured instance of an adapter.
-- ---------------------------------------------------------------------------
CREATE TABLE backends (
    id uuid PRIMARY KEY,
    adapter_type text NOT NULL
        CONSTRAINT backends_adapter_type_grammar
        CHECK (adapter_type ~ '^[a-z0-9]+(-[a-z0-9]+)*$'
               AND char_length(adapter_type) <= 64),
    endpoint text NOT NULL
        CONSTRAINT backends_endpoint_length
        CHECK (char_length(endpoint) BETWEEN 9 AND 2048)
        CONSTRAINT backends_endpoint_url
        CHECK (endpoint ~ '^https?://'),
    credentials_ref text
        CONSTRAINT backends_credentials_ref_length
        CHECK (credentials_ref IS NULL OR char_length(credentials_ref) BETWEEN 1 AND 512),
    egress_policy_ref text
        CONSTRAINT backends_egress_policy_ref_length
        CHECK (egress_policy_ref IS NULL OR char_length(egress_policy_ref) BETWEEN 1 AND 512),
    state text NOT NULL
        CONSTRAINT backends_state_valid
        CHECK (state IN ('active', 'disabled')),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

COMMENT ON TABLE backends IS
    'Catalog (ADR 0002): one configured instance of an adapter — an endpoint, an adapter type, and opaque credential/egress references the provider phase resolves. Candidate selection skips a disabled backend.';
COMMENT ON COLUMN backends.adapter_type IS
    'Lowercase kebab token picking the driver (openai-compatible, anthropic, …). The set is open by design — a new provider is a row, not a migration — so this is a grammar check, not an enumeration.';
COMMENT ON COLUMN backends.credentials_ref IS
    'Opaque reference to the credentials this backend calls out with. Never interpreted here; nullable because some adapters need no credential. Where credentials live is the provider phase''s decision (issue #49).';
COMMENT ON COLUMN backends.egress_policy_ref IS
    'Opaque reference to the egress policy this backend''s calls route through. Never interpreted here; same decision record as credentials_ref (issue #49).';
COMMENT ON COLUMN backends.state IS
    'Lifecycle: active <-> disabled, reversible — disabling is operational, not terminal.';

-- ---------------------------------------------------------------------------
-- model_aliases — the only model identifier a client ever sees.
-- ---------------------------------------------------------------------------
CREATE TABLE model_aliases (
    id uuid PRIMARY KEY,
    name text NOT NULL
        CONSTRAINT model_aliases_name_grammar
        CHECK (name ~ '^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$'),
    state text NOT NULL
        CONSTRAINT model_aliases_state_valid
        CHECK (state IN ('active', 'retired')),
    max_output_tokens bigint NOT NULL
        CONSTRAINT model_aliases_max_output_tokens_positive
        CHECK (max_output_tokens > 0),
    reservation_cap bigint NOT NULL
        CONSTRAINT model_aliases_reservation_cap_positive
        CHECK (reservation_cap > 0),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    retired_at timestamptz,
    CONSTRAINT model_aliases_retirement_consistency
        CHECK ((state = 'retired') = (retired_at IS NOT NULL))
);

-- Total uniqueness, and the contrast with the control lane's only index of
-- this kind is the point: control.users' live-email index is partial (a
-- removed row releases its address), while an alias's name is never reused —
-- a retired alias keeps its name forever, because historical usage facts and
-- entitlement scopes must keep resolving to the same identity they named
-- (data-implications.md's uniqueness list). No WHERE clause is what makes
-- that true.
CREATE UNIQUE INDEX model_aliases_name_key
    ON model_aliases (name);

COMMENT ON TABLE model_aliases IS
    'Catalog (ADR 0002): the logical model name clients send, with the bounds admission enforces. Gateway-global, operator-managed; the runtime resolves it on every request. One aggregate with its candidates (ADR 0001, rule 3).';
COMMENT ON COLUMN model_aliases.name IS
    'The client-visible model identifier, matched exactly as registered. Total uniqueness across active and retired: names are never reused.';
COMMENT ON COLUMN model_aliases.max_output_tokens IS
    'The output limit admission validates client requests against.';
COMMENT ON COLUMN model_aliases.reservation_cap IS
    'The bound on admission-time hold sizing.';
COMMENT ON COLUMN model_aliases.state IS
    'Lifecycle: active -> retired, one-way. Admission to a retired alias is unknown_alias; the row stays as history.';
COMMENT ON COLUMN model_aliases.retired_at IS
    'Retirement instant; non-nil exactly when state = retired (model_aliases_retirement_consistency).';

-- ---------------------------------------------------------------------------
-- model_candidates — the alias's ordered fallback list (position 1 primary).
-- ---------------------------------------------------------------------------
CREATE TABLE model_candidates (
    id uuid PRIMARY KEY,
    alias_id uuid NOT NULL
        CONSTRAINT model_candidates_alias_id_fkey
        REFERENCES model_aliases (id)
        ON DELETE RESTRICT,
    position integer NOT NULL
        CONSTRAINT model_candidates_position_valid
        CHECK (position >= 1),
    backend_id uuid NOT NULL
        CONSTRAINT model_candidates_backend_id_fkey
        REFERENCES backends (id)
        ON DELETE RESTRICT,
    provider_model text NOT NULL
        CONSTRAINT model_candidates_provider_model_length
        CHECK (char_length(provider_model) BETWEEN 1 AND 256),
    parameter_overrides jsonb,
    created_at timestamptz NOT NULL,
    CONSTRAINT model_candidates_alias_position_key
        UNIQUE (alias_id, position),
    CONSTRAINT model_candidates_alias_target_key
        UNIQUE (alias_id, backend_id, provider_model)
);

-- The aggregate read is "one alias's candidates in position order"; this
-- index is the lookup that serves it, beside the two uniqueness constraints
-- the list's shape already requires.
CREATE INDEX model_candidates_alias_id_idx
    ON model_candidates (alias_id);

COMMENT ON TABLE model_candidates IS
    'Catalog (ADR 0002): one entry in an alias''s fallback order. Replaced wholesale by the alias''s aggregate transaction — the one DELETE path in this schema — because candidate rows are configuration, and attempts snapshot what they used.';
COMMENT ON COLUMN model_candidates.position IS
    '1-based fallback order; position 1 is primary. Positions are contiguous 1..n — enforced by the domain, which owns the list''s shape.';
COMMENT ON COLUMN model_candidates.backend_id IS
    'The backend this candidate calls, by id alone (ADR 0001, rule 3). RESTRICT: a backend with candidates on file cannot be orphaned.';
COMMENT ON COLUMN model_candidates.provider_model IS
    'The model identifier sent to the provider — the provider''s own vocabulary, not the alias''s.';
COMMENT ON COLUMN model_candidates.parameter_overrides IS
    'Optional per-candidate provider parameter overrides: opaque to the gateway, never queried, validated by the domain and passed through at execution. persistence.md''s sanctioned second JSONB category (issue #49).';

-- ---------------------------------------------------------------------------
-- alias_group_versions — immutable membership snapshots.
-- ---------------------------------------------------------------------------
CREATE TABLE alias_group_versions (
    id uuid PRIMARY KEY,
    group_name text NOT NULL
        CONSTRAINT alias_group_versions_group_name_grammar
        CHECK (group_name = '*'
               OR group_name ~ '^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$'),
    version integer NOT NULL
        CONSTRAINT alias_group_versions_version_valid
        CHECK (version >= 1),
    created_at timestamptz NOT NULL,
    CONSTRAINT alias_group_versions_group_version_key
        UNIQUE (group_name, version),
    -- The wildcard is one row, ever. Its membership is the empty set — every
    -- alias by definition — so there is nothing to re-version: editing a
    -- named group creates version n+1, while `*` has exactly this one. The
    -- CHECK pins the singleton's version at 1 so the row is fully
    -- determined; the partial unique index below is the mechanism.
    CONSTRAINT alias_group_versions_wildcard_version
        CHECK (group_name <> '*' OR version = 1)
);

CREATE UNIQUE INDEX alias_group_versions_wildcard_singleton
    ON alias_group_versions (group_name)
    WHERE group_name = '*';

COMMENT ON TABLE alias_group_versions IS
    'Catalog (ADR 0003): an immutable snapshot of a named alias group''s membership. Entitlements in the control database reference a version by id alone (ADR 0006 §7), so versions are never edited — a membership change opens a new version, and old grants keep the snapshot they were granted with.';
COMMENT ON COLUMN alias_group_versions.group_name IS
    'The group''s operator-facing name, or * — the reserved wildcard, whose snapshot contains every alias and which exists only as this one version-1 row.';
COMMENT ON COLUMN alias_group_versions.version IS
    '1-based, monotonic per group. The current version of a group is its highest; a roll pins whatever is highest at roll time.';

-- ---------------------------------------------------------------------------
-- alias_group_members — the snapshot itself.
-- ---------------------------------------------------------------------------
CREATE TABLE alias_group_members (
    group_version_id uuid NOT NULL
        CONSTRAINT alias_group_members_group_version_id_fkey
        REFERENCES alias_group_versions (id)
        ON DELETE RESTRICT,
    alias_id uuid NOT NULL
        CONSTRAINT alias_group_members_alias_id_fkey
        REFERENCES model_aliases (id)
        ON DELETE RESTRICT,
    CONSTRAINT alias_group_members_version_alias_key
        UNIQUE (group_version_id, alias_id)
);

-- Containment evaluation — "which stored snapshots contain alias a" — is the
-- lookup the admission waterfall runs against every entitlement scope; this
-- index is that lookup. The version-scoped read is served by the unique
-- constraint above it.
CREATE INDEX alias_group_members_alias_id_idx
    ON alias_group_members (alias_id);

COMMENT ON TABLE alias_group_members IS
    'Catalog (ADR 0003): one alias''s membership in one group version''s stored snapshot. Inserted at version creation and never touched again — immutability is the version''s point, and the wildcard version has no rows here at all.';
