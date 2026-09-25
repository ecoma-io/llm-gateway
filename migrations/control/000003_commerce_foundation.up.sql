-- Migrations lane: control — the Control Plane's `control` database
-- (ADR 0006 §7). The lane's second business schema: everything commerce, the
-- third link of ADR 0005's ordering — identity → catalog → commerce →
-- accounting. Catalog itself landed in the Data Plane's lane (the catalog
-- rows live in the `dataplane` database), which shapes two rules below: a
-- foreign key into that database cannot exist, so commerce references
-- catalog's immutable alias-group versions by identifier alone, and nothing
-- here copies catalog state (model names, membership) into `control`.
--
-- 000003 — commerce foundation (ADR 0003; issue #58):
--
--   plans                    the commercial product's identity root: a name,
--                            nothing else. A plan grants nothing by itself;
--                            every commercial term lives on an immutable
--                            version.
--   plan_versions            the immutable, monotonic version of a plan:
--                            recurring price, billing period, lifecycle
--                            draft → published → retired. Drafts are
--                            editable; published versions are immutable;
--                            retiring stops new subscriptions and rewrites
--                            nothing. Subscriptions pin a version forever —
--                            this table is why "forever" is reconstructible.
--   plan_grant_definitions   one version's grants: a stable grant-definition
--                            id, the alias-group NAME the grant is scoped to
--                            (not a version — the roll resolves whatever
--                            version of that group is current at roll time,
--                            ADR 0003), a dimension (cost is the only one
--                            defined) and a granted amount. One definition
--                            per (scope, dimension) per version.
--   subscriptions            one account's instantiation of one plan version,
--                            pinned forever. ADR 0003's lifecycle
--                            pending | active | suspended | cancelled |
--                            expired, with scheduled cancellation as data
--                            (cancel_at + cancellation_mode), cycle fields
--                            null only while pending, and no current_plan
--                            anywhere — same-plan double subscriptions are
--                            first-class and never merged.
--   entitlements             one live grant materialised from exactly one
--                            (subscription, cycle, grant definition), pinning
--                            the alias-group VERSION the roll resolved for
--                            that cycle. Immutable after the roll except the
--                            terminal active → expired flip: no capacity or
--                            consumption column lives here — capacity is
--                            Accounting's buckets (B6) and the runtime's
--                            quota projections, and this table must never
--                            become a third copy of it.
--   account_payg             the PAYG enablement flag as a Commerce-owned
--                            row keyed by the account (ADR 0003: "the flag is
--                            this context's"), plus the funding-bucket
--                            reference ADR 0001 rule 5 assigns to Accounting.
--                            The bucket table arrives with B6; the reference
--                            is nullable until the account-creation
--                            choreography assigns it, and the absent row
--                            means PAYG is disabled. This row is the
--                            one-PAYG-source-per-account invariant, ever.
--
-- Conventions held across the lane, and the three places this file departs
-- from them on purpose:
--   * explicit names on every constraint and index, so a future migration can
--     name what it alters and error messages name what fired — the repos map
--     constraint names to domain sentinels, because one insert into these
--     tables can lose to several different business rules;
--   * lifecycle states are checked text, spelled exactly as the domain spells
--     them, one-way exits included;
--   * timestamptz everywhere, NOT NULL. Created/updated stamps remain
--     application-supplied, and every application writer in this domain
--     mints them UTC. THE CYCLE BOUNDS ARE THE ONE EXCEPTION: start_at,
--     period_start and period_end decide cycle membership, and ADR 0003
--     decides membership by the database clock — the roll transaction reads
--     transaction_timestamp() inside itself and authors these bounds from
--     that instant, and the due-work scans compare against
--     transaction_timestamp() directly. No gateway node's clock authors a
--     boundary a boundary test is judged by.
--   * no DELETE path and no ON DELETE CASCADE anywhere: subscriptions,
--     entitlements and their definitions are history immutable accounting
--     rows reference; the foreign keys below are RESTRICT by default.
--   * every commerce-minted id is an RFC 4122 version-7 UUID, and the CHECKs
--     below pin the v7 version and variant nibbles the way the identity
--     migration pins the api-key prefix's v4 form (persistence.md: v7 for any
--     id that crosses a plane or reaches a client — subscriptions and
--     entitlements travel in grants and usage facts). Account ids remain the
--     identity phase's v4; that recorded deviation belongs to this lane's
--     documentation, not to a silent exception here.
--
-- This file carries no BEGIN, COMMIT or ROLLBACK of its own, per
-- migrations/README.md: the runner delivers it as one simple query — one
-- implicit transaction — and a file-managed transaction would replace that
-- guarantee, not add to it.
--
-- The inverse of this file is 000003_commerce_foundation.down.sql, in
-- reverse creation order, no CASCADE.

-- ---------------------------------------------------------------------------
-- plans — the commercial product's identity root.
-- ---------------------------------------------------------------------------
CREATE TABLE control.plans (
    id uuid PRIMARY KEY
        CONSTRAINT plans_id_uuid_v7
        CHECK (id::text ~ '^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'),
    name text NOT NULL
        CONSTRAINT plans_name_length
        CHECK (char_length(name) BETWEEN 1 AND 256),
    created_at timestamptz NOT NULL,
    CONSTRAINT plans_name_key
        UNIQUE (name)
);

COMMENT ON TABLE control.plans IS
    'Commerce (ADR 0003): the plan identity root. Every commercial term lives on an immutable plan_versions row; this row never changes after creation, so it carries no updated_at.';
COMMENT ON COLUMN control.plans.name IS
    'Operator-facing plan name, unique across plans. Renaming a plan is not defined; a new plan is created instead.';

-- ---------------------------------------------------------------------------
-- plan_versions — the immutable commercial template.
-- ---------------------------------------------------------------------------
CREATE TABLE control.plan_versions (
    id uuid PRIMARY KEY
        CONSTRAINT plan_versions_id_uuid_v7
        CHECK (id::text ~ '^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'),
    plan_id uuid NOT NULL
        CONSTRAINT plan_versions_plan_id_fkey
        REFERENCES control.plans (id),
    version_number integer NOT NULL
        CONSTRAINT plan_versions_version_number_valid
        CHECK (version_number >= 1),
    period text NOT NULL
        CONSTRAINT plan_versions_period_valid
        CHECK (period IN ('calendar_month')),
    recurring_price_minor_units bigint NOT NULL
        CONSTRAINT plan_versions_price_non_negative
        CHECK (recurring_price_minor_units >= 0),
    state text NOT NULL
        CONSTRAINT plan_versions_state_valid
        CHECK (state IN ('draft', 'published', 'retired')),
    published_at timestamptz,
    retired_at timestamptz,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    CONSTRAINT plan_versions_version_number_key
        UNIQUE (plan_id, version_number),
    CONSTRAINT plan_versions_state_stamps
        CHECK (
            (state = 'draft' AND published_at IS NULL AND retired_at IS NULL)
            OR (state = 'published' AND published_at IS NOT NULL AND retired_at IS NULL)
            OR (state = 'retired' AND published_at IS NOT NULL AND retired_at IS NOT NULL)
        )
);

-- A subscription may only pin a published version: drafts are editable and a
-- live commercial state must never reinterpret under an edit; retired
-- versions take no new subscriptions either. The guard lives in the
-- subscribing use case, not in a constraint — a foreign key cannot filter on
-- another row's state, and the window between a version's retirement and a
-- racing subscribe is a refused sale, not a corrupted one.
COMMENT ON TABLE control.plan_versions IS
    'Commerce (ADR 0003): one immutable version of a plan. Published versions are never edited; retiring stops new subscriptions and rewrites nothing. Subscriptions pin a version row forever.';
COMMENT ON COLUMN control.plan_versions.version_number IS
    'Monotonic per plan, 1-based. The (plan_id, version_number) pair is unique; the subscribing use case enforces that a new version outnumbers the current one.';
COMMENT ON COLUMN control.plan_versions.period IS
    'Billing period shape: calendar_month — a period is a calendar month anchored at the subscription''s start_at, day-of-month clipped at month end. Other periods are named future extensions (ADR 0003).';
COMMENT ON COLUMN control.plan_versions.recurring_price_minor_units IS
    'The plan''s recurring price in the settlement currency''s minor units — never a float. Informational in v1: renewals are operator-administered outside the ledger until the revenue concept is designed (see docs/architecture/commerce.md).';
COMMENT ON COLUMN control.plan_versions.state IS
    'Lifecycle: draft -> published -> retired. Published is immutable; retired is terminal and stops new subscriptions only.';
COMMENT ON COLUMN control.plan_versions.published_at IS
    'Publication instant; non-null exactly when state is published or retired (plan_versions_state_stamps).';
COMMENT ON COLUMN control.plan_versions.retired_at IS
    'Retirement instant; non-null exactly when state is retired (plan_versions_state_stamps).';

-- ---------------------------------------------------------------------------
-- plan_grant_definitions — one version's grants.
-- ---------------------------------------------------------------------------
CREATE TABLE control.plan_grant_definitions (
    id uuid PRIMARY KEY
        CONSTRAINT plan_grant_definitions_id_uuid_v7
        CHECK (id::text ~ '^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'),
    plan_version_id uuid NOT NULL
        CONSTRAINT plan_grant_definitions_plan_version_id_fkey
        REFERENCES control.plan_versions (id),
    alias_group_name text NOT NULL
        CONSTRAINT plan_grant_definitions_group_name_grammar
        CHECK (alias_group_name = '*'
               OR alias_group_name ~ '^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$'),
    dimension text NOT NULL
        CONSTRAINT plan_grant_definitions_dimension_valid
        CHECK (dimension IN ('cost')),
    granted_amount bigint NOT NULL
        CONSTRAINT plan_grant_definitions_amount_positive
        CHECK (granted_amount > 0),
    created_at timestamptz NOT NULL,
    CONSTRAINT plan_grant_definitions_scope_dimension_key
        UNIQUE (plan_version_id, alias_group_name, dimension)
);

-- The definition pins the group NAME, not a version: each cycle roll resolves
-- whatever version of the named group is current at that roll and pins the
-- resolved version on the entitlement it creates (ADR 0003). The name's
-- grammar is catalog's own alias_group_versions_group_name_grammar verbatim —
-- the wildcard `*` included — so a definition can never name a group the
-- catalog would refuse to have. Whether the named group (and version) exists
-- is validated at draft time through the Control → Data management read; this
-- database cannot foreign-key into the `dataplane` database, and no shadow
-- copy of catalog state is kept here to fake one.
COMMENT ON TABLE control.plan_grant_definitions IS
    'Commerce (ADR 0003): one grant of a plan version — a stable id, the alias-group name it is scoped to, a dimension and an amount. Immutable with its version once published.';
COMMENT ON COLUMN control.plan_grant_definitions.alias_group_name IS
    'The catalog alias group the grant is scoped to, or * — every alias. A version id is NOT stored here: each roll pins the group''s current version on the entitlement.';
COMMENT ON COLUMN control.plan_grant_definitions.dimension IS
    'Entitlement dimension: cost is the only defined one (ledger currency''s minor units); token/request dimensions are named future extensions, never implicit conversions.';
COMMENT ON COLUMN control.plan_grant_definitions.granted_amount IS
    'Amount granted per cycle, in the dimension''s minor units. Strictly positive — a zero grant purchases nothing.';

-- ---------------------------------------------------------------------------
-- subscriptions — one account's instantiation of one plan version.
-- ---------------------------------------------------------------------------
CREATE TABLE control.subscriptions (
    id uuid PRIMARY KEY
        CONSTRAINT subscriptions_id_uuid_v7
        CHECK (id::text ~ '^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'),
    account_id uuid NOT NULL
        CONSTRAINT subscriptions_account_id_fkey
        REFERENCES control.accounts (id),
    plan_version_id uuid NOT NULL
        CONSTRAINT subscriptions_plan_version_id_fkey
        REFERENCES control.plan_versions (id),
    state text NOT NULL
        CONSTRAINT subscriptions_state_valid
        CHECK (state IN ('pending', 'active', 'suspended', 'cancelled', 'expired')),
    start_at timestamptz NOT NULL,
    renewal_enabled boolean NOT NULL,
    cancel_at timestamptz,
    cancellation_mode text
        CONSTRAINT subscriptions_cancellation_mode_valid
        CHECK (cancellation_mode IN ('scheduled', 'immediate')),
    cycle_number integer
        CONSTRAINT subscriptions_cycle_number_valid
        CHECK (cycle_number >= 1),
    period_start timestamptz,
    period_end timestamptz,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    CONSTRAINT subscriptions_cancellation_consistency
        CHECK ((cancel_at IS NULL) = (cancellation_mode IS NULL)),
    CONSTRAINT subscriptions_pending_has_no_cycle
        CHECK (
            state <> 'pending'
            OR (cycle_number IS NULL AND period_start IS NULL AND period_end IS NULL)
        ),
    CONSTRAINT subscriptions_rolled_have_cycle
        CHECK (
            state NOT IN ('active', 'suspended', 'expired')
            OR (cycle_number IS NOT NULL AND period_start IS NOT NULL AND period_end IS NOT NULL)
        ),
    CONSTRAINT subscriptions_period_bounds
        CHECK (period_start IS NULL OR period_end IS NULL OR period_end > period_start)
);

-- Due-work scans and the waterfall's tie-breaks are the lookups these serve:
-- the pending scan promotes subscriptions whose start_at the database clock
-- has passed; the active scan finds cycles to roll and natural ends to
-- expire; created_at is the waterfall's third key (oldest subscription
-- first) and is never rewritten.
CREATE INDEX subscriptions_account_id_idx ON control.subscriptions (account_id);
CREATE INDEX subscriptions_pending_due_idx
    ON control.subscriptions (start_at)
    WHERE state = 'pending';
