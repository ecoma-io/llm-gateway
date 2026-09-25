-- The runtime storage foundation: every table the Data Plane owns for the
-- request-serving lifecycle — intake, requests, attempts, reservations and
-- their allocation legs, quota projections, and the usage-fact stream (ADR
-- 0006 §5). Tables sit bare in `public`: the ADRs name them unqualified and
-- this lane keeps the default namespace (migrations/README.md).
--
-- Two decisions this file makes on purpose, each because it cannot be made
-- later on live data:
--
-- PLAIN TABLES. Every table here is unpartitioned, event family included.
-- ADR 0005 places the event family on hypertables; that placement is
-- impossible for this schema and the impossibility was verified against the
-- pinned image: a hypertable's PK/UNIQUE must include the partitioning column
-- (killing requests(id), the attempts business key, and the fact-dedup
-- uniques), and no FK may reference a hypertable (killing every attempt and
-- fact reference to requests). The documented fallback — plain time
-- partitioning — fails the same uniqueness test. What is given up (attempts
-- compression, chunk pruning, continuous aggregates) and the retention
-- consequence are recorded as the repository's decision:
-- https://github.com/ecoma-io/llm-gateway/issues/48. The timescaledb
-- extension stays installed (000001) until that decision retires it.
--
-- FACT ORDERING IS COMMIT ORDERING. usage_events.append_seq is allocated from
-- the single row of usage_events_stream by an UPSERT whose row lock is held to
-- the transaction's commit, so allocation order equals visibility order — the
-- property cross-plane-protocols.md demands ("assigned at the moment a fact
-- commits ... a hole that is skipped rather than a reordering"). A bare
-- sequence allocates at INSERT time, and a transaction that allocated a lower
-- seq but committed after a higher one would have its fact land behind the
-- consumer's cursor forever — a settled request that is never delivered, with
-- no error anywhere. The append statement is therefore the LAST write of the
-- settlement unit of work: it serialises only the tail of each settlement,
-- and the store refuses an append that arrives with no unit of work at all —
-- a sequence allocated outside one could commit apart from its fact, which is
-- the tearing this ordering exists to make impossible.
-- Aborted units roll the counter back (no hole at all; the contract's
-- gap-awareness is satisfied a fortiori). The row's `epoch` is minted by the
-- server on first append — never in a migration — so a database that loses its
-- rows (recreated volume) also loses the stream identity, and every cursor
-- minted by the old stream fails closed as cursor_expired instead of silently
-- skipping re-appended facts. Cursors are "v1.<epoch>.<append_seq>"; the
-- genesis position, before any fact exists, is "v1.0".
--
-- TERMINAL VOCABULARIES. Requests finalise exactly once, by settlement close
-- or by release or by the reaper (overview.md), so the schema must represent
-- every terminal path now, on a forever-retained table:
--
--   status       reason column       values
--   -----------  ------------------  -------------------------------------
--   executing    (both NULL)         —
--   succeeded    (both NULL)         committed_attempt_id NOT NULL
--   rejected     rejection_reason    account_suspended, account_closed,
--                                    unknown_alias, invalid_request,
--                                    insufficient_entitlement, no_access,
--                                    no_candidate, no_candidate_succeeded
--   failed       failure_reason      stream_failed_after_commitment (with
--                                    committed_attempt_id), gateway_abandoned
--                                    (without — the reaper cannot know whether
--                                    a dead process had committed)
--
-- Two values the docs name are deliberately absent: `unauthenticated` writes
-- nothing at all, and `idempotency_conflict` reuses the original request's row
-- (request-lifecycle.md step 4) — neither can appear on a row. The admission
-- eight are request-lifecycle.md steps 2–8 verbatim; the two execution values
-- come from ADR 0001 rule 6 ("finalise the request with its failure reason")
-- and ADR 0002's post-commitment outcome. Usage facts mirror the same split:
-- settled facts carry capture method, committed attempt, price snapshot and
-- settled amount; released and expired facts carry none of them (their
-- allocation tail rides in the payload); a proven orphaned completion carries
-- usage and is never a customer charge (request-lifecycle.md step 10).
--
-- GUARD NUMBERS, and where their siblings live: the payload CHECK caps the
-- serialised text at 32768 octets (the reader re-serialises the text form, so
-- that is what is bounded; octet_length has no jsonb overload). The
-- writer-side domain cap is half of it, 16384 bytes of serialised JSON, and
-- produces
-- the legible error at the write site; the derived read-side backstop
-- (limit × 32KiB) is recorded in cross-plane-protocols.md. Free-text columns
-- carry octet_length caps because every value on them arrives from a client,
-- a provider, or a foreign database; vocabulary columns are bounded by their
-- IN lists instead. started_at/finished_at are app-wall-clock values and are
-- deliberately NOT compared by a CHECK: a clock step backward must not fail a
-- telemetry insert.
--
-- QUOTA PROJECTIONS are the runtime's lockable copy of a grant, not a balance
-- (ADR 0006): one row per funding bucket — the single key every allocation leg
-- carries, so drawdown and return need no other join. Admission orders an
-- account's rows by ADR 0003's one and only waterfall, computed from the raw
-- inputs the row stores (named scope before `*`, earliest period_end, oldest
-- subscription, entitlement id) — the order is derived at read time, never
-- baked into a seed-time rank. Publication algebra, for the migration that
-- first writes here: INSERT seeds available = limit_amount; a redelivered or
-- stale publication is a no-op via `revision`; the conflict branch updates the
-- Control-Plane-owned identity/limit/state columns and NEVER `available` —
-- spent capacity must never be resurrected by a republication. Capacity
-- increases beyond the original grant arrive as their own guarded refill
-- operation, not as a re-seed, and each refill carries the Control Plane's
-- refill identity: quota_refills below records every identity once, so a
-- redelivered refill answers "already applied" instead of minting twice, and
-- one that arrives after the projection moved past its guard revision answers
-- "stale" — the identity is spent unapplied, and the increase must be
-- re-derived under a new one. A missing row at admission (publication lag)
-- is capacity the runtime cannot see: admission treats it as zero.
--
-- Every terminal-state guard is stated twice on purpose: as column CHECKs (a
-- row cannot be BORN illegal — rejections are inserted terminal) and as the
-- BEFORE UPDATE triggers below (a legal row cannot be driven illegal — state
-- transitions are CAS UPDATEs whose WHERE clause is the real guard; the
-- trigger is the bug detector behind them, with zero per-row cost on open rows
-- via its WHEN clause). It can be bypassed by session_replication_role =
-- replica; that is the privilege model's business, not the trigger's.

CREATE TABLE public.requests (
    id uuid NOT NULL PRIMARY KEY,
    account_id text NOT NULL CHECK (octet_length(account_id) <= 128),
    api_key_id text NOT NULL CHECK (octet_length(api_key_id) <= 128),
    alias text CHECK (octet_length(alias) <= 256),
    input_tokens integer CHECK (input_tokens >= 0),
    max_output_tokens integer CHECK (max_output_tokens > 0),
    price_revision_id text CHECK (octet_length(price_revision_id) <= 128),
    input_unit_price bigint CHECK (input_unit_price >= 0),
    output_unit_price bigint CHECK (output_unit_price >= 0),
    status text NOT NULL CHECK (status IN ('executing', 'succeeded', 'failed', 'rejected')),
    rejection_reason text,
    failure_reason text,
    committed_attempt_id uuid,
    admitted_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    finished_at timestamptz,
    -- The price snapshot is all-or-nothing: a revision id without its two
    -- resolved unit prices would be a snapshot the settlement cannot re-derive.
    CONSTRAINT requests_price_snapshot_pairing CHECK (
        (price_revision_id IS NULL) = (input_unit_price IS NULL)
        AND (price_revision_id IS NULL) = (output_unit_price IS NULL)
    ),
    -- A final request carries the full admission snapshot — alias, canonical
    -- bounds, price revision — because its fact is the settlement's only
    -- input. A rejected row is written "with the fields known so far"
    -- (request-lifecycle.md step 3), so everything but the status is optional.
    CONSTRAINT requests_final_snapshot_present CHECK (
        status NOT IN ('succeeded', 'failed')
        OR (
            alias IS NOT NULL
            AND input_tokens IS NOT NULL
            AND max_output_tokens IS NOT NULL
            AND price_revision_id IS NOT NULL
        )
    ),
    -- executing is the only status with nothing else set; each terminal status
    -- owns exactly one reason column and a closed shape. Together these four
    -- constraints also pin committed_attempt_id to the succeeded and
    -- failed-after-commitment rows and nowhere else.
    CONSTRAINT requests_executing_shape CHECK (
        (status = 'executing')
        = (
            rejection_reason IS NULL
            AND failure_reason IS NULL
            AND committed_attempt_id IS NULL
            AND finished_at IS NULL
        )
    ),
    CONSTRAINT requests_rejected_shape CHECK (
        status <> 'rejected'
        OR (
            rejection_reason IN (
                'account_suspended', 'account_closed', 'unknown_alias', 'invalid_request',
                'insufficient_entitlement', 'no_access', 'no_candidate', 'no_candidate_succeeded'
            )
            AND failure_reason IS NULL
            AND committed_attempt_id IS NULL
            AND finished_at IS NOT NULL
        )
    ),
    CONSTRAINT requests_failed_shape CHECK (
        status <> 'failed'
        OR (
            rejection_reason IS NULL
            AND failure_reason IN ('stream_failed_after_commitment', 'gateway_abandoned')
            AND finished_at IS NOT NULL
        )
    ),
    -- stream_failed_after_commitment names the attempt the commitment happened
    -- on; gateway_abandoned is the reaper's close, which cannot know whether a
    -- commitment happened and therefore claims no attempt.
    CONSTRAINT requests_failure_reason_attempt_pairing CHECK (
        failure_reason IS NULL
        OR (failure_reason = 'stream_failed_after_commitment') = (committed_attempt_id IS NOT NULL)
    ),
    CONSTRAINT requests_succeeded_shape CHECK (
        status <> 'succeeded'
        OR (
            rejection_reason IS NULL
            AND failure_reason IS NULL
            AND committed_attempt_id IS NOT NULL
            AND finished_at IS NOT NULL
        )
    )
);

-- One row per upstream call, appended as the call finishes (ADR 0001 rule 4) —
-- never while it is in flight, so no transaction is open across a provider
-- call and a crash mid-call simply leaves no row. The row is insert-only for
-- its business content; the ONE sanctioned update rewrites the provider-usage
-- telemetry columns alone, because provider usage reported after a disconnect
-- arrives after the row exists (overview.md, RequestAttempt). Identity,
-- outcome, class and timing are never updated.
CREATE TABLE public.request_attempts (
    id uuid NOT NULL PRIMARY KEY,
    request_id uuid NOT NULL REFERENCES public.requests (id),
    candidate_position integer NOT NULL CHECK (candidate_position >= 0),
    retry_sequence integer NOT NULL CHECK (retry_sequence >= 0),
    backend_id text NOT NULL CHECK (octet_length(backend_id) <= 128),
    provider_model text NOT NULL CHECK (octet_length(provider_model) <= 256),
    provider_request_id text CHECK (octet_length(provider_request_id) <= 256),
    outcome text NOT NULL CHECK (
        outcome IN ('succeeded', 'failed_before_commitment', 'failed_after_commitment')
    ),
    error_class text CHECK (
        error_class IS NULL
        OR error_class IN (
            'authentication', 'rate_limited', 'provider_unavailable', 'provider_rejected_request',
            'context_too_large', 'invalid_upstream_response', 'upstream_error',
            'stream_failed_after_commitment'
        )
    ),
    provider_input_tokens bigint CHECK (provider_input_tokens >= 0),
    provider_output_tokens bigint CHECK (provider_output_tokens >= 0),
    delivery_tokens bigint CHECK (delivery_tokens >= 0),
    -- One of the schema's two sanctioned jsonb columns (the other is the fact
    -- payload below): opaque provider telemetry, preserved for debugging
    -- (ADR 0002). Same object + size guard as the payload's, because neither
    -- column may become a place where an unbounded blob rides past every
    -- other guard.
    provider_error jsonb CHECK (
        provider_error IS NULL
        OR (jsonb_typeof(provider_error) = 'object' AND octet_length(provider_error::text) <= 65536)
    ),
    started_at timestamptz NOT NULL,
    finished_at timestamptz NOT NULL,
    -- (request_id, id) exists to be referenced: it lets the request's
    -- billing-subject pointer name an attempt OF ITS OWN REQUEST and no
    -- other — a plain FK on id alone would accept another request's attempt.
    CONSTRAINT request_attempts_business_key UNIQUE (request_id, candidate_position, retry_sequence),
    CONSTRAINT request_attempts_request_id_id_key UNIQUE (request_id, id),
    -- succeeded has no error to classify. failed_before_commitment always has
    -- one. failed_after_commitment may or may not: the provider's stream can
    -- die after commitment (class set), and the client can walk away from a
    -- healthy stream (no provider fault to name — usage settles on best-known
    -- delivery). No CHECK compares started_at to finished_at: both are app
    -- wall-clock values, and an NTP step backward must not fail a telemetry
    -- insert.
    CONSTRAINT request_attempts_succeeded_has_no_error CHECK (
        outcome <> 'succeeded' OR error_class IS NULL
    ),
    CONSTRAINT request_attempts_pre_commitment_has_error CHECK (
        outcome <> 'failed_before_commitment' OR error_class IS NOT NULL
    )
);

