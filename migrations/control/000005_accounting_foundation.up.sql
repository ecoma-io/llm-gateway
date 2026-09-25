-- Migrations lane: control — the Control Plane's `control` database
-- (ADR 0006 §7). This file is the lane's fourth business schema: Accounting,
-- the money half of the Control Plane (ADR 0004) — the funding buckets whose
-- cached balances are authoritative, the settlements of record that are the
-- exactly-once boundary, and the append-only ledger legs every movement of
-- money is written as.
--
-- 000005 — accounting foundation (ADR 0004, as amended by ADR 0006;
-- ADR 0001 rule 5):
--
--   funding_buckets   the authoritative, lockable capacity projection for
--                     exactly one entitlement cycle or one account PAYG
--                     balance. Exactly one owner: an entitlement's cycle
--                     bucket or an account's PAYG bucket, never both, never
--                     neither. The row caches the three balance projections
--                     beside a version marker and a per-bucket leg counter,
--                     and the cache is maintained inside the same
--                     transaction that writes the legs — it is a concurrency
--                     control projection, rebuildable from the ledger and
--                     never an independently editable balance (ADR 0004).
--   settlements       the settlement of record: one per request, ever.
--                     Unique by request_id, whose uniqueness IS the
--                     exactly-once boundary (invariant 4); settled_total is
--                     written once, equal to the sum of the settlement's
--                     consume legs. No state column and no currency column:
--                     the legs are the state, and the single platform-wide
--                     settlement currency is configuration (ADR 0004's
--                     recorded consequence), not a per-row lookup.
--   ledger_entries    one immutable bucket leg per movement of money. The
--                     kind names the movement and the two signed delta
--                     columns state exactly how it moves the bucket's two
--                     balances — consume subtracts from both — and a CHECK
--                     pins the whole algebra, kind by kind, so a row whose
--                     deltas contradict its kind cannot exist even by raw
--                     SQL. The price snapshot is carried by value on consume
--                     legs and is null everywhere else; reservation and
--                     settlement references are by identifier alone, because
--                     no foreign key can cross the plane boundary (ADR 0006
--                     §7).
--
-- Three engine guards ride with the tables, because append-only and
-- write-once are schema promises here, not application discipline:
--
--   * ledger_entries and settlements refuse UPDATE and DELETE outright — a
--     correction is a new adjustment leg referencing the original entry, and
--     the ledger is history in the strongest sense the database can state;
--   * account_payg.funding_bucket_id (B5's nullable reference) becomes a
--     real foreign key into funding_buckets and is enforced write-once: the
--     reference lands once and is never re-pointed, because a changed
--     reference would silently split an account's prepaid money across two
--     buckets. The statement-level guard the commerce adapter already
--     carries (the upsert's WHERE clause) is belt; this trigger is braces —
--     no future writer can re-point the reference any more than it can
--     rewrite the ledger.
--
-- Conventions held across the lane, kept here:
--   * explicit names on every constraint, index and trigger, so error
--     messages name what fired — one insert into ledger_entries can lose to
--     several different business rules, and the adapters map constraint
--     names to domain sentinels;
--   * timestamptz everywhere, application-supplied, NOT NULL;
--   * lifecycle states are checked text spelled exactly as the domain
--     spells them;
--   * no DELETE path and no ON DELETE CASCADE anywhere: the ledger and the
--     settlements are history, and the buckets are the rows history points
--     at;
--   * every accounting-minted id is an RFC 9562 version-7 UUID, CHECKed on
--     the version and variant nibbles exactly as commerce's are. The two
--     blind references are the deliberate exceptions the plane boundary
--     forces: entitlement ids are v7-checked (commerce mints them as v7)
--     but referenced without a foreign key, and account ids carry no
--     grammar CHECK at all — identity mints accounts as version-4 uuids,
--     the recorded deviation this lane documents rather than repeats, and
--     the reservation_id column carries no grammar either because the
--     Data Plane owns that identifier's grammar and has not published it
--     yet; the uuid type's own form check is all this plane asserts.
--
-- A note on the balance CHECKs on purpose: held_amount and available_amount
-- are pinned non-negative, and settled_amount is pinned TRANSITIVELY — the
-- projection equality makes settled = available + held, so no leg, not even
-- an operator `adjustment`, can drive it below zero while the other two hold.
-- That is the spec's no-credit rule made structural: corrections fix records
-- through new ledger entries, they never overdraw or credit an account
-- (ADR 0004). A dedicated settled CHECK would be redundant with the
-- projection; what matters is that no path in the algebra reaches a negative
-- balance at all.
--
-- The file carries no BEGIN, COMMIT or ROLLBACK of its own, per
-- migrations/README.md: the runner delivers it as one simple query — one
-- implicit transaction — and the migration fails or applies whole. The same
-- rule shapes the plpgsql bodies below: each opens its block on the AS line
-- (`AS $name$ BEGIN`), because a line that opens with BEGIN would read as
-- file-managed transaction control to verify.sh's lane-drift scan — the
-- bodies carry none.
--
-- The inverse of this file is 000005_accounting_foundation.down.sql, in
-- reverse creation order, no CASCADE.

-- ---------------------------------------------------------------------------
-- funding_buckets — the authoritative capacity projection.
-- ---------------------------------------------------------------------------
CREATE TABLE control.funding_buckets (
    id uuid PRIMARY KEY
        CONSTRAINT funding_buckets_id_uuid_v7
        CHECK (id::text ~ '^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'),
    entitlement_id uuid
        CONSTRAINT funding_buckets_entitlement_uuid_v7
        CHECK (entitlement_id::text ~ '^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$')
        CONSTRAINT funding_buckets_entitlement_id_fkey
        REFERENCES control.entitlements (id),
    account_id uuid
        CONSTRAINT funding_buckets_account_id_fkey
        REFERENCES control.accounts (id),
    status text NOT NULL
        CONSTRAINT funding_buckets_status_valid
        CHECK (status IN ('active', 'closed')),
    version bigint NOT NULL
        CONSTRAINT funding_buckets_version_nonnegative
        CHECK (version >= 0),
    last_sequence bigint NOT NULL
        CONSTRAINT funding_buckets_last_sequence_nonnegative
        CHECK (last_sequence >= 0),
    settled_amount bigint NOT NULL,
    held_amount bigint NOT NULL,
    available_amount bigint NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    CONSTRAINT funding_buckets_owner_xor
        CHECK ((entitlement_id IS NULL) <> (account_id IS NULL)),
    CONSTRAINT funding_buckets_balance_projection
        CHECK (
            available_amount = settled_amount - held_amount
            AND held_amount >= 0
            AND available_amount >= 0
        ),
    CONSTRAINT funding_buckets_entitlement_id_key UNIQUE (entitlement_id),
    CONSTRAINT funding_buckets_account_id_key UNIQUE (account_id)
);

COMMENT ON TABLE control.funding_buckets IS
    'Accounting (ADR 0004): the authoritative capacity projection for exactly one entitlement cycle or one account PAYG balance, and the source of truth for money. The cached balances ride every leg append in the same transaction; they are a concurrency control projection, rebuildable from the ledger, never an independently editable balance.';
COMMENT ON COLUMN control.funding_buckets.entitlement_id IS
    'The commerce entitlement this cycle bucket funds — exactly one owner per bucket (funding_buckets_owner_xor), unique, so an entitlement can never grow a second bucket. Referenced by identifier: the row lives in this same database, but the owner is Commerce''s aggregate, not Accounting''s.';
COMMENT ON COLUMN control.funding_buckets.account_id IS
    'The account this PAYG bucket funds — the bucket control.account_payg references. Identity mints account ids as version-4 uuids, so this column carries no v7 CHECK: the uuid type''s form check is the whole grammar this plane asserts about another context''s identifier.';
COMMENT ON COLUMN control.funding_buckets.status IS
    'active or closed. Closing is administrative and refuses an open hold — closing a bucket with money held against it would strand the held amount. A closed bucket takes no new legs.';
COMMENT ON COLUMN control.funding_buckets.version IS
    'Optimistic-concurrency marker on the cached projection: every guarded balance move bumps it. It exists so a reader can tell a stale read from a fresh one without holding a lock.';
COMMENT ON COLUMN control.funding_buckets.last_sequence IS
    'The highest ledger sequence allocated for this bucket. Leg appends allocate `last_sequence + 1` by UPDATE ... RETURNING inside the writer''s own transaction — the row lock held to commit serialises a bucket''s writers, which makes sequence order equal commit order for that bucket''s history.';
COMMENT ON COLUMN control.funding_buckets.settled_amount IS
    'Cached Σ grants/topups − Σ consumes + Σ adjustment settled_deltas. Non-negative without a dedicated CHECK: the projection equality makes settled = available + held, and the adjustment guard refuses any move that would drive settled below zero — corrections never credit an account (ADR 0004''s no-credit rule).';
COMMENT ON COLUMN control.funding_buckets.held_amount IS
    'Cached Σ holds − Σ releases − Σ consumes + Σ adjustment held_deltas. Pinned non-negative: a hold is a hard ceiling and a release returns what a hold secured, so held money below zero is a defect the schema refuses, never a state.';
COMMENT ON COLUMN control.funding_buckets.available_amount IS
    'Cached settled_amount − held_amount, the number admission draws down against. Pinned non-negative with its inputs by funding_buckets_balance_projection.';

-- ---------------------------------------------------------------------------
-- settlements — the settlement of record, one per request, ever.
-- ---------------------------------------------------------------------------
CREATE TABLE control.settlements (
    id uuid PRIMARY KEY
        CONSTRAINT settlements_id_uuid_v7
        CHECK (id::text ~ '^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'),
    request_id text NOT NULL
        CONSTRAINT settlements_request_id_grammar
        CHECK (char_length(request_id) BETWEEN 1 AND 256)
        CONSTRAINT settlements_request_id_key UNIQUE,
    settled_total bigint NOT NULL
        CONSTRAINT settlements_settled_total_nonnegative
        CHECK (settled_total >= 0),
    created_at timestamptz NOT NULL
);

COMMENT ON TABLE control.settlements IS
    'Accounting (ADR 0004): the settlement of record — the unique-per-request header whose request_id uniqueness is the exactly-once boundary. Nothing on it is ever mutated; a competing finalizer hits settlements_request_id_key and reads the first result instead of charging again.';
COMMENT ON COLUMN control.settlements.request_id IS
    'The Data Plane request this settlement closes, carried as opaque text: the runtime owns the identifier''s grammar, and this plane asserts only its presence and length. Uniqueness here is invariant 4 — one settlement per request, ever.';
COMMENT ON COLUMN control.settlements.settled_total IS
    'The sum of the settlement''s consume legs, written once at creation and never re-derived: the legs are append-only, so the header and its legs are one fact. Non-negative because a consume amount is a positive magnitude.';
-- No currency column, on purpose: ADR 0004's recorded consequence — amounts
-- are integer minor units in the single platform-wide settlement currency,
-- which is configuration, not a per-row lookup. The provenance gap that
-- follows (a settled figure carries no currency or price provenance across
-- the plane boundary) is issue #63, owed by the fact contract, not by this
-- table.

-- ---------------------------------------------------------------------------
-- ledger_entries — one immutable bucket leg per movement of money.
-- ---------------------------------------------------------------------------
CREATE TABLE control.ledger_entries (
    id uuid PRIMARY KEY
        CONSTRAINT ledger_entries_id_uuid_v7
        CHECK (id::text ~ '^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'),
    funding_bucket_id uuid NOT NULL
        CONSTRAINT ledger_entries_funding_bucket_id_fkey
        REFERENCES control.funding_buckets (id),
    kind text NOT NULL
        CONSTRAINT ledger_entries_kind_valid
        CHECK (kind IN ('grant', 'topup', 'hold', 'release', 'consume', 'adjustment')),
    amount bigint NOT NULL
        CONSTRAINT ledger_entries_amount_positive
        CHECK (amount > 0),
    settled_delta bigint NOT NULL,
    held_delta bigint NOT NULL,
    settlement_id uuid
        CONSTRAINT ledger_entries_settlement_id_fkey
        REFERENCES control.settlements (id),
    reservation_id uuid,
    command_key text,
    price_revision_id text,
    input_unit_price bigint,
    output_unit_price bigint,
    adjustment_reason text,
    original_entry_id uuid
        CONSTRAINT ledger_entries_original_entry_id_fkey
        REFERENCES control.ledger_entries (id),
    operator_id text,
    sequence bigint NOT NULL,
    created_at timestamptz NOT NULL,
    CONSTRAINT ledger_entries_leg_algebra
        CHECK (
            CASE kind
                WHEN 'grant' THEN settled_delta = amount AND held_delta = 0
                WHEN 'topup' THEN settled_delta = amount AND held_delta = 0
                WHEN 'hold' THEN settled_delta = 0 AND held_delta = amount
                WHEN 'release' THEN settled_delta = 0 AND held_delta = -amount
                WHEN 'consume' THEN settled_delta = -amount AND held_delta = -amount
                ELSE
                    (settled_delta <> 0) <> (held_delta <> 0)
                    AND amount = GREATEST(abs(settled_delta), abs(held_delta))
            END
        ),
    CONSTRAINT ledger_entries_price_snapshot
        CHECK (
            (kind = 'consume') = (price_revision_id IS NOT NULL)
            AND (kind = 'consume') = (input_unit_price IS NOT NULL)
            AND (kind = 'consume') = (output_unit_price IS NOT NULL)
            AND (price_revision_id IS NULL OR char_length(price_revision_id) BETWEEN 1 AND 256)
            AND (input_unit_price IS NULL OR input_unit_price > 0)
            AND (output_unit_price IS NULL OR output_unit_price > 0)
        ),
    CONSTRAINT ledger_entries_reference_shape
        CHECK (
            CASE kind
                WHEN 'grant' THEN settlement_id IS NULL AND reservation_id IS NULL
                WHEN 'topup' THEN settlement_id IS NULL AND reservation_id IS NULL
                WHEN 'hold' THEN settlement_id IS NULL AND reservation_id IS NOT NULL
                WHEN 'release' THEN reservation_id IS NOT NULL
                WHEN 'consume' THEN settlement_id IS NOT NULL AND reservation_id IS NULL
                ELSE settlement_id IS NULL AND reservation_id IS NULL
            END
        ),
    CONSTRAINT ledger_entries_adjustment_shape
        CHECK (
            (kind = 'adjustment') = (adjustment_reason IS NOT NULL)
            AND (kind = 'adjustment') = (original_entry_id IS NOT NULL)
            AND (kind = 'adjustment') = (operator_id IS NOT NULL)
            AND (adjustment_reason IS NULL OR char_length(adjustment_reason) BETWEEN 1 AND 1024)
            AND (operator_id IS NULL OR char_length(operator_id) BETWEEN 1 AND 256)
        ),
    CONSTRAINT ledger_entries_command_key_scope
        CHECK (
            CASE
                WHEN kind = 'topup' THEN command_key IS NOT NULL
                WHEN kind = 'adjustment' THEN TRUE
                ELSE command_key IS NULL
            END
            AND (command_key IS NULL OR char_length(command_key) BETWEEN 1 AND 256)
        ),
    CONSTRAINT ledger_entries_bucket_sequence_key UNIQUE (funding_bucket_id, sequence)
);

-- The idempotency keys are PARTIAL unique indexes, not table constraints:
-- PostgreSQL has no partial-unique table-constraint syntax, and each key is
-- only a key where its column is not null — one leg per command key per
-- bucket, one leg per (settlement, bucket, kind) and per (reservation,
-- bucket, kind). The names are what the ledger adapter maps to the port's
-- two duplicate sentinels, so they are the contract's spelling, not
-- decoration.
CREATE UNIQUE INDEX ledger_entries_bucket_command_key
    ON control.ledger_entries (funding_bucket_id, command_key)
    WHERE command_key IS NOT NULL;
CREATE UNIQUE INDEX ledger_entries_settlement_bucket_kind
    ON control.ledger_entries (settlement_id, funding_bucket_id, kind)
    WHERE settlement_id IS NOT NULL;
CREATE UNIQUE INDEX ledger_entries_reservation_bucket_kind
    ON control.ledger_entries (reservation_id, funding_bucket_id, kind)
    WHERE reservation_id IS NOT NULL;

COMMENT ON TABLE control.ledger_entries IS
    'Accounting (ADR 0004): one immutable bucket leg per movement of money — the ledger, and the only thing authoritative for money. Every row states its own algebra (kind, positive amount, the two signed deltas it moves), carries its price snapshot when and only when it consumes, and names what it belongs to by identifier alone where the other row lives in another plane. Corrections are new adjustment legs referencing the original entry; nothing here is ever rewritten.';
COMMENT ON COLUMN control.ledger_entries.amount IS
    'The movement''s magnitude, always strictly positive — direction is the kind''s, stated by the delta columns. On adjustments it is the magnitude of the one stated delta.';
COMMENT ON COLUMN control.ledger_entries.settled_delta IS
    'How this leg moves the bucket''s cached settled balance: +amount for grant/topup, −amount for consume, 0 for hold/release, and whatever the operator stated for an adjustment. The leg_algebra CHECK pins the non-adjustment values to the kind.';
COMMENT ON COLUMN control.ledger_entries.held_delta IS
    'How this leg moves the bucket''s cached held balance: +amount for hold, −amount for release, and −amount for consume — consumption vacates the hold it was secured by, which is why consume subtracts from both balances (ADR 0004).';
COMMENT ON COLUMN control.ledger_entries.settlement_id IS
    'The settlement this leg belongs to: required on consume legs, carried by the release legs that return an allocation''s unconsumed tail. A reservation/settlement may have many legs; one of each kind per bucket per settlement is the ceiling (ledger_entries_settlement_bucket_kind).';
COMMENT ON COLUMN control.ledger_entries.reservation_id IS
    'The Data Plane reservation this leg secures or returns, by identifier alone — no foreign key can cross the plane boundary (ADR 0006 §7), and no grammar is asserted beyond the uuid type''s, because the runtime owns this identifier''s shape. Holds and releases always name one; the pair with the bucket is unique per kind (ledger_entries_reservation_bucket_kind), which is what makes a redelivered fact unable to book a movement twice.';
COMMENT ON COLUMN control.ledger_entries.command_key IS
    'The caller''s idempotency key: required on topups, optional on adjustments, forbidden everywhere else. Unique per bucket where present, so a retried command converges on its original leg instead of moving money twice.';
COMMENT ON COLUMN control.ledger_entries.price_revision_id IS
    'The price revision the consume was priced against, copied by value — the B12 catalog owns revisions, so this is a reference by text, not a foreign key. Required on consume legs and null elsewhere: the other kinds move money without consuming tokens.';
COMMENT ON COLUMN control.ledger_entries.input_unit_price IS
    'The input-token unit price copied from the revision at consume time, in the settlement currency''s minor units. No token counts live here — the leg prices, the usage event counts.';
COMMENT ON COLUMN control.ledger_entries.output_unit_price IS
    'The output-token unit price copied from the revision at consume time, in the settlement currency''s minor units.';
COMMENT ON COLUMN control.ledger_entries.adjustment_reason IS
    'The operator''s stated reason, required exactly on adjustment legs — a movement of money without a stated why is not a correction, it is a defect.';
COMMENT ON COLUMN control.ledger_entries.original_entry_id IS
    'The leg being corrected, required exactly on adjustment legs. Self-referencing: corrections point at history, history points at nothing.';
COMMENT ON COLUMN control.ledger_entries.operator_id IS
    'Who authorised the adjustment, required exactly on adjustment legs. Text, not a users-row reference: the operator surface is not built yet, and the ledger must not guess at its grammar.';
COMMENT ON COLUMN control.ledger_entries.sequence IS
    'The leg''s position in its bucket''s history: allocated from the bucket row''s last_sequence by the same transaction that writes the leg, unique per bucket. Sequence order is the bucket''s true order — never order by id, which orders mint time, not commit time.';

-- ---------------------------------------------------------------------------
-- account_payg — the bucket reference becomes a real, write-once edge.
-- ---------------------------------------------------------------------------

-- B5 landed the column as an unchecked nullable reference; B6 lands the
-- bucket it names. From here the reference is a real foreign key and
-- write-once is engine-enforced: the commerce adapter's upsert already
-- refuses a re-point at the statement level, and the trigger below makes the
-- refusal structural, so no future writer — raw SQL included — can move an
-- account's money to a second bucket or delete the edge that names the one
-- it has.
ALTER TABLE control.account_payg
    ADD CONSTRAINT account_payg_funding_bucket_fkey
    FOREIGN KEY (funding_bucket_id) REFERENCES control.funding_buckets (id);

CREATE FUNCTION control.account_payg_bucket_write_once() RETURNS trigger
LANGUAGE plpgsql AS $account_payg_write_once$ BEGIN
    IF TG_OP = 'DELETE' AND OLD.funding_bucket_id IS NOT NULL THEN
        RAISE EXCEPTION 'control.account_payg.funding_bucket_id is write-once: deleting the reference to bucket % is refused', OLD.funding_bucket_id;
    END IF;
    IF TG_OP = 'UPDATE' AND OLD.funding_bucket_id IS NOT NULL
        AND NEW.funding_bucket_id IS DISTINCT FROM OLD.funding_bucket_id THEN
        RAISE EXCEPTION 'control.account_payg.funding_bucket_id is write-once: re-pointing account % from bucket % to % is refused', OLD.account_id, OLD.funding_bucket_id, NEW.funding_bucket_id;
    END IF;
    RETURN COALESCE(NEW, OLD);
END;
$account_payg_write_once$;

CREATE TRIGGER account_payg_bucket_write_once_guard
    BEFORE UPDATE OF funding_bucket_id OR DELETE ON control.account_payg
    FOR EACH ROW EXECUTE FUNCTION control.account_payg_bucket_write_once();

COMMENT ON FUNCTION control.account_payg_bucket_write_once() IS
    'Accounting engine guard: the PAYG bucket reference is write-once (ADR 0001 rule 5, one PAYG source and one bucket per account, ever). The statement-level guard in the commerce adapter is the belt; this trigger is braces.';

-- ---------------------------------------------------------------------------
-- Append-only engine guards on the history tables.
-- ---------------------------------------------------------------------------

CREATE FUNCTION control.ledger_entries_append_only() RETURNS trigger
LANGUAGE plpgsql AS $ledger_append_only$ BEGIN
    RAISE EXCEPTION 'control.ledger_entries is append-only: % is refused (a correction is a new adjustment leg referencing the original entry)', TG_OP;
END;
$ledger_append_only$;

CREATE TRIGGER ledger_entries_append_only_guard
    BEFORE UPDATE OR DELETE ON control.ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION control.ledger_entries_append_only();

CREATE FUNCTION control.settlements_append_only() RETURNS trigger
LANGUAGE plpgsql AS $settlements_append_only$ BEGIN
    RAISE EXCEPTION 'control.settlements is append-only: % is refused (a correction is a new adjustment leg referencing the original entry)', TG_OP;
END;
$settlements_append_only$;

CREATE TRIGGER settlements_append_only_guard
    BEFORE UPDATE OR DELETE ON control.settlements
    FOR EACH STATEMENT EXECUTE FUNCTION control.settlements_append_only();

COMMENT ON FUNCTION control.ledger_entries_append_only() IS
    'Accounting engine guard: the ledger is history (ADR 0004 invariants 1–2). Balances move because compensating legs move them, never because a row was edited.';
COMMENT ON FUNCTION control.settlements_append_only() IS
    'Accounting engine guard: the settlement of record is written once (ADR 0004). A charge correction appends adjustment legs; the header is never touched again.';
