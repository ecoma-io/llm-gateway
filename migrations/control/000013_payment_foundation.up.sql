-- Migrations lane: control — the Control Plane's `control` database
-- (ADR 0006 §7). This file lands the Control Plane's payment integration:
-- the payments this platform opens with an external provider, the provider
-- deliveries it has verified and recorded, and the deliveries it could not
-- apply.
--
-- 000013 — payment foundation (B15):
--
--   B15 is the path by which an account funds its own PAYG balance. It is the
--   first money door in this plane that a CUSTOMER drives, and that one fact
--   is what every decision below is arranged around. ADR 0004 recorded the
--   platform's topup door as "operator today, payment provider later
--   (webhook)" (docs/architecture/accounting.md, "What the foundation
--   carries"), and this migration lands the later half without widening the
--   door: the money still arrives as a keyed `topup` leg through Accounting,
--   and nothing here books a leg of its own.
--
--   MONEY IS CREDITED BY A PROVIDER-SIGNED SERVER-TO-SERVER WEBHOOK, NEVER BY
--   A BROWSER REDIRECT. The two arrive at the same moment in a customer's
--   journey and they are not the same kind of fact, so the distinction is
--   stated here rather than left to a transport to remember. A redirect is a
--   message from the CUSTOMER'S own browser — a parameter on a URL, arriving
--   over a channel the customer controls, that nothing in it authenticates —
--   and a platform that credited on one would be crediting on whatever the
--   customer typed into their own address bar. The webhook is the provider's
--   own server speaking to this one, and it is the only message whose exact
--   bytes are checked against a signature this deployment shares with the
--   provider. The redirect keeps its real job and nothing more: it is where
--   the customer LANDS after the provider's page, an affordance, never
--   evidence. The outbound port says the same thing from the other side —
--   nothing in it moves money, and its checkout creates a checkout rather
--   than taking one (apps/console-api/internal/ports/outbound/payments).
--   This table's rows are what a verified delivery is resolved against.
--
--   WHY THE FUNDING BUCKET MUST BE AN ACCOUNT BUCKET. `funding_buckets`'
--   owner XOR (000006) gives a bucket exactly one owner: an entitlement's
--   cycle bucket or an account's PAYG bucket, never both, never neither. A
--   top-up is a PAYG movement by definition — it is the kind whose algebra is
--   settled = +amount with no hold behind it and no settlement naming it — so
--   the bucket a payment names must be the ACCOUNT branch, and a cycle bucket
--   is not merely undesirable here but unrepresentable: its money is an
--   entitlement's, granted per cycle by commerce, and prepaid money paid into
--   one would sit in a cycle that closes and whose only history is a grant it
--   never came from. The pairing of (account_id, funding_bucket_id) spans two
--   tables, so no CHECK can state it; the trigger below does, and it is a
--   SAME-COLUMN equality — bucket.account_id = intent.account_id. It is
--   deliberately NOT the two-hop entitlement -> subscription resolution
--   000011 indexes: that resolution exists to find the owner of a CYCLE
--   bucket, and a payment must never name one.
--
--   WHY THREE TABLES AND NOT FOUR. A refund is RECOGNISED AND RECORDED here
--   and is NOT booked as a ledger entry, and the absence of a refunds table
--   is that decision rather than an omission. B6's algebra cannot represent
--   the debit a refund is: any adjustment that drives a settled balance below
--   zero is refused in three places on purpose (ApplyTo, the persisted echo's
--   WHERE clause, and funding_buckets_balance_projection), and the ordinary
--   case is exactly that case — a customer tops up 10000, spends 9000, and is
--   then refunded the whole payment. They are owed 10000 and they hold 1000;
--   both facts cannot live in a balance, and the balance has to win, because
--   the alternative is an account that reads -9000. The way around it, a held
--   debit authored under an operator id, is minting a machine principal to
--   force a correction through the ledger, and that is how an audit trail
--   learns to lie — this repository has already refused it once, and 000009's
--   header records the same refusal for reconciliation's auto-repair. So the
--   refund is carried as two figures on the intent (what the provider
--   returned, and the part the balance could not cover) beside the deliveries
--   that reported it, and the accounting decision stays with whoever is
--   entitled to make it. A fourth table would be a table whose rows have no
--   leg behind them and no way to get one — a fourth place one figure is
--   written, and the one place it could never be reconciled.
--
--   The three tables, then:
--
--     payment_intents      one row per funding top-up this platform initiated
--                          with an external provider. It is the only mutable
--                          table here: its status moves, its refund projection
--                          grows, and its state_version says which write a
--                          reader's expectation belongs to. Everything else
--                          about it is frozen at insert — whose money it is,
--                          how much, in what unit, and the two provider
--                          references the later messages are matched on.
--
--     payment_events       one row per VERIFIED delivery whose signature
--                          check passed — the idempotency ledger, and the
--                          reason a redelivered event is a duplicate instead
--                          of a second credit. Append-only.
--
--     payment_quarantine   one row per delivery this build could NOT apply.
--                          Append-only.
--
--   The dedup key is (provider, provider_account_key, provider_event_id) and
--   it is the whole of `payment_events`' purpose. The provider account is IN
--   the key and not a decoration: a provider that scopes its event ids per
--   merchant account hands two different customers the same event id, and a
--   global uniqueness claim on (provider, event_id) would then refuse the
--   second customer's insert, absorb it, and leave a paying customer unfunded
--   with no error anywhere. Carrying the merchant makes that collision
--   structurally impossible instead of merely unlikely. The key is added as a
--   NAMED constraint rather than a bare index because the Go adapter switches
--   on the constraint name to turn the violation into the domain's duplicate
--   sentinel.
--
--   Why `payment_quarantine` is a SEPARATE TABLE and not a disposition value
--   on `payment_events`: an unverifiable delivery's event id is NOT
--   trustworthy — that is what "unverifiable" means — so it cannot go in a
--   table whose uniqueness rests on that id being real. Filing it there would
--   mean inventing an id, and inventing an id to store an unauthenticated
--   body is exactly the forgery vector the signature check exists to prevent.
--   The quarantine is where the refusal itself is the record.
--
--   Deliberately absent, and recorded rather than discovered:
--     * no uniqueness at all on the quarantine, and therefore no reference
--       column in ANY key: two identical unapplied deliveries are two rows, and
--       an operator reading them learns the provider tried twice. The claimed
--       payment reference IS carried on the row (`provider_payment_ref`), but
--       as evidence beside the raw payload rather than as identity — see the
--       column comment for why an unknown_payment row would otherwise be
--       resolvable only by decoding bytes;
--     * no account or bucket column on the quarantine: a delivery that could
--       not be resolved to a payment has no account this plane is entitled to
--       name, and a nullable account id would be a column a future writer
--       could fill from a payload;
--     * no DELETE path on `payment_intents` or `payment_events`, and none on
--       `payment_quarantine` either: a payment's record and its deliveries'
--       record are the audit trail this plane would have to produce if a
--       customer disputed a charge.
--
--   Lane conventions, kept here and stated once:
--     * explicit names on every constraint, index, trigger and function, so
--       an error message names what fired and a later migration can name what
--       it alters;
--     * timestamptz everywhere, NOT NULL except where a fact is genuinely
--       absent, supplied by the application from the DATABASE's clock —
--       payments are history and two replicas minting timelines from their
--       own clocks produce two timelines nothing can interleave;
--     * money is integer MINOR UNITS only, with the currency and its exponent
--       carried beside the figure. There is no float, no decimal and no
--       formatted amount anywhere in this file: a currency's exponent is what
--       turns 1000 into a thousand yen or ten dollars, so an amount without
--       its unit is a number whose size is unknown;
--     * ids this plane MINTS are version-7 uuids, shape-checked exactly as
--       000006 checks its own. The two exceptions are recorded where they
--       occur below: `account_id` carries no v7 CHECK because identity mints
--       accounts as version-4 uuids, and the two append-only tables use
--       `bigint GENERATED ALWAYS AS IDENTITY` because a delivery is an
--       infrastructure record with no domain identity and nothing mints ids
--       for it — 000009's own precedent for `reconciliation_runs`;
--     * no ON DELETE CASCADE anywhere: every foreign key here is RESTRICT by
--       default on purpose.
--
--   The file carries no BEGIN, COMMIT or ROLLBACK of its own, per
--   migrations/README.md: the runner delivers it as one simple query — one
--   implicit transaction — and the migration fails or applies whole. The same
--   rule shapes the plpgsql bodies below: each opens its block on the AS line
--   (`AS $name$ BEGIN`), because a line that opens with BEGIN would read as
--   file-managed transaction control to verify.sh's lane-drift scan. Every
--   body pins `SET search_path = pg_catalog`, for the reason 000009 gives: a
--   trigger runs inside the writing session, and an unqualified name in the
--   body would resolve against whatever search_path the caller set.
--
-- The inverse of this file is 000013_payment_foundation.down.sql, in reverse
-- creation order, no CASCADE.

