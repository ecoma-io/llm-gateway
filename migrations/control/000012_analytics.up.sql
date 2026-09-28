-- Migrations lane: control — the Control Plane's `control` database
-- (ADR 0006 §7). This file lands the analytics read model's durable surface:
-- the account a usage fact belongs to, and the two time indexes the money
-- series reads need.
--
-- 000012 — analytics:
--
--   B16 is DERIVED STATE. Everything this migration makes durable is
--   reconstructable from state this plane already holds, and none of it is an
--   authority: the ledger is the money record, the settlement header is the
--   settlement of record, and a disagreement between an analytics figure and
--   either is resolved in the ledger's favour every time.
--
--   Two things in this migration are decisions, and both are worth the
--   sentences that follow them.
--
--   1. THE ACCOUNT RIDES THE FACT, ON A TABLE OF ITS OWN. Account-scoped
--      analytics needs an account on a usage fact, and no plane's schema
--      carries one: the Data Plane's usage_events has no account column
--      (migrations/dataplane/000003:437-460) and its public.requests table,
--      which does, has no cross-plane read path. The obvious place to put it
--      is control.settlements, and that is deliberately NOT this migration:
--      settlements is referenced by foreign key from applied_facts, is read by
--      two of B13's six reconciliation checks, and is the plane's most
--      consequential money record. Adding a column to it is a signature
--      change, and a signature change to the money record does not belong in
--      an analytics delivery. This table is the smaller, additive and
--      reversible shape, and it is exactly the shape the read model needs.
--
--   2. THE ACCOUNT IS NOT NULL. funding_buckets.account_id is nullable
--      (migrations/control/000006:130-131) because a subscription-funded
--      bucket names an entitlement instead, with the pair constrained by
--      funding_buckets_owner_xor. A read model that filtered on
--      funding_buckets.account_id would therefore SILENTLY DROP every
--      entitlement-funded row — a figure that is wrong in the direction that
--      looks like good news. The account is denormalized here as NOT NULL so
--      that a row without one cannot be written at all, and a scope that
--      cannot omit rows is a scope that cannot be wrong quietly.
--
--   The account here is DERIVED, never named by a request: the read model has
--   no account parameter, and the account on a fact is reached from the
--   funding buckets the fact's own allocation tail names. A request whose
--   fact draws on no bucket of the account belongs to no account, which is
--   what makes the scope a tenancy rule rather than a filter.
--
--   AND IT STARTS EMPTY, WITH NO BACKFILL, WHICH A READER MUST BE TOLD.
--   applied_facts holds every fact this plane has ever applied and this table
--   holds none of them: nothing populates it except the ingestion transaction
--   of a fact applied after this migration runs. So the day after 000012, a
--   report on an account that has been settling for months answers "no
--   derivations", and at the surface that answer is indistinguishable from a
--   brand-new account's — AccountHasDerivations is a statement about THIS
--   table, not about the account's history. There is no backfill here on
--   purpose and not by omission: a backfill is a second writer to a
--   Control-Plane table (docs/architecture/analytics.md §7), and re-deriving
--   the account of a past fact means re-reading an allocation tail whose
--   buckets may since have been deleted. The gap closes by time passing: the
--   difference between "no derivations" and "this plane was not recording yet"
--   is a question about the ledger, and the analytics surface has no settlement
--   read to answer it with.

