-- Migrations lane: control — the Control Plane's `control` database
-- (ADR 0006 §7). This file RESTATES 000013's three payment tables in the
-- vocabulary of the provider this deployment now speaks, and adds the two
-- columns that provider's answer needs. It creates no table and drops none.
--
-- 000014 — SePay bank transfer (B15, revised):
--
--   000013 landed the payment integration against a provider whose instrument
--   was a hosted CHECKOUT: this platform asked for a session, the customer
--   completed it on the provider's page, and a delivery reported the capture.
--   ADR 0013's doctrine — a signed server-to-server delivery is the only
--   funding authority, a delivery is a CLAIM resolved against rows this plane
--   wrote, and the resolution key is a reference the provider issued rather
--   than anything a customer typed — is unchanged and is not renegotiated
--   here. What changed is the instrument.
--
--   The provider is now SePay, whose instrument is a BANK TRANSFER to an
--   account it mints per payment. There is no session, no hosted page and no
--   redirect, so three names on these tables described a thing that no longer
--   exists:
--
--     provider_checkout_ref -> provider_transfer_ref
--     checkout_url          -> provider_qr_url
--     status checkout_open  -> awaiting_transfer
--
--   Each is a RENAME rather than a new column beside a dead one. A payment
--   that carried both `provider_checkout_ref` and `provider_transfer_ref`
--   would be a row where two columns claim to be the key a delivery resolves
--   by, and the one thing this schema cannot afford is a resolution key that
--   is optional in practice: the delivery-resolution read is the single
--   lookup a third party's payload drives, and it has to be one column, NOT
--   NULL, with one partial unique index over it.
--
--   WHY THE VIRTUAL ACCOUNT IS THE KEY AND THE TRANSFER MEMO IS NOT. The
--   provider offers both ways to tie a transfer to a payment: the account the
--   money was sent TO, which it echoes back on the delivery, and a code it
--   extracts from the transfer's description. Only the first is usable as a
--   key. The virtual account is issued by the provider, is unique to one
--   payment, and is not the customer's to change — a customer sends money TO
--   it, and a delivery naming it is a statement that money arrived at a
--   destination this platform asked for. A memo is the customer's own free
--   text: their bank may uppercase it, shorten it or strip it, and the
--   customer may replace it with a word of their own, so a payment resolved
--   by one would be a payment that stopped resolving the first time somebody's
--   banking app reformatted a description. That asymmetry — a destination
--   versus a comment — is what the column rename records, and it is why
--   provider_transfer_ref keeps the write-once arm and the partial unique
--   index its predecessor had.
--
--   WHY THE QR IS STORED AND NEVER COMPOSED. The provider returns its own QR
--   image for the transfer, and this plane stores the URL it was handed. It
--   does not encode one: a payment QR here is an EMVCo/VietQR payload with a
--   tag-length-value structure and a CRC, and a platform that composed one
--   would own that encoding — every bank app that disagreed with its reading
--   would be a customer who could not pay. Storing the provider's own answer
--   keeps the encoding where the account number comes from. It is evidence of
--   nothing, exactly as checkout_url was: an image cannot authenticate a
--   payment, and the credit still arrives only from a signed delivery.
--
--   THE TWO NEW COLUMNS are the rest of that answer — the bank the account is
--   held at and the name it is held in. They are shown to a customer so the
--   destination can be checked before money is sent, and so a customer typing
--   the account number by hand knows where it goes. They are the provider's
--   strings, reproduced verbatim and matched against nothing: `bank_name` is a
--   name and not a code, and `account_holder` is the provider's spelling of a
--   company. Like provider_qr_url they are write-once in practice because the
--   only writer sets them once, and like checkout_url before them they are
--   left in the mutable set rather than given write-once arms of their own —
--   they decide no money, and the column that does has an arm.
--
--   WHAT DOES NOT CHANGE. No table is added, so the funding bucket's
--   same-column owner guard, the append-only ledger, the delete guards, the
--   immutability tuple, the capture-shape CHECK, the refund projection and its
--   two ceilings, and every index on payment_events and payment_quarantine are
--   exactly as 000013 left them. The refund machinery stays reachable in the
--   domain and unreachable through this provider, which is the correct shape:
--   a refund is recorded when a delivery reports one, and a provider that
--   reports none simply never exercises the path. Nothing here books money.
--
-- The inverse of this file is 000014_sepay_bank_transfer.down.sql, which
-- restores the three names, the status CHECK and the two trigger functions,
-- and drops the two columns this file adds.

-- ---------------------------------------------------------------------------
-- The rename, which carries the constraint expressions, the partial unique
-- index and the two trigger functions with it.
-- ---------------------------------------------------------------------------
-- PostgreSQL rewrites every expression that names a renamed column: the
-- grammar CHECKs on the column itself, the `WHERE` predicate of the partial
-- unique index over it, and the bodies of the two plpgsql functions below are
-- all stored as parsed trees or as source text referencing the column, and the
-- rename keeps them pointing at the right one. What it does NOT do is rename
-- the OBJECTS, so the constraint and index names below are renamed beside the
-- columns to keep the lane's names describing what they guard. The two
-- function bodies ARE rewritten by hand further down, because a plpgsql body
-- is checked at first execution rather than at creation and the two functions
-- quote their own column names in their refusal DETAIL text.
ALTER TABLE control.payment_intents
    RENAME COLUMN provider_checkout_ref TO provider_transfer_ref;