-- ---------------------------------------------------------------------------
-- payment_intents — one funding top-up this platform initiated.
-- ---------------------------------------------------------------------------
CREATE TABLE control.payment_intents (
    id uuid PRIMARY KEY
        CONSTRAINT payment_intents_id_uuid_v7
        CHECK (id::text ~ '^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'),
    account_id uuid NOT NULL
        CONSTRAINT payment_intents_account_id_fkey
        REFERENCES control.accounts (id),
    -- v7, because it names a funding_buckets row and 000006 mints those as
    -- v7 — the same CHECK the entitlement reference there carries. The
    -- account id above deliberately has none: identity mints accounts as
    -- version-4 uuids, which 000006's funding_buckets.account_id comment
    -- records as the lane's one masked deviation rather than a rule.
    funding_bucket_id uuid NOT NULL
        CONSTRAINT payment_intents_funding_bucket_id_uuid_v7
        CHECK (funding_bucket_id::text ~ '^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$')
        CONSTRAINT payment_intents_funding_bucket_id_fkey
        REFERENCES control.funding_buckets (id),
    amount_minor_units bigint NOT NULL
        CONSTRAINT payment_intents_amount_positive
        CHECK (amount_minor_units > 0),
    -- ISO 4217 in its defined case. Three uppercase ASCII letters and nothing
    -- else: the shape is the whole check, and membership of a list of
    -- currencies is deliberately NOT asserted — a list would be a copy of the
    -- world's currency table that goes stale, and a deployment whose
    -- settlement currency is one this build has not heard of must still be
    -- able to carry a payment in it. What must never be accepted is a
    -- three-character string that is not a code, because it would sail
    -- through every later comparison: the stored value and the provider's
    -- value would be the same wrong string, and the mismatch check would find
    -- them in agreement.
    currency text NOT NULL
        CONSTRAINT payment_intents_currency_grammar
        CHECK (currency ~ '^[A-Z]{3}$'),
    -- How many decimal places this currency's minor unit sits at, stored
    -- rather than derived: USD's hundredth and JPY's whole yen are both
    -- "minor units" and differ by a factor of a hundred, so an amount without
    -- this is a number whose unit is unknown. 0..4 is ISO 4217's whole range.
    minor_unit_exponent smallint NOT NULL
        CONSTRAINT payment_intents_minor_unit_exponent_valid
        CHECK (minor_unit_exponent BETWEEN 0 AND 4),
    -- A bounded GRAMMAR, not an enumeration, and the distinction is the one
    -- reconciliation_runs.scope records at 000009: the vocabulary belongs to
    -- the adapter, it is one value today, and a new value is a code change
    -- that arrives with its own CHECK revision rather than a silent insert.
    provider text NOT NULL
        CONSTRAINT payment_intents_provider_grammar
        CHECK (char_length(provider) BETWEEN 1 AND 64),
    -- The caller's stable key for this logical top-up, unique per account.
    -- Two clicks of one button are one payment, and the unique constraint at
    -- the end of this table is what makes that true rather than merely
    -- intended.
    idempotency_key text NOT NULL
        CONSTRAINT payment_intents_idempotency_key_grammar
        CHECK (char_length(idempotency_key) BETWEEN 1 AND 128),
    -- The state machine's vocabulary, spelled exactly as the domain spells it
    -- (apps/console-api/internal/domain/payments/intent_model.go). The set is
    -- closed here and the EDGES are the transition trigger's business below,
    -- because a CHECK cannot see OLD.
    status text NOT NULL DEFAULT 'created'
        CONSTRAINT payment_intents_status_valid
        CHECK (status IN (
            'created', 'checkout_open', 'requires_action', 'succeeded',
            'failed', 'cancelled', 'expired', 'partially_refunded',
            'refunded', 'quarantined'
        )),
    -- The provider's identifier for the CHECKOUT, written once when the hosted
    -- checkout is opened: it is the lookup key a later delivery is matched on,
    -- which is why an event arriving before it is set is a "not yet" rather
    -- than an "unknown".
    provider_checkout_ref text
        CONSTRAINT payment_intents_provider_checkout_ref_grammar
        CHECK (provider_checkout_ref IS NULL OR char_length(provider_checkout_ref) BETWEEN 1 AND 255),
    -- The provider's identifier for the PAYMENT — the economic event, as
    -- distinct from the delivery that reported it. Written once when the
    -- capture is first seen, and the value the funding leg's command key is
    -- DERIVED from, which is exactly why a second delivery of the same
    -- capture cannot become a second credit.
    provider_payment_ref text
        CONSTRAINT payment_intents_provider_payment_ref_grammar
        CHECK (provider_payment_ref IS NULL OR char_length(provider_payment_ref) BETWEEN 1 AND 255),
    -- The hosted checkout URL, returned by the provider verbatim and never
    -- parsed by anything on this side. The bound is an evidence bound, not a
    -- grammar: a URL is a third party's string and its length is the
    -- provider's business, and this is only a ceiling that keeps an unbounded
    -- third-party value out of the row.
    checkout_url text
        CONSTRAINT payment_intents_checkout_url_evidence
        CHECK (checkout_url IS NULL OR char_length(checkout_url) <= 2048),
    -- The total the provider says it has returned, and the part of THAT
    -- CUMULATIVE total the balance cannot account for. Both are PROJECTIONS of
    -- what the deliveries reported, kept here for the read surface and the
    -- ceiling check; the authoritative total is what the provider reports, on
    -- the same rule funding_buckets' balances follow and for the same reason.
    -- The second is measured against the payment rather than against one
    -- delivery, because the balance is a property of the payment: a figure
    -- taken per delivery would spend the same balance once per refund.
    refunded_minor_units bigint NOT NULL DEFAULT 0
        CONSTRAINT payment_intents_refunded_nonnegative
        CHECK (refunded_minor_units >= 0),
    uncovered_refund_minor_units bigint NOT NULL DEFAULT 0
        CONSTRAINT payment_intents_uncovered_refund_nonnegative
        CHECK (uncovered_refund_minor_units >= 0),
    -- The optimistic-concurrency marker: every move bumps it, and it is what
    -- makes a compare-and-swap that lost distinguishable from one that never
    -- ran.
    state_version bigint NOT NULL DEFAULT 0
        CONSTRAINT payment_intents_state_version_nonnegative
        CHECK (state_version >= 0),
    -- When an un-completed checkout stops being WAITABLE. It is a local
    -- deadline and not a terminal fact about the money: an event arriving
    -- after it is still honoured, because the provider is the only party that
    -- gets to say whether the customer paid.
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,

    -- Two clicks of one button are one payment. This is the index that makes
    -- that true: a retried open-checkout converges on the payment it already
    -- has instead of minting a second one, at the engine rather than in a
    -- handler that might be bypassed.
    CONSTRAINT payment_intents_idempotency_key UNIQUE (account_id, idempotency_key),
    -- The refund ceiling, stated as the schema's own fact: what was returned
    -- can never exceed what was captured. The authoritative arithmetic is the
    -- domain's (RefundCeiling / CanRefund) and this is what holds when the
    -- read that arithmetic was based on is stale.
    CONSTRAINT payment_intents_refunded_within_capture
        CHECK (refunded_minor_units <= amount_minor_units),
    -- The uncovered part is a part OF a refund, so it cannot exceed the
    -- refunds that exist. It is a ceiling and not a sum, deliberately: the
    -- figure names what this platform could not take out of the balance, and
    -- a platform that recorded more uncovered than refunded would be
    -- describing money it never saw returned.
    CONSTRAINT payment_intents_uncovered_refund_within_refund
        CHECK (uncovered_refund_minor_units <= refunded_minor_units),
    -- A capture reference and a captured status are one fact told twice, so
    -- the shape is a biconditional rather than an implication: a payment in
    -- one of the three post-capture states ALWAYS names the capture that put
    -- it there, and no other status names one. The forward direction is the
    -- transition trigger's rule restated for the path the trigger cannot see —
    -- a direct INSERT — because a trigger on UPDATE OF status fires for a
    -- move and not for a birth. The reverse direction is what makes "a second
    -- event describing the same capture cannot become a second credit" hold:
    -- the reference is the value a redelivery converges on, and a payment
    -- that had one while it was still waiting for a capture would be a
    -- payment whose money had two stories. `quarantined` is deliberately NOT
    -- in the set (the domain is explicit that it is a delivery's disposition
    -- and never a state an intent transitions into), and neither are the
    -- abandoned states: a failed or cancelled payment was never captured, so
    -- it has no capture to name.
    CONSTRAINT payment_intents_capture_shape CHECK (
        (provider_payment_ref IS NULL)
        = (status NOT IN ('succeeded', 'partially_refunded', 'refunded'))
    )
);