-- The pointer cannot be declared inline: it references the table above. The
-- composite form carries the same-request guarantee; its NULL half — an
-- executing or rejected request names no attempt — skips enforcement by
-- MATCH SIMPLE's default.
ALTER TABLE public.requests
    ADD CONSTRAINT requests_committed_attempt_fkey
    FOREIGN KEY (id, committed_attempt_id)
    REFERENCES public.request_attempts (request_id, id);

-- The relational replay record (ADR 0004): decided on the hot path with the
-- Control Plane switched off, and never joined to the event family for a
-- business decision (ADR 0005). request_id is therefore an ID reference, not
-- an enforced FK — enforced keys stop at the family boundary in both
-- directions, and admission's own transaction is what keeps the pair atomic.
-- The terminal pointer is written once, at finalisation ("immutable
-- afterwards" is scoped to the replay-identity fields: account, key, digest,
-- request_id — and that scope is engine-enforced, by
-- request_intake_identity_immutability below). final_status NULL means the
-- original is still executing: a
-- replay arriving then is refused with retry-later semantics, never queued.
CREATE TABLE public.request_intake (
    account_id text NOT NULL CHECK (octet_length(account_id) <= 128),
    idempotency_key text NOT NULL CHECK (octet_length(idempotency_key) <= 256),
    request_digest text NOT NULL CHECK (octet_length(request_digest) <= 128),
    request_id uuid NOT NULL,
    final_status text CHECK (final_status IN ('succeeded', 'failed', 'rejected')),
    final_rejection_reason text CHECK (
        final_rejection_reason IS NULL
        OR final_rejection_reason IN (
            'account_suspended', 'account_closed', 'unknown_alias', 'invalid_request',
            'insufficient_entitlement', 'no_access', 'no_candidate', 'no_candidate_succeeded'
        )
    ),
    final_failure_reason text CHECK (
        final_failure_reason IS NULL
        OR final_failure_reason IN ('stream_failed_after_commitment', 'gateway_abandoned')
    ),
    created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    CONSTRAINT request_intake_scope_key UNIQUE (account_id, idempotency_key),
    -- The pointer is all-or-nothing at its base: a reason column exists only
    -- under a status that names it. Which reason each status carries is the
    -- three shape CHECKs below — succeeded carries none, and that is exactly
    -- why this constraint demands a status for any reason, not the reverse:
    -- a pairing spelled "status NULL iff both reasons NULL" would make a
    -- succeeded pointer (status set, both reasons NULL) unwritable, the one
    -- terminal fate the replay record most needs to point at.
    CONSTRAINT request_intake_final_pairing CHECK (
        final_status IS NOT NULL
        OR (final_rejection_reason IS NULL AND final_failure_reason IS NULL)
    ),
    CONSTRAINT request_intake_final_rejected_shape CHECK (
        final_status <> 'rejected'
        OR (final_rejection_reason IS NOT NULL AND final_failure_reason IS NULL)
    ),
    CONSTRAINT request_intake_final_failed_shape CHECK (
        final_status <> 'failed'
        OR (final_rejection_reason IS NULL AND final_failure_reason IS NOT NULL)
    ),
    CONSTRAINT request_intake_final_succeeded_shape CHECK (
        final_status <> 'succeeded'
        OR (final_rejection_reason IS NULL AND final_failure_reason IS NULL)
    )
);