CREATE INDEX subscriptions_active_period_end_idx
    ON control.subscriptions (period_end)
    WHERE state = 'active';

COMMENT ON TABLE control.subscriptions IS
    'Commerce (ADR 0003): one account''s pinning of one plan version, with its own cycle identity. Any number may be active concurrently — including two of the same version; they never merge, and no account carries a current_plan anywhere.';
COMMENT ON COLUMN control.subscriptions.state IS
    'Lifecycle: pending -> active -> suspended -> active | cancelled | expired. Cancelled and expired are terminal; scheduled cancellation is data (cancel_at + cancellation_mode), not a state.';
COMMENT ON COLUMN control.subscriptions.start_at IS
    'The instant the subscription becomes due for its first cycle: the pending scan promotes it and rolls cycle 1 once start_at <= transaction_timestamp(). Authored by the creating transaction''s clock, like every other application stamp.';
COMMENT ON COLUMN control.subscriptions.renewal_enabled IS
    'Whether the subscription rolls into further cycles. Gates rolls after the first — a fixed-term subscription is born renewal_enabled = false and still receives cycle 1.';
COMMENT ON COLUMN control.subscriptions.cancel_at IS
    'Scheduled cancellation instant. Non-null exactly when cancellation_mode is (subscriptions_cancellation_consistency). The subscription stays active and usable until it passes; rolls are suppressed past it.';