ALTER TABLE control.payment_intents
    RENAME COLUMN checkout_url TO provider_qr_url;

ALTER TABLE control.payment_intents
    RENAME CONSTRAINT payment_intents_provider_checkout_ref_grammar
    TO payment_intents_provider_transfer_ref_grammar;

ALTER TABLE control.payment_intents
    RENAME CONSTRAINT payment_intents_checkout_url_evidence
    TO payment_intents_provider_qr_url_evidence;

ALTER INDEX control.payment_intents_provider_checkout_ref_key
    RENAME TO payment_intents_provider_transfer_ref_key;

-- ---------------------------------------------------------------------------
-- The two columns the provider's answer adds.
-- ---------------------------------------------------------------------------
-- Both carry the same evidence bound provider_qr_url carries, for the same
-- reason: these are a third party's strings, their length is the provider's
-- business, and this is a ceiling that keeps an unbounded value out of the
-- row rather than a grammar that claims to know what a bank is called. Both
-- are NULL until the provider answers, on the rule provider_transfer_ref
-- follows — a payment exists before this plane has a destination for it — and
-- both are therefore NOT NULL-able in the direction that matters: a row can
-- carry them or not, and a row that carries one has a provider_transfer_ref
-- beside it, because the three are written by one statement.
ALTER TABLE control.payment_intents
    ADD COLUMN provider_bank_name text
        CONSTRAINT payment_intents_provider_bank_name_evidence
        CHECK (provider_bank_name IS NULL OR char_length(provider_bank_name) <= 255);

ALTER TABLE control.payment_intents
    ADD COLUMN provider_account_holder text
        CONSTRAINT payment_intents_provider_account_holder_evidence
        CHECK (provider_account_holder IS NULL OR char_length(provider_account_holder) <= 255);

-- ---------------------------------------------------------------------------
-- The status vocabulary, restated.
-- ---------------------------------------------------------------------------
-- A CHECK cannot be altered in place, so the closed set is dropped and
-- restated with the new member. The set is otherwise untouched: the state
-- machine's EDGES are the trigger's business below, and no edge is added,
-- removed or moved here. `requires_action` stays in the set although the
-- provider this deployment now speaks never reports it — it is a fact about
-- money rather than about one provider's vocabulary, and the state machine
-- keeps it for the same reason it keeps the two refund states.
ALTER TABLE control.payment_intents
    DROP CONSTRAINT payment_intents_status_valid;

ALTER TABLE control.payment_intents
    ADD CONSTRAINT payment_intents_status_valid
    CHECK (status IN (
        'created', 'awaiting_transfer', 'requires_action', 'succeeded',
        'failed', 'cancelled', 'expired', 'partially_refunded',
        'refunded', 'quarantined'
    ));