-- The runtime reservation: a lock on future spend, never a financial record
-- and never a B6 funding bucket. request_id is a cross-family ID reference
-- (relational → event, ADR 0005): reservations are created inside the
-- admission transaction next to their request, retention is forever, and no
-- delete path exists to dangle. UNIQUE(request_id) is the engine half of
-- "one reservation per request" (ADR 0004 invariant 6): a duplicate hold can
-- never exist to be settled twice. The pricing snapshot is the reservation's
-- own basis — reserved_amount was derived from exactly these numbers, and the
-- reservation outlives any catalog change that would make a join lie.
CREATE TABLE public.reservations (
    id uuid NOT NULL PRIMARY KEY,
    request_id uuid NOT NULL UNIQUE,
    price_revision_id text NOT NULL CHECK (octet_length(price_revision_id) <= 128),
    input_unit_price bigint NOT NULL CHECK (input_unit_price >= 0),
    output_unit_price bigint NOT NULL CHECK (output_unit_price >= 0),
    input_tokens integer NOT NULL CHECK (input_tokens >= 0),
    max_output_tokens integer NOT NULL CHECK (max_output_tokens > 0),
    reserved_amount bigint NOT NULL CHECK (reserved_amount >= 0),
    state text NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'settled', 'released', 'expired')),
    created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    expires_at timestamptz NOT NULL,
    lease_owner text NOT NULL CHECK (octet_length(lease_owner) <= 256),
    lease_expires_at timestamptz NOT NULL,
    closed_at timestamptz,
    CONSTRAINT reservations_open_shape CHECK ((state = 'open') = (closed_at IS NULL)),
    CONSTRAINT reservations_expiry_after_creation CHECK (expires_at > created_at),
    CONSTRAINT reservations_lease_expiry_after_creation CHECK (lease_expires_at >= created_at)
);