COMMENT ON COLUMN control.subscriptions.cancellation_mode IS
    'scheduled: cancel_at was set ahead of time; immediate: the cancellation took effect at once. Both end in the same terminal state.';
COMMENT ON COLUMN control.subscriptions.cycle_number IS
    'The current grant cycle, starting at 1 on the first roll. Null only while pending; never reused (subscriptions_pending_has_no_cycle, subscriptions_rolled_have_cycle).';
COMMENT ON COLUMN control.subscriptions.period_start IS
    'The current cycle''s start, authored by the roll transaction from transaction_timestamp(). Null only while pending.';
COMMENT ON COLUMN control.subscriptions.period_end IS
    'The current cycle''s end — a calendar month after period_start, day clipped at month end. Null only while pending. Capacity stops being available to new admissions at this instant.';

-- ---------------------------------------------------------------------------
-- entitlements — one live grant per (subscription, cycle, definition).
-- ---------------------------------------------------------------------------
CREATE TABLE control.entitlements (
    id uuid PRIMARY KEY
        CONSTRAINT entitlements_id_uuid_v7
        CHECK (id::text ~ '^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'),
    subscription_id uuid NOT NULL
        CONSTRAINT entitlements_subscription_id_fkey
        REFERENCES control.subscriptions (id),
    cycle_number integer NOT NULL
        CONSTRAINT entitlements_cycle_number_valid
        CHECK (cycle_number >= 1),
    grant_definition_id uuid NOT NULL
        CONSTRAINT entitlements_grant_definition_id_fkey
        REFERENCES control.plan_grant_definitions (id),
    alias_group_version_id uuid NOT NULL
        CONSTRAINT entitlements_alias_group_version_uuid_v7
        CHECK (alias_group_version_id::text ~ '^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'),
    dimension text NOT NULL
        CONSTRAINT entitlements_dimension_valid
        CHECK (dimension IN ('cost')),
    granted_amount bigint NOT NULL
        CONSTRAINT entitlements_amount_positive
        CHECK (granted_amount > 0),
    state text NOT NULL
        CONSTRAINT entitlements_state_valid
        CHECK (state IN ('active', 'expired')),
    period_start timestamptz NOT NULL,
    period_end timestamptz NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    CONSTRAINT entitlements_period_bounds
        CHECK (period_end > period_start),
    CONSTRAINT entitlements_grant_once_per_cycle
        UNIQUE (subscription_id, cycle_number, grant_definition_id)
);