-- ---------------------------------------------------------------------------
-- The account a usage fact belongs to, one row per (fact, account).
-- ---------------------------------------------------------------------------
CREATE TABLE control.analytics_fact_dimensions (
    request_id text NOT NULL
        CONSTRAINT analytics_fact_dimensions_request_id_grammar
        CHECK (char_length(request_id) BETWEEN 1 AND 256),
    -- The fact's idempotency class, carried verbatim rather than derived:
    -- applied_facts owns the pairing (its kind_class_pairing CHECK is what
    -- makes a settlement's kind fileable under the orphan class impossible),
    -- and duplicating that rule here would be a second copy of a promise
    -- this plane keeps in one place.
    kind_class text NOT NULL
        CONSTRAINT analytics_fact_dimensions_kind_class_valid
        CHECK (kind_class IN ('settlement', 'unbillable_orphaned')),
    -- NOT NULL, and the reason is the paragraph above: a scope that can
    -- omit a row is a scope that can be wrong without saying so. The
    -- reference is to control.accounts rather than to a bucket, because a
    -- fact is priced against buckets and an account OWNS buckets —
    -- directly for a PAYG balance, and through a subscription for an
    -- entitlement cycle (control.entitlements has no account column of its
    -- own; migrations/control/000003:311-347).
    account_id uuid NOT NULL
        CONSTRAINT analytics_fact_dimensions_account_id_fkey
        REFERENCES control.accounts (id),
    append_seq bigint NOT NULL
        CONSTRAINT analytics_fact_dimensions_append_seq_positive
        CHECK (append_seq >= 1),
    -- The disposition axis: when this plane recorded the derivation. Copied
    -- from applied_facts.applied_at rather than joined to it, and the copy
    -- is exact by construction rather than by convention — both columns
    -- default to now(), which PostgreSQL defines as transaction_timestamp(),
    -- and this row is written inside the same transaction as the applied
    -- fact it belongs to. So the two can never disagree, and the read model
    -- scopes a range over this table alone instead of joining a second one
    -- to obtain an instant it could have had.
    applied_at timestamptz NOT NULL,

    -- The same exactly-once boundary applied_facts enforces, on the same key
    -- PLUS THE ACCOUNT — and the plus is the whole reason this key is not the
    -- applied-facts key. A settlement's allocation tail can draw on buckets
    -- belonging to more than one account (the waterfall plans per bucket, and
    -- nothing in funding_buckets_owner_xor prevents a tail naming a PAYG bucket
    -- of one account and an entitlement bucket of another; the XOR constrains a
    -- BUCKET's owner, not a FACT's set of them). A key of (request_id,
    -- kind_class) would record the first account and silently drop the rest
    -- through ON CONFLICT DO NOTHING, and the dropped account's report would be
    -- short by exactly the requests it funded — a figure wrong in the direction
    -- that looks like good news. So the key is per (fact, account), and a
    -- redelivery still converges to a no-op exactly as it does in applied_facts.
    CONSTRAINT analytics_fact_dimensions_pkey
        PRIMARY KEY (request_id, kind_class, account_id)
);

COMMENT ON TABLE control.analytics_fact_dimensions IS
    'Analytics (B16): the account one applied usage fact belongs to, denormalized so the read model can scope to an account at all. Derived state — reconstructable from the fact''s own allocation tail joined to control.funding_buckets, and an authority for nothing. Populated inside the ingestion transaction on txCtx, in the same unit of work as the settlement or the disposition the fact produced, so a fact and the account it belongs to cannot be committed apart. Created EMPTY and never backfilled: only facts applied after this migration carry a row, so a report asks about the account''s derivations SINCE 000012 and not about its history.';
COMMENT ON COLUMN control.analytics_fact_dimensions.account_id IS
    'The account whose funding buckets the fact''s allocation tail names, resolved at apply time. Not a request parameter and not a caller-supplied value: the read model has no account filter a caller could tamper with, because the scope is applied in the statement rather than after it. NOT NULL because funding_buckets.account_id is nullable, and a read scoped through that column would silently drop every entitlement-funded fact.';
COMMENT ON COLUMN control.analytics_fact_dimensions.applied_at IS
    'When this plane recorded the derivation, equal to applied_facts.applied_at by construction: both default to now(), PostgreSQL defines now() as transaction_timestamp(), and both rows are written inside the same transaction. It is the bucket axis for the request counts — NOT occurred_at, which is the runtime''s clock and never an ordering key (domain/ingestion/fact.go:93-95).';

-- The reader's only access path, and the one the surface''s bounds are stated
-- against: (account, when this plane applied the fact) in that order, because
-- the scope is the account and the range is the predicate that narrows it.
--
-- Plain CREATE INDEX, never CONCURRENTLY, for the same reason
-- applied_facts_applied_at_idx gives (migrations/control/000009:304-306): this
-- lane's transaction rule is one migration per transaction, and a CONCURRENTLY
-- index cannot run inside one.
CREATE INDEX analytics_fact_dimensions_account_applied_idx
    ON control.analytics_fact_dimensions (account_id, applied_at);

-- ---------------------------------------------------------------------------
-- The two money axes.
-- ---------------------------------------------------------------------------
-- Settled and released and funds-added are bucketed by the DATABASE clock at
-- the instant the row was booked, never by the fact's occurred_at: that is the
-- runtime's clock, clocks disagree between processes, and ordering one plane's
-- records by another plane's timestamps is how a modest skew becomes a fact
-- filed outside the window it belongs to (the rule B13 applied to its own
-- window bound, application/reconciliation.go:567-573).
--
-- These two indexes are not an optimization — they are the difference between
-- a working surface and a dead one. Verified across every migration in this
-- lane: settlements carried no index at all beyond its primary key and its
-- request_id unique, and ledger_entries' only indexes are the three partial
-- uniques on (funding_bucket_id, ...). Both created_at columns were unindexed,
-- so a 90-day money series was a sequential scan of the entire ledger, and
-- because the range bound is a constant 90 days while the table grows without
-- bound, report latency would have grown without limit against a fixed
-- statement_timeout — failing closed forever, which is safe and useless.
--
-- The claim is measured, not asserted. On a migrated database seeded with eight
-- years of history (203,000 settlements, 406,000 ledger entries, oldest row
-- 2018-10-02), the 90-day reads are driven by these indexes:
--
--   settled:   Index Scan using settlements_created_at_idx
--              Index Cond: (created_at >= now() - interval '90 days' AND
--                           created_at < now())                        1.8ms
--   ledger:    Bitmap Index Scan on ledger_entries_created_at_idx
--              Index Cond: (the same range)                            2.3ms
--
-- Dropping both inside a transaction and re-running the identical statements
-- turns each into a Parallel Seq Scan over the whole table — 17.2ms and 27.3ms
-- respectively, 9x and 12x — and the rollback restores both indexes. A plan
-- line naming an index shows the planner accepted it; the reversal is what
-- shows it was doing the work. Both figures are on a warm cache and a laptop,
-- so the absolute numbers are not the point: the ratio is, and it grows with the
-- table while the timeout does not.
--
-- A leading column of created_at rather than a composite with a scope the
-- reader may not have: the money tables carry no account column (see the
-- header), so scoping them is a join through the fact dimensions, and the
-- index that can serve a range is the one whose leading column is the range.
CREATE INDEX settlements_created_at_idx
    ON control.settlements (created_at);

CREATE INDEX ledger_entries_created_at_idx
    ON control.ledger_entries (created_at);