-- The waterfall split, recorded as data: which funding buckets carried the
-- hold, in what order, and for how much. Money is integer minor units with no
-- currency column (ADR 0004). A Reservation Allocation is NOT a Ledger Entry:
-- this is the runtime's memory of its own drawdown, from which the fact
-- payload is built; the ledger's legs are the Control Plane's derivation.
CREATE TABLE public.reservation_allocations (
    reservation_id uuid NOT NULL REFERENCES public.reservations (id),
    funding_bucket_id text NOT NULL CHECK (octet_length(funding_bucket_id) <= 128),
    amount bigint NOT NULL CHECK (amount > 0),
    ordinal integer NOT NULL CHECK (ordinal > 0),
    CONSTRAINT reservation_allocations_bucket_key UNIQUE (reservation_id, funding_bucket_id),
    CONSTRAINT reservation_allocations_ordinal_key UNIQUE (reservation_id, ordinal)
);

-- The runtime's lockable copy of one grant (see the header's publication
-- algebra). One row per funding bucket — the only key an allocation leg
-- carries — so entitlement cycles and PAYG share one row shape and one
-- drawdown verb. The waterfall order inputs are stored raw; the order itself
-- is computed at admission per ADR 0003 and must never be precomputed into a
-- seed-time rank. `available` is the runtime's number alone: publications
-- never touch it.
CREATE TABLE public.quota_projections (
    account_id text NOT NULL CHECK (octet_length(account_id) <= 128),
    scope_kind text NOT NULL CHECK (scope_kind IN ('entitlement_cycle', 'payg_balance')),
    entitlement_id text CHECK (octet_length(entitlement_id) <= 128),
    cycle_number integer CHECK (cycle_number > 0),
    funding_bucket_id text NOT NULL UNIQUE CHECK (octet_length(funding_bucket_id) <= 128),
    alias_group_version_id text NOT NULL CHECK (octet_length(alias_group_version_id) <= 128),
    -- Scope specificity as a seed-time boolean: the version this grant was
    -- made under is a named group (true) or the singleton `*` (false). Stored
    -- rather than derived so admission needs nothing but this table.
    named_scope boolean NOT NULL,
    dimension text NOT NULL CHECK (dimension = 'cost'),
    period_end timestamptz,
    subscription_created_at timestamptz NOT NULL,
    state text NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'inactive')),
    limit_amount bigint NOT NULL CHECK (limit_amount >= 0),
    -- No upper bound against limit_amount on purpose: a publication that
    -- shrinks the ceiling below outstanding capacity must not turn every later
    -- capacity return into a failed release. The guard that matters is the
    -- conditional drawdown's `available >= take`.
    available bigint NOT NULL CHECK (available >= 0),
    revision bigint NOT NULL DEFAULT 0,
    seeded_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    CONSTRAINT quota_projections_entitlement_scope CHECK (
        (scope_kind = 'entitlement_cycle')
        = (entitlement_id IS NOT NULL AND cycle_number IS NOT NULL AND period_end IS NOT NULL)
    ),
    CONSTRAINT quota_projections_payg_scope CHECK (
        (scope_kind = 'payg_balance')
        = (entitlement_id IS NULL AND cycle_number IS NULL AND period_end IS NULL)
    )
);