-- The expiry scan retires grants whose cycle the database clock has passed;
-- the waterfall's reads are covered by the uniqueness index above (its
-- leading columns are exactly a cycle's identity).
CREATE INDEX entitlements_expiry_scan_idx
    ON control.entitlements (period_end)
    WHERE state = 'active';

COMMENT ON TABLE control.entitlements IS
    'Commerce (ADR 0003): the live grant a cycle roll materialises from one grant definition. The (subscription, cycle, definition) triple is unique, so a retried roll cannot grant a cycle twice. Retained forever — immutable accounting references these rows.';
COMMENT ON COLUMN control.entitlements.alias_group_version_id IS
    'The catalog alias-group VERSION this grant is scoped to, pinned by the roll that created it — owned by the Data Plane''s catalog, referenced by id alone (ADR 0006 §7); the v7 form is checked, its existence is not (no cross-database foreign key can exist). Purchased scope never changes retroactively.';
COMMENT ON COLUMN control.entitlements.dimension IS
    'Entitlement dimension, copied from the grant definition at the roll: cost, in the ledger currency''s minor units.';
COMMENT ON COLUMN control.entitlements.granted_amount IS
    'Amount granted for this cycle, copied from the grant definition at the roll. No available or consumed column exists here: capacity is drawn down in Accounting''s buckets and the runtime''s quota projections, never in this table.';
