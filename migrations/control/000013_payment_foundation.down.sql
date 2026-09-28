-- The inverse of 000013_payment_foundation.up.sql, in the order that leaves a
-- clean re-apply: every trigger on this migration's own tables first, by name
-- and on the table it was created against, then the trigger functions by name
-- (DROP TABLE does not drop a function — a plpgsql trigger function has no
-- dependency edge to its table, only its triggers do), then the tables
-- newest-first with no CASCADE. That last ordering is the dependency order and
-- not a convention: payment_quarantine and payment_events each carry a
-- foreign key into payment_intents, so the parent goes last and a drop that
-- would strand a reference fails loudly instead of cascading into a
-- neighbour's object.
--
-- The indexes on these tables go with the tables and are not named here: an
-- index on a dropped table is dropped with it, so an explicit drop afterwards
-- would name an object that is no longer there. That includes the partial
-- unique index over provider_payment_ref — the one index in this file that
-- looks like a key and is not a constraint, and it belongs to
-- payment_intents exactly as the two constraints beside it do.
--
-- No IF EXISTS either, holding the lane's convention this file states and
-- 000009's and 000010's own down files state with it: this runs against a
-- database whose history says 000013 was applied, so a schema missing these
-- objects is an unexpected fact the rollback should fail on rather than paper
-- over.
--
-- What this rollback discards is worth stating rather than leaving to be
-- discovered, because it is the one thing a down file here can destroy that
-- cannot be re-derived: the payment records themselves. Dropping these tables
-- takes with them the only record of which provider delivery funded which
-- payment, and the funding legs the deliveries produced live in
-- control.ledger_entries — a table this file does not touch and must not, so a
-- database rolled back past this point keeps the money movements it booked and
-- loses the provider messages that authorised them. That asymmetry is why
-- down files are executed by deploy/postgres/verify.sh and by nothing that
-- serves traffic.

DROP TRIGGER payment_quarantine_no_delete ON control.payment_quarantine;
DROP TRIGGER payment_quarantine_append_only_guard ON control.payment_quarantine;
DROP TRIGGER payment_events_no_delete ON control.payment_events;
DROP TRIGGER payment_events_append_only_guard ON control.payment_events;
DROP TRIGGER payment_intents_no_delete ON control.payment_intents;
DROP TRIGGER payment_intents_immutability_guard ON control.payment_intents;
DROP TRIGGER payment_intents_status_transition_guard ON control.payment_intents;
DROP TRIGGER payment_intents_funding_bucket_owner_guard ON control.payment_intents;

DROP FUNCTION control.reject_payment_quarantine_delete();
DROP FUNCTION control.payment_quarantine_append_only();
DROP FUNCTION control.reject_payment_event_delete();
DROP FUNCTION control.payment_events_append_only();
DROP FUNCTION control.reject_payment_intent_delete();
DROP FUNCTION control.payment_intents_identity_immutability();
DROP FUNCTION control.payment_intents_status_transition();
DROP FUNCTION control.payment_intents_funding_bucket_owner();

DROP TABLE control.payment_quarantine;
DROP TABLE control.payment_events;
DROP TABLE control.payment_intents;