-- Refills are the one operation here that MINTS capacity, which is why they
-- carry an identity and this table exists: the refill message crosses the
-- plane boundary at-least-once, and the unique below is the engine guard that
-- turns the redelivery into a recorded no-op instead of a second mint. A
-- refill whose guard revision has already passed is recorded under its
-- identity but raises nothing — the identity is spent unapplied, and the
-- increase must arrive again under a new one; that asymmetry is deliberate,
-- because the alternative (raising on a stale guard) is drawing against a
-- grant state the Control Plane has already superseded. The row is the
-- receipt: "this identity was seen at this revision", whatever the outcome.
CREATE TABLE public.quota_refills (
    funding_bucket_id text NOT NULL REFERENCES public.quota_projections (funding_bucket_id),
    refill_id text NOT NULL CHECK (octet_length(refill_id) <= 128),
    amount bigint NOT NULL CHECK (amount > 0),
    at_revision bigint NOT NULL CHECK (at_revision >= 0),
    applied_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    CONSTRAINT quota_refills_refill_id_key UNIQUE (refill_id)
);

-- One settled (or released/expired) request has exactly one
-- settlement-relevant fact, enforced by the engine: the dedup partial unique
-- below is what keeps a duplicated or out-of-order close from poisoning a
-- feed page that can then never be applied. A correction, when the day comes,
-- is a NEW fact referencing the original (ADR 0004 invariant 1) — the nullable
-- corrects_append_seq exists now so the unique shape does not foreclose it;
-- while the wire contract pins "at most one settlement-relevant fact per
-- request_id", no correction is ever appended. unbillable_orphaned sits
-- outside the settlement unique on purpose: it is NOT settlement-relevant (it
-- derives no legs), it coexists with an expired fact by design, and the
-- consumer's per-request idempotency must not read the pair as a violation.
CREATE TABLE public.usage_events (
    append_seq bigint NOT NULL PRIMARY KEY,
    request_id uuid NOT NULL REFERENCES public.requests (id),
    kind text NOT NULL CHECK (kind IN ('settled', 'released', 'expired', 'unbillable_orphaned')),
    schema_version integer NOT NULL CHECK (schema_version >= 1),
    capture_method text CHECK (capture_method IN ('reported', 'gateway_observed', 'reservation_floor')),
    committed_attempt_id uuid,
    provider_input_tokens bigint CHECK (provider_input_tokens >= 0),
    provider_output_tokens bigint CHECK (provider_output_tokens >= 0),
    delivery_tokens bigint CHECK (delivery_tokens >= 0),
    price_revision_id text CHECK (octet_length(price_revision_id) <= 128),
    input_unit_price bigint CHECK (input_unit_price >= 0),
    output_unit_price bigint CHECK (output_unit_price >= 0),
    settled_amount bigint CHECK (settled_amount >= 0),
    corrects_append_seq bigint,
    payload jsonb NOT NULL,
    occurred_at timestamptz NOT NULL,
    -- jsonb admits scalars, arrays and `null` as valid values; the contract
    -- requires an object, and the size bound is the read-side backstop's
    -- basis (see the header). `::text` because octet_length has no jsonb
    -- overload.
    CONSTRAINT usage_events_payload_shape CHECK (
        jsonb_typeof(payload) = 'object' AND octet_length(payload::text) <= 32768
    ),
    -- Usage-bearing facts (settled, and a proven orphaned completion) name
    -- how their usage was captured and which attempt it came from; terminal
    -- non-usage facts (released, expired) carry none of the settlement
    -- figures. Tokens stay nullable even on settled facts: usage that could
    -- not be observed is settled conservatively, never invented.
    CONSTRAINT usage_events_usage_bearing_shape CHECK (
        (kind IN ('settled', 'unbillable_orphaned'))
        = (capture_method IS NOT NULL AND committed_attempt_id IS NOT NULL)
    ),
    CONSTRAINT usage_events_settled_amount_shape CHECK (
        (kind = 'settled') = (settled_amount IS NOT NULL)
    ),
    CONSTRAINT usage_events_settled_price_shape CHECK (
        (kind = 'settled')
        = (
            price_revision_id IS NOT NULL
            AND input_unit_price IS NOT NULL
            AND output_unit_price IS NOT NULL
        )
    ),
    CONSTRAINT usage_events_non_usage_shape CHECK (
        kind NOT IN ('released', 'expired')
        OR (
            capture_method IS NULL
            AND committed_attempt_id IS NULL
            AND settled_amount IS NULL
            AND price_revision_id IS NULL
            AND input_unit_price IS NULL
            AND output_unit_price IS NULL
            AND provider_input_tokens IS NULL
            AND provider_output_tokens IS NULL
            AND delivery_tokens IS NULL
        )
    ),
    -- (request_id, append_seq) exists to be referenced: the correction pointer
    -- must name a fact OF THIS REQUEST, and a plain FK on append_seq alone
    -- would let a correction claim another request's fact as its target.
    CONSTRAINT usage_events_request_seq_key UNIQUE (request_id, append_seq)
);

