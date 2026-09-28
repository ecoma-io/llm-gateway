-- The inverse of 000014_sepay_bank_transfer.up.sql, in the order that leaves a
-- clean re-apply: the two columns this migration added, the status CHECK and
-- the two function bodies it restated, then every object it renamed.
--
-- WHY THE FUNCTION BODIES ARE RESTATED RATHER THAN LEFT ALONE. A plpgsql body
-- is stored as source text and parsed at first execution, so a function that
-- still said `provider_transfer_ref` after that column was renamed back would
-- not fail here — it would fail later, inside a customer's payment, at the
-- moment the guard it implements was supposed to fire. The rename is carried
-- by PostgreSQL into the constraint expressions and the index predicate for
-- free; a function body is the one dependent thing it cannot carry. The whole
-- file is one transaction, so the moment where a restated body names a column
-- the rename below has not yet restored is never observable — but it is
-- written in the order that leaves the last statement a rename rather than a
-- body, so a reader who stops early has still seen the coherent half.
--
-- Everything else is a rename back, and PostgreSQL carries the dependent
-- expressions with it exactly as the up file's rename did: the two grammar
-- CHECKs on the columns, the `WHERE` predicate of the partial unique index,
-- and the status CHECK's membership. No data is moved, converted or dropped,
-- and no row written under 000014 is invalid under 000013 except one case
-- worth stating: a payment whose status is `awaiting_transfer` cannot satisfy
-- the CHECK this file restores, because that member did not exist. A rollback
-- that meets such a row will fail on the constraint rather than silently
-- relabel a payment's state, which is the right failure — a status is what the
-- provider said about money, and this file has no authority to rewrite one.
--
-- No IF EXISTS, holding the lane's convention: this runs against a database
-- whose history says 000014 was applied, so a schema missing these objects is
-- an unexpected fact the rollback should fail on rather than paper over.

-- The two columns the provider's answer added. Nothing else on this table
-- depended on them: no index, no constraint outside their own CHECKs, and no
-- trigger body below reads them.
ALTER TABLE control.payment_intents
    DROP CONSTRAINT payment_intents_provider_bank_name_evidence;

ALTER TABLE control.payment_intents
    DROP CONSTRAINT payment_intents_provider_account_holder_evidence;

ALTER TABLE control.payment_intents
    DROP COLUMN provider_account_holder;

ALTER TABLE control.payment_intents
    DROP COLUMN provider_bank_name;

-- The status vocabulary, back to the member 000013 closed the set with.
ALTER TABLE control.payment_intents
    DROP CONSTRAINT payment_intents_status_valid;

ALTER TABLE control.payment_intents
    ADD CONSTRAINT payment_intents_status_valid
    CHECK (status IN (
        'created', 'checkout_open', 'requires_action', 'succeeded',
        'failed', 'cancelled', 'expired', 'partially_refunded',
        'refunded', 'quarantined'
    ));

-- The two function bodies, restated with the names this file restores below.
CREATE OR REPLACE FUNCTION control.payment_intents_status_transition() RETURNS trigger
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

COMMENT ON FUNCTION control.payment_intents_identity_immutability() IS
    'Payment engine guard: a payment''s identity — whose money, how much, in what unit, which provider, which key, when it was opened — and its expiry are written once, and both provider references are write-once (NULL -> value allowed, value -> different value refused). What the two write-once arms protect is the derivation: the command key a funding leg carries is derived from the capture reference, so a re-pointed reference is a second credit.';

-- The objects whose names this migration changed, back to 000013's.
ALTER INDEX control.payment_intents_provider_transfer_ref_key
    RENAME TO payment_intents_provider_checkout_ref_key;

ALTER TABLE control.payment_intents
    RENAME CONSTRAINT payment_intents_provider_qr_url_evidence
    TO payment_intents_checkout_url_evidence;

ALTER TABLE control.payment_intents
    RENAME CONSTRAINT payment_intents_provider_transfer_ref_grammar
    TO payment_intents_provider_checkout_ref_grammar;

ALTER TABLE control.payment_intents
    RENAME COLUMN provider_qr_url TO checkout_url;

ALTER TABLE control.payment_intents
    RENAME COLUMN provider_transfer_ref TO provider_checkout_ref;

-- The comments 000013 wrote. The two columns the renames restored carry the
-- text that was on them before 000014 replaced it, which is the only thing on
-- this table a rename cannot bring back.
COMMENT ON TABLE control.payment_intents IS
    'Payment integration (B15): one row per funding top-up this platform initiated with an external provider. Money is credited only by a provider-signed server-to-server webhook resolved against this row — never by a browser redirect — and this row''s account and funding bucket are fixed at creation, because an event resolved an account from a payload would let a third party say whose money moves. The three tables here hold refunds as RECOGNISED AND RECORDED figures rather than booked ledger legs: B6''s algebra cannot represent a debit that drives a settled balance below zero.';

COMMENT ON COLUMN control.payment_intents.provider_checkout_ref IS
    'The provider''s identifier for the hosted CHECKOUT, write-once: NULL until the checkout is opened, then never re-pointed. It is the key a delivery is resolved by, and a payment whose checkout reference moved would silently stop matching the messages about its own checkout.';

COMMENT ON COLUMN control.payment_intents.checkout_url IS
    'The provider''s hosted-checkout URL, verbatim and never parsed. It is stored so a returning customer can be sent back to the checkout they started — an affordance, never evidence: nothing about a redirect authenticates a payment, which is why credit comes from the webhook alone.';

COMMENT ON COLUMN control.payment_intents.expires_at IS
    'When an un-completed checkout stops being WAITABLE — a local deadline, not a terminal fact about the money. A capture reported after it is still honoured and still funded (the expired -> succeeded edge), because a customer who paid thirty seconds after a local timer fired has paid.';