-- ---------------------------------------------------------------------------
-- The status machine, restated in the new vocabulary.
-- ---------------------------------------------------------------------------
-- The edges are the domain's own (apps/console-api/internal/domain/payments/
-- state.go), and this restatement moves exactly two member names and no edge:
-- every rule below reads as 000013's did with `checkout_open` replaced by
-- `awaiting_transfer`.
CREATE OR REPLACE FUNCTION control.payment_intents_status_transition() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
    AS $payment_intents_status_transition$ BEGIN
    IF NEW.status IS DISTINCT FROM OLD.status THEN
        IF NOT (
            (OLD.status = 'created' AND NEW.status IN ('awaiting_transfer', 'failed', 'cancelled'))
            OR (OLD.status = 'awaiting_transfer' AND NEW.status IN ('requires_action', 'succeeded', 'failed', 'cancelled', 'expired'))
            OR (OLD.status = 'requires_action' AND NEW.status IN ('awaiting_transfer', 'succeeded', 'failed', 'cancelled'))
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

COMMENT ON FUNCTION control.payment_intents_status_transition() IS
    'Payment engine guard: the legal status edges, enforced for every writer including raw SQL because a CHECK cannot see OLD. expired -> succeeded and cancelled -> succeeded are legal — expiry and cancellation are local decisions, the provider states the money — and a payment may not reach succeeded without the capture reference that funded it.';

-- ---------------------------------------------------------------------------
-- The identity-immutability guard, restated in the new vocabulary.
-- ---------------------------------------------------------------------------
-- The tuple is UNCHANGED, and that is the point of restating it here rather
-- than leaving the old body in place: the tuple is the frozen set and the two
-- new columns are deliberately outside it, so a reader comparing this function
-- against the table can see which columns are identity and which are the
-- provider's answer arriving late. No column moves between the two sets in
-- this migration. What moves is the write-once arm's name and the prose of
-- both refusal messages, which quote column names an operator has to be able
-- to find.
CREATE OR REPLACE FUNCTION control.payment_intents_identity_immutability() RETURNS trigger
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
            MESSAGE = 'control.payment_intents is immutable in its identity: only status, the provider''s transfer answer, the refund projection, state_version and updated_at may change',
            DETAIL = 'the refused change would have rewritten whose money this payment funds, how much, in what unit, or when it was opened — and those were decided before the customer was ever asked to pay. The account and the funding bucket are fixed at creation because a webhook resolves them FROM THIS ROW: a payment that could change hands is a payment whose ledger legs belong to one account and whose money landed in another, and an edited amount is a charge this platform never priced. The error deliberately does not restate the row''s values, because an amount and an account id are not things an operator-facing log line has any business quoting',
            ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.provider_transfer_ref IS NOT NULL
        AND NEW.provider_transfer_ref IS DISTINCT FROM OLD.provider_transfer_ref THEN
        RAISE EXCEPTION USING
            MESSAGE = 'control.payment_intents.provider_transfer_ref is write-once: it may be set from NULL, never re-pointed',
            DETAIL = 'the refused change would have re-pointed the account this payment is paid into on an existing payment. That reference is the key a delivery is resolved by — the provider echoes it back when money arrives at it — so a second value would silently stop matching the deliveries about the destination this payment actually handed the customer, and, worse in the other direction, a value swapped in from another payment would make this row the resolution target for a transfer it never asked for. NULL -> value is the write this table exists to receive and is not refused',
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

COMMENT ON FUNCTION control.payment_intents_identity_immutability() IS
    'Payment engine guard: a payment''s identity — whose money, how much, in what unit, which provider, which key, when it was opened — and its expiry are written once, and both provider references are write-once (NULL -> value allowed, value -> different value refused). What the two write-once arms protect is the derivation: the command key a funding leg carries is derived from the capture reference, so a re-pointed reference is a second credit, and a re-pointed transfer reference is a payment that stops matching its own deliveries.';

-- ---------------------------------------------------------------------------
-- The comments, restated. The two renamed columns and the two new ones carry
-- the doctrine a reader of this table needs, and the table comment names what
-- the three tables hold in the vocabulary the provider actually uses.
-- ---------------------------------------------------------------------------
COMMENT ON TABLE control.payment_intents IS
    'Payment integration (B15): one row per funding top-up this platform initiated with an external provider. Money is credited only by a provider-signed server-to-server webhook resolved against this row — never by a browser return, a QR display or anything a customer says — and this row''s account and funding bucket are fixed at creation, because an event resolved an account from a payload would let a third party say whose money moves. The three tables here hold refunds as RECOGNISED AND RECORDED figures rather than booked ledger legs: B6''s algebra cannot represent a debit that drives a settled balance below zero.';

COMMENT ON COLUMN control.payment_intents.provider_transfer_ref IS
    'The account the provider issued for this payment to be paid INTO, write-once: NULL until the provider answers, then never re-pointed. It is the key a delivery is resolved by — the provider echoes it back on the delivery that reports money arriving at it — and a payment whose destination moved would silently stop matching the messages about its own transfer. It is the provider''s virtual account and not the customer''s transfer description, deliberately: a customer may edit, shorten or delete a memo, and a destination is not theirs to change.';

COMMENT ON COLUMN control.payment_intents.provider_qr_url IS
    'The provider''s own QR image for this transfer, as a URL, verbatim and never parsed. It is stored so a customer can scan it and so the same destination is shown again when they return, and it is an affordance and never evidence: an image authenticates nothing, which is why credit comes from the webhook alone. This plane composes no QR of its own — the encoding is the provider''s, and a platform that reimplemented it would own every bank app that disagreed with its reading of it.';

COMMENT ON COLUMN control.payment_intents.provider_bank_name IS
    'The bank the provider says the virtual account is held at, verbatim. Shown to the customer so the destination can be checked before money is sent, and matched against nothing: it is a name and not a code, and the provider''s spelling is the whole of its meaning.';

COMMENT ON COLUMN control.payment_intents.provider_account_holder IS
    'The name the provider says the account is held in, verbatim. It is what a customer checks before sending money and what their banking app verifies the destination against, which is the whole reason it is stored rather than reconstructed: this plane has no other source for it and must not invent one.';

COMMENT ON COLUMN control.payment_intents.expires_at IS
    'When an un-completed transfer stops being WAITABLE — a local deadline, not a terminal fact about the money. A capture reported after it is still honoured and still funded (the expired -> succeeded edge), because a customer who paid thirty seconds after a local timer fired has paid. It is this platform''s own window and not the destination''s expiry at the provider: the two are set from one configuration value and the bank''s copy is the provider''s business, but a customer who transferred inside the bank''s window and arrived after this one is still funded, which is what makes the deadline a statement about patience rather than about the money.';