-- These two pointers cannot be declared inline: one references the attempts
-- table above, the other this table's own (request_id, append_seq) key. Both
-- take the composite form for the same reason requests_committed_attempt_fkey
-- does — usage-bearing facts and corrections name rows OF THEIR OWN REQUEST
-- and no other; a plain FK on id or on append_seq alone would accept another
-- request's attempt or another request's fact. The NULL halves — a released
-- or expired fact names no attempt, a first fact corrects nothing — skip
-- enforcement by MATCH SIMPLE's default.
ALTER TABLE public.usage_events
    ADD CONSTRAINT usage_events_committed_attempt_fkey
    FOREIGN KEY (request_id, committed_attempt_id)
    REFERENCES public.request_attempts (request_id, id);

ALTER TABLE public.usage_events
    ADD CONSTRAINT usage_events_correction_targets_same_request_fkey
    FOREIGN KEY (request_id, corrects_append_seq)
    REFERENCES public.usage_events (request_id, append_seq);

-- The dedup guards themselves. Both are partial, so a correction fact (whose
-- corrects_append_seq is set) never collides with the fact it corrects, and
-- neither index spends space on rows it cannot arbitrate. They are also the
-- only request_id index this table carries: every lookup by request is a
-- settlement-dedup lookup, so the query path and the uniqueness guard are the
-- same index (the planner reaches both branches through a BitmapOr).
CREATE UNIQUE INDEX usage_events_settlement_key
    ON public.usage_events (request_id)
    WHERE kind IN ('settled', 'released', 'expired') AND corrects_append_seq IS NULL;
CREATE UNIQUE INDEX usage_events_orphan_key
    ON public.usage_events (request_id)
    WHERE kind = 'unbillable_orphaned';