COMMENT ON COLUMN control.entitlements.state IS
    'active or expired, nothing else — suspension is a subscription state applied at admission, never a per-grant state. Expiry is terminal and flips from active only.';
COMMENT ON COLUMN control.entitlements.period_start IS
    'This cycle''s bounds, snapshotted from the subscription at the roll. Stored here because the subscription''s own fields advance to the next cycle — history must stay reconstructible without them.';
COMMENT ON COLUMN control.entitlements.period_end IS
    'This cycle''s end: capacity stops being available to new admissions here, while allocations reserved before it stay settlement-eligible (ADR 0003).';

-- ---------------------------------------------------------------------------
-- account_payg — the PAYG enablement flag, Commerce-owned.
-- ---------------------------------------------------------------------------
CREATE TABLE control.account_payg (
    account_id uuid PRIMARY KEY
        CONSTRAINT account_payg_account_id_fkey
        REFERENCES control.accounts (id),
    enabled boolean NOT NULL,
    funding_bucket_id uuid
        CONSTRAINT account_payg_funding_bucket_uuid_v7
        CHECK (funding_bucket_id::text ~ '^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

-- One row per account, ever: the primary key IS the invariant. The row is
-- created the first time PAYG is enabled for the account; no row means the
-- account has never enabled PAYG. This is Commerce's decision (ADR 0003: the
-- flag is this context's) — deliberately not a column on control.accounts,
-- which is Identity's aggregate and carries no billing state by design. The
-- bucket it enables is Accounting's aggregate: funding_bucket_id holds the
-- bucket's identifier under ADR 0001 rule 5, nullable until B6's
-- account-creation choreography assigns it, and the v7 form is pinned now so
-- the reference discipline is fixed before the first bucket exists.
COMMENT ON TABLE control.account_payg IS
    'Commerce (ADR 0003): the per-account PAYG enablement flag and its Accounting bucket reference (ADR 0001 rule 5). Enabling authorises spending and funds nothing; no row means never enabled. One row per account, ever.';
COMMENT ON COLUMN control.account_payg.enabled IS
    'Whether new admissions may spill to PAYG. Disabling blocks new spills; holds already secured settle normally.';
COMMENT ON COLUMN control.account_payg.funding_bucket_id IS
    'The account''s PAYG funding bucket — Accounting''s aggregate, referenced by identifier. Null until the account-creation choreography (B6) assigns it; never reassigned.';