-- The provider's payment reference is unique PER PROVIDER, and the index is
-- PARTIAL rather than a table constraint, for the reason 000006 gives for the
-- ledger's idempotency keys: PostgreSQL has no partial-unique table-constraint
-- syntax, and this key only exists where its column does. It must be partial
-- for a second reason that is the important one — most rows have no capture
-- yet, and a plain UNIQUE over a nullable column treats every NULL as
-- distinct, so it would not refuse anything at all while looking like a guard.
-- What the index buys is the property the whole integration rests on: one
-- capture is one credit, so a second delivery of the same capture re-derives
-- the same funding command key and converges on the original leg instead of
-- writing a second one.
CREATE UNIQUE INDEX payment_intents_provider_payment_ref_key
    ON control.payment_intents (provider, provider_payment_ref)
    WHERE provider_payment_ref IS NOT NULL;

-- The delivery-resolution read's access path: `WHERE provider = $1 AND
-- provider_checkout_ref = $2`. It is the hottest lookup on this table and the
-- one lookup a third party's payload drives — every verified delivery is
-- resolved through it before anything else happens — and it carries NO account
-- predicate, because a webhook has no session: an event names a provider
-- reference, the reference resolves to a row this platform wrote, and that row
-- carries the account. Without this index the read is a sequential scan of
-- every payment the deployment has ever opened, once per delivery, for the life
-- of the deployment.
--
-- UNIQUE rather than plain, and the uniqueness is a guard as much as a seek:
-- a provider's checkout session belongs to exactly one payment of ours, so two
-- payments claiming one checkout is not a state this table should be able to
-- hold. The second claim loses the insert, and the second checkout the provider
-- opened has no row it could be mistaken for. It is the same shape as
-- payment_intents_provider_payment_ref_key beside it, for the same reason:
-- two provider references of the same kind, each of which resolves to exactly
-- one payment.
--
-- PARTIAL, and the predicate is load-bearing rather than an optimisation. A
-- payment carries no checkout reference until a checkout is opened, so most
-- rows hold NULL here, and a plain UNIQUE over a nullable column treats every
-- NULL as distinct from every other — it would refuse nothing at all while
-- looking like a guard, which is the one thing an index here must not be. The
-- predicate also keeps the rows with no checkout yet out of the index, so it is
-- proportional to the checkouts that exist rather than to the payments that do.
--
-- An INDEX and not a table constraint, for the reason 000006 records for the
-- ledger's idempotency keys: PostgreSQL has no partial-unique
-- table-constraint syntax, so the partial shape and the named constraint are
-- mutually exclusive and the shape wins — it is the one that refuses a second
-- claim on a real checkout. Plain CREATE INDEX, never CONCURRENTLY: this lane's
-- transaction rule forbids non-transactional DDL, and a CONCURRENTLY that
-- failed would leave an invalid index behind rather than nothing.
CREATE UNIQUE INDEX payment_intents_provider_checkout_ref_key
    ON control.payment_intents (provider, provider_checkout_ref)
    WHERE provider_checkout_ref IS NOT NULL;

-- The console's list of an account's payments, newest first — the account
-- predicate is in the WHERE clause and leads, so another account's payment is
-- a row the query never returned.
--
-- The order column is `id` and NOT `created_at`, and the index is written that
-- way because the list's cursor is an id: an index on `created_at` would leave
-- the keyset predicate — `id < $2` — with nothing to seek with, and the plan
-- would sort the account's whole history to answer a page of it. `id` is a
-- version-7 uuid, so its leading forty-eight bits ARE a millisecond timestamp
-- and `id DESC` is already newest-first; it is also unique, which `created_at`
-- is not, so a keyset page break can never land inside a tie. The two orders
-- agree in any case, because the id's timestamp and this row's `created_at`
-- are minted from the same database clock reading.
CREATE INDEX payment_intents_account_id_idx
    ON control.payment_intents (account_id, id DESC);

COMMENT ON TABLE control.payment_intents IS
    'Payment integration (B15): one row per funding top-up this platform initiated with an external provider. Money is credited only by a provider-signed server-to-server webhook resolved against this row — never by a browser redirect — and this row''s account and funding bucket are fixed at creation, because an event resolved an account from a payload would let a third party say whose money moves. The three tables here hold refunds as RECOGNISED AND RECORDED figures rather than booked ledger legs: B6''s algebra cannot represent a debit that drives a settled balance below zero.';
COMMENT ON COLUMN control.payment_intents.account_id IS
    'The account this payment funds, fixed at creation and immutable: an event does not choose it, because the account is a consequence of a stored row this plane wrote with a session''s account id in it. Identity mints account ids as version-4 uuids, so this column carries no v7 CHECK — the deviation funding_buckets.account_id records, repeated here because the account id is the one value on this row that another context owns.';
COMMENT ON COLUMN control.payment_intents.funding_bucket_id IS
    'The account PAYG bucket this payment credits, fixed at creation and immutable. It must be the ACCOUNT branch of funding_buckets'' owner XOR — a top-up is a PAYG movement — and the pairing is enforced by a trigger below because it spans two tables, where no CHECK can state it.';
COMMENT ON COLUMN control.payment_intents.amount_minor_units IS
    'The CANONICAL funding amount: what the ledger will be told, in integer minor units, in `currency`. It is not the provider''s figure and never becomes it — a delivery that reports another amount is quarantined rather than credited, because this plane cannot tell which of the two is wrong.';
COMMENT ON COLUMN control.payment_intents.currency IS
    'The ISO 4217 code this payment was denominated in, carried once here. This is not a violation of the ledger''s no-per-row-currency rule (ADR 0004): the ledger''s currency is the deployment''s single settlement currency, while a payment is denominated in whatever a CUSTOMER was charged, which is a fact about a transaction with a third party. There is no conversion anywhere in this plane and no authority to pick a rate.';
COMMENT ON COLUMN control.payment_intents.minor_unit_exponent IS
    'How many decimal places `currency` has, stored rather than derived: 0..4 is ISO 4217''s range (JPY at 0, CLF at 4), and the exponent is what turns 1000 into a thousand yen or ten dollars. It travels with the payment so a deployment offering a currency this build has never heard of can still be checked: the amount sent must equal the amount reported, in the same unit, without this plane knowing which currencies exist.';
COMMENT ON COLUMN control.payment_intents.idempotency_key IS
    'The caller''s stable key for this logical top-up, unique per account: two clicks of one button are one payment. Stored verbatim as the caller sent it — a key is an identifier, so "the same key" means the same bytes, and folding or trimming it here would answer a repeat with a second payment for a client whose own key differed only in case. The bound the domain enforces (1..128 characters) exists so this uniqueness rests on a value the row can always hold.';
COMMENT ON COLUMN control.payment_intents.status IS
    'Where the payment stands, in the domain''s own vocabulary. The legal MOVES between these values are enforced by a trigger below, because a CHECK cannot see OLD; `expired` and `cancelled` are not terminal, because expiry and cancellation are this platform''s local decisions about its own patience and the provider alone states the money sentence.';
COMMENT ON COLUMN control.payment_intents.provider_checkout_ref IS
    'The provider''s identifier for the hosted CHECKOUT, write-once: NULL until the checkout is opened, then never re-pointed. It is the key a delivery is resolved by, and a payment whose checkout reference moved would silently stop matching the messages about its own checkout.';
COMMENT ON COLUMN control.payment_intents.provider_payment_ref IS
    'The provider''s identifier for the PAYMENT — the economic event, as distinct from the delivery that reported it — write-once, unique per provider where present, and the value the funding leg''s command key is derived from. Kept distinct from the delivery id on purpose: a capture has several deliveries, and keying the credit on a delivery is one credit per delivery.';
COMMENT ON COLUMN control.payment_intents.checkout_url IS
    'The provider''s hosted-checkout URL, verbatim and never parsed. It is stored so a returning customer can be sent back to the checkout they started — an affordance, never evidence: nothing about a redirect authenticates a payment, which is why credit comes from the webhook alone.';
COMMENT ON COLUMN control.payment_intents.refunded_minor_units IS
    'The total the provider reports it has returned, a projection of the refund deliveries rather than a second authority. Bounded by the captured amount (payment_intents_refunded_within_capture), so a refund above the capture is refused rather than clamped.';
COMMENT ON COLUMN control.payment_intents.uncovered_refund_minor_units IS
    'The part of the CUMULATIVE refunded total the bucket''s balance cannot account for, as the last refund delivery had it measured. It is a recorded claim, not a booked adjustment: this plane books NO refund leg, because B6''s algebra refuses any adjustment that would drive a balance below zero and a customer refunded after spending has been paid back, so there is no entry that tells the truth. The figure is what an operator resolves the case with — measured against the payment rather than against one delivery, so two refunds on one payment cannot each spend the same balance.';
COMMENT ON COLUMN control.payment_intents.state_version IS
    'Optimistic-concurrency marker: every guarded status or projection write bumps it, so a compare-and-swap that lost is distinguishable from one that never ran. It exists so a reader can tell a stale read from a fresh one without holding a lock.';
COMMENT ON COLUMN control.payment_intents.expires_at IS
    'When an un-completed checkout stops being WAITABLE — a local deadline, not a terminal fact about the money. A capture reported after it is still honoured and still funded (the expired -> succeeded edge), because a customer who paid thirty seconds after a local timer fired has paid.';
COMMENT ON COLUMN control.payment_intents.created_at IS
    'When this platform opened the payment, read from the DATABASE''s clock and supplied by the application. The intent is durable BEFORE the provider is called, because the provider''s idempotency key is derived from this row''s identity: a key derived from something that does not exist until the provider answers cannot make a retry the same request.';

-- ---------------------------------------------------------------------------
-- payment_events — the idempotency ledger: one row per VERIFIED delivery.
-- ---------------------------------------------------------------------------
CREATE TABLE control.payment_events (
    id bigint GENERATED ALWAYS AS IDENTITY,
    provider text NOT NULL
        CONSTRAINT payment_events_provider_grammar
        CHECK (char_length(provider) BETWEEN 1 AND 64),
    -- Part of the dedup key, and the reason the key is three columns rather
    -- than two. See the column comment below and the table comment.
    provider_account_key text NOT NULL DEFAULT ''
        CONSTRAINT payment_events_provider_account_key_grammar
        CHECK (char_length(provider_account_key) <= 128),
    provider_event_id text NOT NULL
        CONSTRAINT payment_events_provider_event_id_grammar
        CHECK (char_length(provider_event_id) BETWEEN 1 AND 255),
    provider_payment_ref text
        CONSTRAINT payment_events_provider_payment_ref_grammar
        CHECK (provider_payment_ref IS NULL OR char_length(provider_payment_ref) BETWEEN 1 AND 255),
    -- The payment this delivery resolved to, by identifier. Nullable because a
    -- delivery that named no payment this platform ever opened is recorded
    -- too: the row is the evidence of the refusal, and the operator's queue is
    -- where it is answered.
    intent_id uuid
        CONSTRAINT payment_events_intent_id_fkey
        REFERENCES control.payment_intents (id),
    -- The provider's own vocabulary for the delivery, carried through as the
    -- provider wrote it so a later reconciliation can show a customer what the
    -- provider said rather than this platform's paraphrase of it.
    --
    -- NULLABLE, and the same bound and the same grammar as the sibling column on
    -- payment_quarantine because it is the same value. A delivery whose event
    -- type is absent, is not a string, or is longer than 128 bytes is a real
    -- delivery this build cannot interpret, and the answer for it is a
    -- quarantine with reason unknown-kind. That answer needs a row to live on,
    -- and a NOT NULL column would make the one delivery most worth recording the
    -- one that cannot be recorded — a check violation nothing translates, a
    -- rolled-back transaction, and a provider retrying the same bytes forever.
    -- NULL says "the provider named no type we could read", which is a fact
    -- about the delivery rather than a gap in the row.
    kind text
        CONSTRAINT payment_events_kind_grammar
        CHECK (kind IS NULL OR char_length(kind) BETWEEN 1 AND 128),
    -- Absent is NOT zero. The port's money fields are pointers for exactly
    -- this reason: a delivery that reports no amount and a delivery that
    -- reports an amount of zero are different sentences, and only one of them
    -- is a payment. A column that collapsed the two would let the second be
    -- read as the first at every layer above.
    amount_minor_units bigint,
    currency text
        CONSTRAINT payment_events_currency_grammar
        CHECK (currency IS NULL OR currency ~ '^[A-Z]{3}$'),
    -- What this plane did with the delivery. The three settled values are
    -- genuinely different states of the world rather than three spellings of
    -- success, and a single boolean would lose the distinction an operator
    -- needs: an applied delivery carried its claim out, a duplicate was already
    -- recorded and its effect, if any, is already durable, and a quarantined one
    -- was authenticated and recorded but deliberately NOT acted on — its reason
    -- and the bytes it arrived with are on control.payment_quarantine.
    --
    -- The FOURTH value is the provisional one this row is INSERTed with, and it
    -- is the only value a reader can never see. It exists because the shape of
    -- the delivery path forces the row to be written before this plane knows
    -- what the payment will turn out to be: the insert is the arbiter of the
    -- race between two concurrent deliveries of one event id, so it must
    -- happen first, and a delivery that claims its dedup key and is then
    -- refused is a real row with a real verdict. Nothing outside the
    -- transaction can observe the provisional value — no other session sees the
    -- row until the commit, and the commit carries the verdict.
    --
    -- It is in the CHECK rather than left to a DEFAULT, deliberately. A DEFAULT
    -- would let the admitted UPDATE move a settled row BACK to 'recorded' —
    -- 'recorded' to 'recorded' is a change, so the guard's disposition
    -- comparison alone would wave it through — and it would let an INSERT
    -- written without the column sit permanently provisional. With the value
    -- in the CHECK there is one closed vocabulary of five, the guard branches
    -- on the same literal, and a row is either a claim or a verdict.
    --
    -- A later transaction cannot revise a verdict: the admitted UPDATE is
    -- refused on any row that already carries one, so the application settles
    -- only the delivery its own unit of work recorded.
    disposition text NOT NULL
        CONSTRAINT payment_events_disposition_valid
        CHECK (disposition IN ('recorded', 'applied', 'duplicate', 'quarantined')),
    -- When the PROVIDER observed the outcome, as the signed bytes stated it —
    -- or NULL when the delivery stated no such instant. Nullable for the same
    -- reason payment_quarantine.occurred_at is, and the two must agree: the
    -- adapter that reads a delivery records a zero instant for a body that
    -- carries no readable `created`, so a NOT NULL column would be handed that
    -- absence and store year 1 — a claim the provider never made, in a shape
    -- that passes every IS NULL a reader writes to find the rows that said
    -- nothing. It is NOT part of the replay defence either way: the freshness
    -- bound is computed from the signature header's timestamp, and the replay
    -- defence is this table's uniqueness.
    occurred_at timestamptz,
    recorded_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT payment_events_pkey PRIMARY KEY (id),
    -- THE dedup key, and the reason this table exists. It is a named
    -- constraint rather than a bare index because the adapter switches on the
    -- name: a redelivery is not a failure, it is the domain's duplicate
    -- sentinel, and a caller that had to match a driver string would be
    -- branching on a message rather than a condition.
    CONSTRAINT payment_events_delivery_key
        UNIQUE (provider, provider_account_key, provider_event_id)
);

-- The other question this table is asked: what has arrived for one payment,
-- newest first — the shape the reconciliation and console surfaces read. The
-- dedup key above already indexes the delivery direction; this is the
-- payment's direction, and it is not the same walk.
CREATE INDEX payment_events_intent_recorded_idx
    ON control.payment_events (intent_id, recorded_at DESC);

COMMENT ON TABLE control.payment_events IS
    'Payment integration (B15): one row per VERIFIED provider delivery — the idempotency ledger, written FIRST in the webhook''s unit of work so that a crash rolls the recording and the credit back together and a redelivery re-runs. Uniqueness is (provider, provider_account_key, provider_event_id) and it is what makes a second delivery of one capture a duplicate rather than a second credit. Append-only: nothing here is ever removed, and the one UPDATE that exists — the disposition the delivery path settles once it knows the verdict — runs in the same transaction as the INSERT, so a reader never sees the provisional value.';
COMMENT ON COLUMN control.payment_events.provider_account_key IS
    'The provider''s own identifier for the merchant account this delivery belongs to, and PART OF THE DEDUP KEY. A provider that scopes its event ids per merchant account hands two different customers the same event id, and a global uniqueness claim on (provider, event_id) would then refuse the second customer''s insert, absorb it, and leave a paying customer unfunded with no error anywhere. Carrying the merchant makes that collision structurally impossible instead of merely unlikely.';
COMMENT ON COLUMN control.payment_events.provider_event_id IS
    'The provider''s delivery identity: what makes a redelivery a duplicate. It is NOT the payment''s identity — a provider routinely emits several distinct events for one payment, and keying a credit on this would fund that payment once per event.';
COMMENT ON COLUMN control.payment_events.provider_payment_ref IS
    'The provider''s identifier for the PAYMENT this delivery claimed, recorded even on a row that was not applied, because the claim is the evidence an operator resolves the row with — a quarantine that discarded the figure would be a mystery rather than a question.';
COMMENT ON COLUMN control.payment_events.kind IS
    'The provider''s own event kind, carried verbatim. The column is a bounded string and never an enumeration: a Go enum here would be a copy of a list the provider owns and would go stale the day the provider adds a member, and the APPLICATION is what decides whether it recognises a kind.';
COMMENT ON COLUMN control.payment_events.amount_minor_units IS
    'The provider''s reported amount, in integer minor units, or NULL when the delivery reported none. NULL is not zero: an absent amount is a refusal, not a payment of nothing. The row keeps it even when the delivery was not applied, because the figure is the evidence.';
COMMENT ON COLUMN control.payment_events.disposition IS
    'applied (this delivery carried its claim out), duplicate (it was already recorded and its effect, if any, is already durable) or quarantined (authenticated and recorded but deliberately not acted on — the reason and the bytes are on control.payment_quarantine). NULL would be a fourth state nothing can produce, so the column is NOT NULL and the set is closed.';
COMMENT ON COLUMN control.payment_events.occurred_at IS
    'When the provider observed the outcome, as its signed bytes stated it — the provider''s fact, not this plane''s — or NULL when the delivery stated no such instant. It bounds how long a captured delivery stays useful on RECEIPT; it is not the replay defence, which is this table''s uniqueness.';
COMMENT ON COLUMN control.payment_events.recorded_at IS
    'When this plane recorded the delivery, defaulted from the database''s clock. Server clock rather than the provider''s, because it is this plane''s own act that this instant names.';

-- ---------------------------------------------------------------------------
-- payment_quarantine — one row per delivery this build could NOT apply.
-- ---------------------------------------------------------------------------
CREATE TABLE control.payment_quarantine (
    id bigint GENERATED ALWAYS AS IDENTITY,
    provider text NOT NULL
        CONSTRAINT payment_quarantine_provider_grammar
        CHECK (char_length(provider) BETWEEN 1 AND 64),
    provider_account_key text NOT NULL DEFAULT ''
        CONSTRAINT payment_quarantine_provider_account_key_grammar
        CHECK (char_length(provider_account_key) <= 128),
    -- NULLABLE, and the nullability is the design: this is the table for a
    -- delivery that did not verify, and an unverifiable delivery may have no
    -- trustworthy id at all. A NOT NULL column here would force this plane to
    -- invent one — see the header — so the absence is carried as an absence.
    --
    -- Same bound as the sibling column on payment_events, because it is the
    -- same value: the provider's own id for the delivery, read from the same
    -- signed body. A table with no uniqueness is still a table that can be
    -- handed an unbounded string, and the column an operator reads to find a
    -- delivery is the last place to discover that.
    provider_event_id text
        CONSTRAINT payment_quarantine_provider_event_id_grammar
        CHECK (provider_event_id IS NULL OR char_length(provider_event_id) BETWEEN 1 AND 255),
    -- The provider's identifier for the payment this delivery CLAIMED, when it
    -- named one. It is evidence and never a key — this table has no uniqueness
    -- at all, so the column resolves nothing — and it is here because of the
    -- case the quarantine exists for: a delivery naming a payment this platform
    -- never opened (unknown_payment) is identified by NOTHING ELSE, and without
    -- this column an operator can only recover which payment the provider was
    -- talking about by decoding the raw payload. Same bound and same grammar as
    -- the sibling column on payment_events, because it is the same value.
    provider_payment_ref text
        CONSTRAINT payment_quarantine_provider_payment_ref_grammar
        CHECK (provider_payment_ref IS NULL OR char_length(provider_payment_ref) BETWEEN 1 AND 255),
    intent_id uuid
        CONSTRAINT payment_quarantine_intent_id_fkey
        REFERENCES control.payment_intents (id),
    kind text
        CONSTRAINT payment_quarantine_kind_grammar
        CHECK (kind IS NULL OR char_length(kind) BETWEEN 1 AND 128),
    -- The refusal vocabulary, and it is a closed set rather than a formatted
    -- sentence on purpose: a reason carrying provider text into a column an
    -- operator surface renders is a cross-site-scripting vector built into the
    -- storage layer, and the cheapest way to prevent it is for the value never
    -- to have been able to hold anything else. The values are the domain's own
    -- QuarantineReason enumeration, and the SET of them is legible in one
    -- place — a deployment that wants an alert on one of these names it.
    reason text NOT NULL
        CONSTRAINT payment_quarantine_reason_valid
        CHECK (reason IN (
            'unverifiable', 'unknown_kind', 'unknown_payment',
            'amount_mismatch', 'currency_mismatch', 'stale',
            'state_conflict', 'account_closed', 'bucket_closed',
            'refund_ahead_of_capture', 'refund_ceiling'
        )),
    amount_minor_units bigint,
    -- Same grammar as the sibling column on payment_events, because it is the
    -- same value read from the same signed body. It is NULL where the delivery
    -- stated none, and a delivery that stated a currency this plane cannot
    -- compare is exactly what the currency_mismatch refusal is for — so the
    -- alphabet is enforced here rather than left to the comparison to notice.
    currency text
        CONSTRAINT payment_quarantine_currency_grammar
        CHECK (currency IS NULL OR currency ~ '^[A-Z]{3}$'),
    -- The raw AUTHENTICATED BYTES of the delivery, and `bytea` rather than
    -- `text` because they are not necessarily UTF-8: a signature is computed
    -- over bytes, and a column that insisted on valid text would either
    -- refuse a legitimate body or re-encode it into something the signature no
    -- longer covers. The bound is an EVIDENCE bound rather than a grammar —
    -- the shape reconciliation_findings_subject_id_evidence uses for the same
    -- reason — and there is deliberately NO lower bound: a refusal is not
    -- refused for being small. It is NULL for a signature that did not verify,
    -- because an unauthenticated body is an attacker's free text and storing
    -- it verbatim would make this table somewhere to put things nobody chose
    -- to store.
    payload bytea
        CONSTRAINT payment_quarantine_payload_evidence
        CHECK (payload IS NULL OR octet_length(payload) <= 65536),
    occurred_at timestamptz,
    recorded_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT payment_quarantine_pkey PRIMARY KEY (id)
);

-- The operator's queue: what has arrived and could not be applied, newest
-- first. There is no uniqueness beside it — see the header: two identical
-- unapplied deliveries are two rows, and an operator reading them learns the
-- provider tried twice.
CREATE INDEX payment_quarantine_recorded_idx
    ON control.payment_quarantine (recorded_at DESC);

COMMENT ON TABLE control.payment_quarantine IS
    'Payment integration (B15): one row per provider delivery this build could NOT apply — a signature that did not verify, an event kind it does not interpret, an amount or currency mismatch, a state conflict, a refund ahead of its capture. A SEPARATE TABLE from payment_events and not a disposition value on it: an unverifiable delivery''s event id is not trustworthy, so it cannot live in a table whose uniqueness rests on that id being real, and inventing an id to file it would be the forgery vector the signature check exists to prevent. Append-only.';
COMMENT ON COLUMN control.payment_quarantine.reason IS
    'Why this delivery was not applied, from the domain''s closed QuarantineReason set. A fixed vocabulary rather than a formatted sentence: the column is rendered on an operator surface, and a value that could hold provider text is a cross-site-scripting vector stored at rest. Enumerating them also makes the SET of failure modes legible — a deployment that wants an alert on one names it.';
COMMENT ON COLUMN control.payment_quarantine.payload IS
    'The delivery''s raw authenticated bytes, bytea because a signed body is not necessarily UTF-8. Bounded at 64 KiB as EVIDENCE, with no lower bound: a refusal is not refused for being small. NULL for an unverifiable delivery, deliberately — an unauthenticated body is an attacker''s free text and is not kept.';
COMMENT ON COLUMN control.payment_quarantine.provider_event_id IS
    'The provider''s delivery id when one was readable, or NULL. Nullable because an unverifiable delivery may have no trustworthy id, and a NOT NULL column here would force this plane to invent one.';
COMMENT ON COLUMN control.payment_quarantine.intent_id IS
    'The payment this delivery was resolved to, when it resolved to one. NULL for a delivery that named no payment this platform opened, which is the unknown_payment refusal: this plane does not credit a payment it has no record of creating.';
COMMENT ON COLUMN control.payment_quarantine.provider_payment_ref IS
    'The provider''s identifier for the payment this delivery claimed, recorded as claimed and never acted on. Not a key — this table has no uniqueness — and it matters most for the refusal it was written for: an unknown_payment delivery names a payment this plane never opened, and this reference is that row''s only identifying evidence. Without it an operator resolves the row by decoding the raw payload, which is exactly the work the column exists to remove.';
COMMENT ON COLUMN control.payment_quarantine.provider_account_key IS
    'The merchant account the delivery claimed to belong to, when it said so. It is carried for the operator''s benefit and is NOT part of any key here — a quarantine has no uniqueness at all, which is the difference between this table and payment_events: two identical unapplied deliveries are two rows, and an operator reading them learns the provider tried twice.';
COMMENT ON COLUMN control.payment_quarantine.recorded_at IS
    'When this plane recorded the refusal, defaulted from the database''s clock. It is the operator queue''s order, and it is this plane''s act rather than any claim the delivery made.';
COMMENT ON COLUMN control.payment_quarantine.occurred_at IS
    'When the provider said it observed the outcome, when the delivery said so at all. Nullable for the same reason provider_event_id is: an unverifiable delivery''s claims are not facts this plane asserts.';

-- ---------------------------------------------------------------------------
-- Engine guards: the bucket a payment credits is the payer's own.
-- ---------------------------------------------------------------------------
-- The pairing of (account_id, funding_bucket_id) spans two tables, so no CHECK
-- can state it; the trigger can. It requires the named bucket to EXIST and to
-- have this payment's account_id, and it is a same-column equality rather than
-- the two-hop entitlement -> subscription resolution 000011 indexes: that
-- resolution exists to find the owner of a CYCLE bucket, and a payment must
-- never name one. The funding_buckets owner XOR is what makes the equality
-- sufficient — a bucket has either an entitlement_id or an account_id, never
-- both — and Accounting.TopUp refuses a cycle bucket by definition, because a
-- top-up is a PAYG movement.
CREATE FUNCTION control.payment_intents_funding_bucket_owner() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
    AS $payment_intents_funding_bucket_owner$ BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM control.funding_buckets b
        WHERE b.id = NEW.funding_bucket_id AND b.account_id = NEW.account_id
    ) THEN
        -- Before-row triggers run before the constraint checks, so a value
        -- that is not this guard's to judge is passed through to the
        -- constraint that owns it: a bucket that does not exist at all is the
        -- foreign key's refusal (or the v7 grammar's, for a value that could
        -- not name any bucket), and only a real bucket with the wrong owner is
        -- this guard's to name. 000006's PAYG owner guard states the same
        -- pre-check for the same reason.
        IF NOT EXISTS (
            SELECT 1 FROM control.funding_buckets b
            WHERE b.id = NEW.funding_bucket_id
        ) THEN
            RETURN NEW;
        END IF;
        RAISE EXCEPTION USING
            MESSAGE = 'control.payment_intents.funding_bucket_id must name a bucket owned by the payment''s account',
            DETAIL = 'the refused value names a funding bucket that is not account ' || NEW.account_id || '''s. A bucket has exactly one owner (funding_buckets_owner_xor): either an entitlement''s cycle bucket or an account''s PAYG bucket. A top-up is a PAYG movement, so the bucket this payment names must be the ACCOUNT branch and must belong to the paying account — an edge into someone else''s bucket is an edge into someone else''s money. Reaching for the entitlement -> subscription hop 000011 indexes would be the wrong resolution here, because a payment must never name a cycle bucket at all',
            ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$payment_intents_funding_bucket_owner$;

CREATE TRIGGER payment_intents_funding_bucket_owner_guard
    BEFORE INSERT OR UPDATE OF funding_bucket_id, account_id ON control.payment_intents
    FOR EACH ROW
    EXECUTE FUNCTION control.payment_intents_funding_bucket_owner();

COMMENT ON FUNCTION control.payment_intents_funding_bucket_owner() IS
    'Payment engine guard: the bucket a payment credits belongs to the payment''s own account. The pairing spans funding_buckets and payment_intents, so no CHECK can state it, and an edge into someone else''s bucket is an edge into someone else''s money.';

-- ---------------------------------------------------------------------------
-- Engine guards: the status machine, stated for the writer this build did not
-- write.
-- ---------------------------------------------------------------------------
-- A CHECK cannot see OLD, so the legal-transition rule needs a BEFORE UPDATE
-- trigger. The rule is the domain's own legalEdges table
-- (apps/console-api/internal/domain/payments/state.go), reproduced here keyed
-- by the source state exactly as that map is, and the plpgsql copy is not
-- redundancy for its own sake: the Go rule holds for a writer this build
-- wrote, and this one holds for every writer including raw SQL.
--
-- The trigger fires only when the status actually CHANGES, so a write that
-- restates the state it found passes, which is what makes the compare-and-swap
-- statements above it able to lose without being refused.
CREATE FUNCTION control.payment_intents_status_transition() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
    AS $payment_intents_status_transition$ BEGIN
    IF NEW.status IS DISTINCT FROM OLD.status THEN
        IF NOT (
            (OLD.status = 'created' AND NEW.status IN ('checkout_open', 'failed', 'cancelled'))
            OR (OLD.status = 'checkout_open' AND NEW.status IN ('requires_action', 'succeeded', 'failed', 'cancelled', 'expired'))
            OR (OLD.status = 'requires_action' AND NEW.status IN ('checkout_open', 'succeeded', 'failed', 'cancelled'))
            OR (OLD.status IN ('expired', 'cancelled') AND NEW.status = 'succeeded')
            OR (OLD.status = 'succeeded' AND NEW.status IN ('partially_refunded', 'refunded'))
            OR (OLD.status = 'partially_refunded' AND NEW.status IN ('refunded', 'partially_refunded'))
            OR (OLD.status = 'refunded' AND NEW.status = 'refunded')
        ) THEN
            RAISE EXCEPTION USING
                MESSAGE = 'control.payment_intents has no transition from ' || OLD.status || ' to ' || NEW.status,
                DETAIL = 'the refused move names no legal edge of this payment''s state machine. TWO of the legal ones are worth knowing because they look wrong and are not: expired -> succeeded and cancelled -> succeeded are legal because expiry and cancellation are LOCAL decisions about this platform''s patience, while the provider alone states the money sentence — a customer who paid thirty seconds after a local timer fired must still be funded. And refunded -> refunded is idempotent on itself because a full refund arriving after a partial one is the SAME refund completed, not a second one. What is NOT legal in the other direction: there is no edge out of failed, that state is terminal, and there is no path back from refunded to succeeded or from succeeded to succeeded — a payment reported captured again after its final word is a provider contradicting itself, and the caller records it as a quarantine rather than guessing',
                ERRCODE = 'integrity_constraint_violation';
        END IF;
        -- A payment cannot be succeeded without naming the capture that
        -- succeeded it: the reference is what the funding leg's command key is
        -- derived from, so a succeeded payment without one is a payment whose
        -- credit cannot be re-derived and whose redelivery would fund it
        -- again. The CHECK on the table states the same thing for the path a
        -- trigger on UPDATE OF status cannot see — a direct INSERT — and the
        -- two are one rule told from the two directions a row can arrive from.
        IF NEW.status = 'succeeded' AND NEW.provider_payment_ref IS NULL THEN
            RAISE EXCEPTION USING
                MESSAGE = 'control.payment_intents cannot reach succeeded without the capture that succeeded it',
                DETAIL = 'the refused move would have marked a payment captured while naming no provider payment reference: this plane credits a payment by resolving the provider''s own reference to this row, and a succeeded payment without one is a payment whose funding leg cannot be re-derived, so a later redelivery of the same capture would be unable to converge on the leg it already wrote',
                ERRCODE = 'integrity_constraint_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$payment_intents_status_transition$;

CREATE TRIGGER payment_intents_status_transition_guard
    BEFORE UPDATE OF status ON control.payment_intents
    FOR EACH ROW
    EXECUTE FUNCTION control.payment_intents_status_transition();

COMMENT ON FUNCTION control.payment_intents_status_transition() IS
    'Payment engine guard: the legal status edges, enforced for every writer including raw SQL because a CHECK cannot see OLD. expired -> succeeded and cancelled -> succeeded are legal — expiry and cancellation are local decisions, the provider states the money — and a payment may not reach succeeded without the capture reference that funded it.';

-- The identity-immutability guard. It refuses any change to the columns that
-- were decided when the payment was opened, comparing them row-wise with IS
-- DISTINCT FROM so a NULL on either side is a difference rather than a
-- comparison that never fires — 000009''s idiom, unchanged. The comparison is
-- spelled as a tuple of row constructors so the whole identity is one
-- expression: adding a column to this table later means adding it here too,
-- and a column not named here is frozen.
--
-- The two provider references are WRITE-ONCE rather than immutable, and the
-- difference matters: each is NULL until the message that carries it arrives,
-- and NULL -> value is the write this table exists to receive. value ->
-- different value is refused, because a payment whose capture reference moved
-- would be a payment whose money has two stories, and a checkout reference
-- that moved would stop matching the deliveries about its own checkout.
CREATE FUNCTION control.payment_intents_identity_immutability() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
    AS $payment_intents_identity_immutability$ BEGIN
    IF (NEW.id, NEW.account_id, NEW.funding_bucket_id, NEW.amount_minor_units,
        NEW.currency, NEW.minor_unit_exponent, NEW.provider,
        NEW.idempotency_key, NEW.created_at, NEW.expires_at)
       IS DISTINCT FROM
       (OLD.id, OLD.account_id, OLD.funding_bucket_id, OLD.amount_minor_units,
        OLD.currency, OLD.minor_unit_exponent, OLD.provider,
        OLD.idempotency_key, OLD.created_at, OLD.expires_at) THEN
        RAISE EXCEPTION USING
            MESSAGE = 'control.payment_intents is immutable in its identity: only status, checkout_url, the refund projection, state_version and updated_at may change',
            DETAIL = 'the refused change would have rewritten whose money this payment funds, how much, in what unit, or when it was opened — and those were decided before the customer was ever charged. The account and the funding bucket are fixed at creation because a webhook resolves them FROM THIS ROW: a payment that could change hands is a payment whose ledger legs belong to one account and whose money landed in another, and an edited amount is a charge this platform never priced. The error deliberately does not restate the row''s values, because an amount and an account id are not things an operator-facing log line has any business quoting',
            ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.provider_checkout_ref IS NOT NULL
        AND NEW.provider_checkout_ref IS DISTINCT FROM OLD.provider_checkout_ref THEN
        RAISE EXCEPTION USING
            MESSAGE = 'control.payment_intents.provider_checkout_ref is write-once: it may be set from NULL, never re-pointed',
            DETAIL = 'the refused change would have re-pointed the provider''s CHECKOUT reference on an existing payment. That reference is the key a delivery is resolved by: a second value would silently stop matching the messages about the checkout this payment actually opened, and — worse in the other direction — a value swapped in from another payment would make this row the resolution target for a checkout it never opened',
            ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.provider_payment_ref IS NOT NULL
        AND NEW.provider_payment_ref IS DISTINCT FROM OLD.provider_payment_ref THEN
        RAISE EXCEPTION USING
            MESSAGE = 'control.payment_intents.provider_payment_ref is write-once: it may be set from NULL, never re-pointed',
            DETAIL = 'the refused change would have re-pointed which provider payment this row is the capture of, and the funding leg''s command key is DERIVED from that reference: a second value would derive a second key, and the same capture would be credited a second time. NULL -> value is the write this table exists to receive and is not refused',
            ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$payment_intents_identity_immutability$;

CREATE TRIGGER payment_intents_immutability_guard
    BEFORE UPDATE ON control.payment_intents
    FOR EACH ROW
    EXECUTE FUNCTION control.payment_intents_identity_immutability();

COMMENT ON FUNCTION control.payment_intents_identity_immutability() IS
    'Payment engine guard: a payment''s identity — whose money, how much, in what unit, which provider, which key, when it was opened — and its expiry are written once, and both provider references are write-once (NULL -> value allowed, value -> different value refused). What the two write-once arms protect is the derivation: the command key a funding leg carries is derived from the capture reference, so a re-pointed reference is a second credit.';

-- The delete guard, and it is unconditional — there is no sanctioned delete on
-- this table, so the trigger is one statement with no condition. It fires
-- BEFORE DELETE FOR EACH STATEMENT: a statement-level guard costs nothing on
-- the path that matters (a passing UPDATE fires no DELETE trigger at all) and
-- cannot be defeated by a delete of zero rows. A payment is closed by its
-- status and never removed: the row is the audit trail this plane would have
-- to produce if a customer disputed a charge.
CREATE FUNCTION control.reject_payment_intent_delete() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
    AS $reject_payment_intent_delete$ BEGIN
    RAISE EXCEPTION USING
        MESSAGE = 'control.payment_intents rows are never removed',
        DETAIL = 'the refused operation was a ' || TG_OP || ': a payment that was opened and then deleted is a charge with no record of what it was for, and a payment is closed by its status rather than dropped',
        ERRCODE = 'integrity_constraint_violation';
    RETURN NULL;
END;
$reject_payment_intent_delete$;

CREATE TRIGGER payment_intents_no_delete
    BEFORE DELETE ON control.payment_intents
    FOR EACH STATEMENT
    EXECUTE FUNCTION control.reject_payment_intent_delete();

COMMENT ON FUNCTION control.reject_payment_intent_delete() IS
    'Payment engine guard: a payment is never removed. Closed is a status, not an absence, and a table that could only lose rows could not answer "what did this customer pay for".';

-- ---------------------------------------------------------------------------
-- Engine guards on the two evidence tables: recorded once, never revised.
-- ---------------------------------------------------------------------------
-- Both triggers are stated in the two places the lane states an append-only
-- rule. The row-level one is the speaking guard: it names the operation and
-- the table when an UPDATE touches a row. The statement-level one is the
-- DELETE refusal, and it is the one that fires first — a statement-level
-- BEFORE trigger runs before any row-level one — so on the delete path the
-- row-level guard never runs at all. The arm is stated rather than omitted so
-- each trigger is complete in itself and a reader does not have to
-- reconstruct the firing order to know what this table refuses.
--
-- THE ONE UPDATE THIS TABLE ADMITS IS WRITTEN HERE, AND IT IS NOT A LOOPHOLE.
-- The rule this guard enforces is "recorded once, never revised ACROSS
-- TRANSACTIONS", and the amendment that states it in the function's own
-- DETAIL text — admitted, refused, and what a refusal must look like. Without
-- the function it admits anything, and a caller could revise a verdict or
-- rewrite a delivery's payload; with the function, the only UPDATE that can
-- touch this table sets `disposition`, and only from a recorded default to the
-- outcome the recording unit of work then reached.
--
-- `OLD.disposition = 'recorded'` is what makes it a claim rather than an
-- edit: a verdict is written once, and a row that already carries a verdict is
-- refused even by the admitted UPDATE. So a second settlement, a settlement of
-- somebody else's recorded delivery, and a settlement arriving in a later
-- transaction are all the same refusal, and none of them can leave a row
-- claiming an effect the ledger does not show.
--
-- Why the predicate is stated here rather than trusted to the caller. A
-- `WHEN` clause on the trigger was tried and dropped: the guard is a
-- `BEFORE UPDATE` trigger, so a statement whose WHERE excludes the row raises
-- nothing and a statement that includes it raises unless this function
-- branches. The branch has to live in the function, and a predicate on the
-- trigger could only have hidden the branch from the one place the rule is
-- written down.
--
-- What this costs is stated rather than hidden. This is the one guard in the
-- lane that runs PL/pgSQL on the hot path of a provider delivery, where the
-- fast path is an aborted statement and a rollback; the alternative shapes
-- named in ADR 0013 §7 (an append-only column, or a separate verdict table)
-- were rejected as a second source of truth for the same fact.
CREATE FUNCTION control.payment_events_append_only() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
    AS $payment_events_append_only$
DECLARE
    amendment constant text :=
        'This is the one UPDATE control.payment_events admits. It writes the verdict of a delivery that was recorded in this same unit of work, and it cannot change anything else: any other column of NEW differing from OLD, a row that already carries a verdict, or a second settlement, are all refused by the same branch.';
BEGIN
    -- A DELETE arrives here when the row-level trigger beats the statement-level
    -- one (the statement-level guard refuses it first in practice, so this arm
    -- exists to make the function complete rather than because it is reached).
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION USING
            MESSAGE = 'control.payment_events is append-only: ' || TG_OP || ' is refused',
            DETAIL = 'a recorded delivery is the evidence that a redelivery is a duplicate and of which payment it funded. The effect it had is already durable, so nothing here is removed and nothing is revised across transactions: a correction is a new delivery''s business, and a delivery that was recorded and then edited by some later transaction could not answer whether a customer was funded once or twice. ' || amendment,
            ERRCODE = 'integrity_constraint_violation';
        RETURN NULL;
    END IF;

    IF OLD.disposition = 'recorded'
       AND NEW.disposition <> OLD.disposition
       AND (to_jsonb(NEW) - 'disposition') = (to_jsonb(OLD) - 'disposition') THEN
        RETURN NEW;
    END IF;

    RAISE EXCEPTION USING
        MESSAGE = 'control.payment_events is append-only: ' || TG_OP || ' is refused',
        DETAIL = 'a recorded delivery is the evidence that a redelivery is a duplicate and of which payment it funded. The effect it had is already durable, so nothing here is removed and nothing is revised across transactions: a correction is a new delivery''s business, and a delivery that was recorded and then edited by some later transaction could not answer whether a customer was funded once or twice. ' || amendment,
        ERRCODE = 'integrity_constraint_violation';
    RETURN NULL;
END;
$payment_events_append_only$;

CREATE TRIGGER payment_events_append_only_guard
    BEFORE UPDATE OR DELETE ON control.payment_events
    FOR EACH ROW
    EXECUTE FUNCTION control.payment_events_append_only();

CREATE FUNCTION control.reject_payment_event_delete() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
    AS $reject_payment_event_delete$ BEGIN
    RAISE EXCEPTION USING
        MESSAGE = 'control.payment_events rows are never removed',
        DETAIL = 'the refused operation was a ' || TG_OP || ': a delivery that was recorded and then deleted leaves a credit with no evidence of which message produced it, and the whole value of this table is that a redelivery can be answered',
        ERRCODE = 'integrity_constraint_violation';
    RETURN NULL;
END;
$reject_payment_event_delete$;

CREATE TRIGGER payment_events_no_delete
    BEFORE DELETE ON control.payment_events
    FOR EACH STATEMENT
    EXECUTE FUNCTION control.reject_payment_event_delete();

CREATE FUNCTION control.payment_quarantine_append_only() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
    AS $payment_quarantine_append_only$ BEGIN
    RAISE EXCEPTION USING
        MESSAGE = 'control.payment_quarantine is append-only: ' || TG_OP || ' is refused',
        DETAIL = 'a quarantine row is the evidence a refusal produced — the bytes that did not verify, or the claim that could not be resolved — and resolving it is an operator''s act on the delivery, not an edit to the record. A quarantine that could be rewritten in place is not evidence, and the row is the only thing that says what the provider actually sent',
        ERRCODE = 'integrity_constraint_violation';
    RETURN NULL;
END;
$payment_quarantine_append_only$;

CREATE TRIGGER payment_quarantine_append_only_guard
    BEFORE UPDATE OR DELETE ON control.payment_quarantine
    FOR EACH ROW
    EXECUTE FUNCTION control.payment_quarantine_append_only();

CREATE FUNCTION control.reject_payment_quarantine_delete() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
    AS $reject_payment_quarantine_delete$ BEGIN
    RAISE EXCEPTION USING
        MESSAGE = 'control.payment_quarantine rows are never removed',
        DETAIL = 'the refused operation was a ' || TG_OP || ': a delivery this build could not apply is a question an operator has not answered yet, and a quarantine that was pruned before anybody read it is a paying customer''s message that disappeared without ever being seen',
        ERRCODE = 'integrity_constraint_violation';
    RETURN NULL;
END;
$reject_payment_quarantine_delete$;

CREATE TRIGGER payment_quarantine_no_delete
    BEFORE DELETE ON control.payment_quarantine
    FOR EACH STATEMENT
    EXECUTE FUNCTION control.reject_payment_quarantine_delete();

COMMENT ON FUNCTION control.payment_events_append_only() IS
    'Payment engine guard: a recorded delivery is evidence of what this plane was told and what it did, and evidence is never edited. A redelivery is answered by reading the row, never by rewriting it.';
COMMENT ON FUNCTION control.reject_payment_event_delete() IS
    'Payment engine guard: recorded deliveries are never removed — the audit trail of which message funded which payment. Fires at statement level, so a delete of zero rows is refused too.';
COMMENT ON FUNCTION control.payment_quarantine_append_only() IS
    'Payment engine guard: a quarantined delivery is the verbatim evidence of a refusal. Resolving it is an operator''s act on the delivery, not an edit to the record.';
COMMENT ON FUNCTION control.reject_payment_quarantine_delete() IS
    'Payment engine guard: unapplied deliveries are never removed — each one is a question nobody has answered yet. Fires at statement level, so a delete of zero rows is refused too.';