-- The ordering authority: one row, created by the first append, never by a
-- migration. See the header for why this exists and why the epoch is
-- server-minted. append_seq values start at 1; the genesis cursor "v1.0"
-- predates any row.
CREATE TABLE public.usage_events_stream (
    singleton boolean NOT NULL PRIMARY KEY DEFAULT true CHECK (singleton),
    epoch uuid NOT NULL DEFAULT gen_random_uuid(),
    last_seq bigint NOT NULL DEFAULT 0 CHECK (last_seq >= 0),
    updated_at timestamptz NOT NULL DEFAULT transaction_timestamp()
);

-- Terminal rows are immutable at the engine level. These fire only on rows
-- ALREADY terminal (the WHEN clause), so every open-row write — lease
-- renewals, telemetry updates — costs nothing but the WHEN evaluation, and a
-- CAS UPDATE that misses matches no rows and fires nothing. request_attempts
-- gets no terminal trigger: its sanctioned telemetry update is a real write
-- path. Every function below pins its search path: a trigger runs inside the
-- writing session, and an unqualified name in the body would resolve against
-- whatever search_path the caller set, not against this schema's intent.
CREATE FUNCTION public.reject_terminal_row_update() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
    AS $$
BEGIN
    RAISE EXCEPTION '% has a terminal row that cannot be updated', TG_TABLE_NAME;
END;
$$;

CREATE TRIGGER requests_terminal_immutability
    BEFORE UPDATE ON public.requests
    FOR EACH ROW
    WHEN (OLD.status IN ('succeeded', 'failed', 'rejected'))
    EXECUTE FUNCTION public.reject_terminal_row_update();

CREATE TRIGGER reservations_terminal_immutability
    BEFORE UPDATE ON public.reservations
    FOR EACH ROW
    WHEN (OLD.state IN ('settled', 'released', 'expired'))
    EXECUTE FUNCTION public.reject_terminal_row_update();

-- The intake's replay identity is engine-immutable, not merely write-once by
-- port contract: a decided record whose account, key, digest or request could
-- be rewritten would be a different request wearing the first one's
-- idempotency, and the schema is the last guard against exactly that. The
-- terminal pointer (final_status and its reason columns) stays free to be
-- written by finalisation — the trigger checks the identity fields only.
CREATE FUNCTION public.reject_intake_identity_rewrite() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
    AS $$
BEGIN
    IF NEW.account_id IS DISTINCT FROM OLD.account_id
       OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
       OR NEW.request_digest IS DISTINCT FROM OLD.request_digest
       OR NEW.request_id IS DISTINCT FROM OLD.request_id THEN
        RAISE EXCEPTION '% has a decided replay identity that cannot be rewritten', TG_TABLE_NAME;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER request_intake_identity_immutability
    BEFORE UPDATE ON public.request_intake
    FOR EACH ROW
    EXECUTE FUNCTION public.reject_intake_identity_rewrite();

-- The allocation legs are the reservation's memory, and the memory must
-- re-derive the hold: their sum IS the reserved amount, their ordinals count
-- from 1 without gaps, and no bucket appears twice (the two uniques inside
-- the table keep one leg per ordinal and one per bucket; this trigger keeps
-- the set they form equal to the hold). Stated twice on purpose, like every
-- invariant in this file: validateAllocations in the domain refuses the
-- malformed set at the call site, and this statement trigger — judged over
-- the INSERT's transition relation, so a multi-leg insert is seen whole —
-- refuses it at the engine no matter who wrote it.
CREATE FUNCTION public.reject_legs_that_do_not_rederive_the_hold() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
    AS $$
DECLARE
    held record;
BEGIN
    FOR held IN
        SELECT inserted_legs.reservation_id,
               reservations.reserved_amount,
               sum(inserted_legs.amount) AS leg_sum,
               count(*) AS leg_count,
               count(DISTINCT inserted_legs.funding_bucket_id) AS bucket_count,
               min(inserted_legs.ordinal) AS first_ordinal,
               max(inserted_legs.ordinal) AS last_ordinal
        FROM inserted_legs
        JOIN public.reservations ON reservations.id = inserted_legs.reservation_id
        GROUP BY inserted_legs.reservation_id, reservations.reserved_amount
    LOOP
        IF held.leg_sum <> held.reserved_amount
           OR held.first_ordinal <> 1
           OR held.last_ordinal <> held.leg_count
           OR held.bucket_count <> held.leg_count THEN
            RAISE EXCEPTION '% has allocation legs that do not re-derive its hold', held.reservation_id;
        END IF;
    END LOOP;
    RETURN NULL;
END;
$$;

CREATE TRIGGER reservation_allocations_rederive_the_hold
    AFTER INSERT ON public.reservation_allocations
    REFERENCING NEW TABLE AS inserted_legs
    FOR EACH STATEMENT
    EXECUTE FUNCTION public.reject_legs_that_do_not_rederive_the_hold();

-- And the memory is never rewritten: legs record what the runtime drew down,
-- so each is inserted once — the whole leg set inside the reservation's
-- opening transaction, before the hold can be closed by anyone — and never
-- updated or deleted afterwards.
CREATE FUNCTION public.reject_allocation_leg_rewrite() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
    AS $$
BEGIN
    RAISE EXCEPTION '% has an allocation leg that cannot be rewritten', TG_TABLE_NAME;
END;
$$;

CREATE TRIGGER reservation_allocations_immutability
    BEFORE UPDATE OR DELETE ON public.reservation_allocations
    FOR EACH ROW
    EXECUTE FUNCTION public.reject_allocation_leg_rewrite();

-- The fact payload's envelope is engine-checked, not only writer-checked. The
-- domain marshals the version-1 allocation envelope through a struct and the
-- CHECK above bounds its size; this trigger is the twin that holds for a row
-- written by any other hand: exactly one key — allocations — naming an array
-- of legs shaped like the domain's (non-empty funding bucket, positive
-- integer amount, ordinal counting contiguously from 1, no bucket twice). A
-- payload this build cannot decode as an allocation tail would otherwise be
-- the one place foreign material could ride into a forever-retained money
-- table (this file's doctrine: the engine is the final guard). The integer
-- spellings are demanded in the text form too, not only by value: a
-- consumer's decoder binds these fields to integer types, and a payload the
-- engine accepts but the consumer cannot decode would wedge the feed page it
-- lands in.
CREATE FUNCTION public.reject_undeliverable_fact_payload() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
    AS $$
DECLARE
    legs jsonb;
    leg jsonb;
    duplicates integer;
    i integer;
BEGIN
    IF jsonb_typeof(NEW.payload -> 'allocations') IS DISTINCT FROM 'array'
       OR NEW.payload - 'allocations' <> '{}'::jsonb THEN
        RAISE EXCEPTION '% has a payload that is not exactly the version-1 allocation envelope', TG_TABLE_NAME;
    END IF;
    legs := NEW.payload -> 'allocations';
    FOR i IN 0 .. (jsonb_array_length(legs) - 1) LOOP
        leg := legs -> i;
        IF jsonb_typeof(leg) IS DISTINCT FROM 'object'
           OR leg - 'funding_bucket_id' - 'amount' - 'ordinal' <> '{}'::jsonb
           OR jsonb_typeof(leg -> 'funding_bucket_id') IS DISTINCT FROM 'string'
           OR octet_length(leg ->> 'funding_bucket_id') NOT BETWEEN 1 AND 128
           OR jsonb_typeof(leg -> 'amount') IS DISTINCT FROM 'number'
           OR leg ->> 'amount' !~ '^[1-9][0-9]*$'
           OR (leg ->> 'amount')::bigint <= 0
           OR jsonb_typeof(leg -> 'ordinal') IS DISTINCT FROM 'number'
           OR leg ->> 'ordinal' !~ '^[1-9][0-9]*$'
           OR (leg ->> 'ordinal')::integer IS DISTINCT FROM i + 1 THEN
            RAISE EXCEPTION '% has a payload leg that is not an allocation of the version-1 envelope', TG_TABLE_NAME;
        END IF;
        SELECT count(*) INTO duplicates
        FROM jsonb_array_elements(legs) AS other
        WHERE other -> 'funding_bucket_id' = leg -> 'funding_bucket_id';
        IF duplicates <> 1 THEN
            RAISE EXCEPTION '% has a payload leg naming its funding bucket twice', TG_TABLE_NAME;
        END IF;
    END LOOP;
    RETURN NEW;
END;
$$;

CREATE TRIGGER usage_events_payload_envelope
    BEFORE INSERT OR UPDATE OF payload ON public.usage_events
    FOR EACH ROW
    EXECUTE FUNCTION public.reject_undeliverable_fact_payload();

-- The reaper's scan: open reservations whose hold window AND whose lease have
-- both lapsed, over a forever-retained table where open rows are a vanishing
-- fraction. The index leads on expires_at — the sweep's distinguishing
-- predicate — and carries lease_expires_at so the surviving minority's second
-- condition is evaluated from the index tuple rather than the heap. The sweep
-- is not index-only — the reaper takes whole rows to expire them — and both
-- its conditions compare against clock_timestamp() (advancing), not
-- transaction_timestamp() (frozen for the transaction). A lapsed lease with a
-- live window is a holder mid-renewal-hiccup, not a corpse: taking it would
-- burn the settlement dedup slot on a hold its request may still settle.
CREATE INDEX reservations_open_expiry
    ON public.reservations (expires_at) INCLUDE (lease_expires_at)
    WHERE state = 'open';

-- Admission's candidate fetch: one account's projections, ordered by the
-- waterfall. A handful of rows per account — the sort happens in memory; the
-- index only needs to find the account.
CREATE INDEX quota_projections_account
    ON public.quota_projections (account_id);
