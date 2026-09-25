-- Migrations lane: dataplane — the Data Plane runtime's `dataplane` database
-- (ADR 0006 §7). This file adds an index, not a table: the lookup the reaper's
-- sweep makes by request_id when it carries a closed hold's replay identity
-- back to its caller. Everything else about the read already existed —
-- request_intake carries the account and idempotency key the expired close
-- needs to finalise the record the hold left open — what was missing is a way
-- to find that record by the request the hold names.
--
-- THE READ THIS SERVES. The reaper's batch CAS (ReservationRepository
-- .ExpireLapsedLeases) closes a lapsed hold and returns what its caller needs
-- to settle the close in the same unit of work: the legs for the fact's
-- allocation tail, and — since the port began carrying them — the replay
-- identity, so the request row can finalise and the intake record's terminal
-- pointer can be written by the same keyed Finalise the release path calls.
-- That identity lives in request_intake, and the only path to it from a
-- swept hold is the hold's request_id. Until now no read needed that path:
-- request_intake's enforced key is (account_id, idempotency_key) — the
-- replay pair, the only door a replay ever knocks on — and request_id is an
-- ID reference across the relational/event family boundary (000003's table
-- comment records why no FK stands behind it), so nothing looked a record up
-- by request. The reaper's sweep is the first reader, and this index is its
-- lookup.
--
-- WHY THE INDEX SHIPS WITH THE READ. request_intake is a forever-retained
-- table: one row per request ever admitted, retained by design, with no
-- delete path. A lookup by request_id without an index is a sequential scan
-- of that whole history, and the sweep runs against the serving plane's own
-- database — priced in the same transaction as the close it is part of. The
-- lane's rule for reads like this is the one 000005 stated for the price
-- list's selection: the index is the read's lookup, and it ships in the same
-- migration as the read that needs it.
--
-- NOT UNIQUE, ON PURPOSE. The table does not enforce one record per request
-- — its enforced key is the replay pair, and admission's transaction is what
-- keeps the request/record pair atomic. An index here that smuggled in a
-- uniqueness the schema never decided would be a constraint arriving through
-- the back door. The sweep's read is deterministic without it: oldest
-- created_at first, which is the record born with the request.
--
-- Conventions held across this lane (the secondary-index name follows
-- 000002's model_candidates_alias_id_idx and 000005's
-- client_price_list_entries_alias_id_idx): the index is named in the lane's
-- `<table>_<columns>_idx` vocabulary, and the file carries no transaction
-- control — the runner wraps each file in one implicit transaction.
--
-- The inverse of this file is 000008_intake_request_lookup.down.sql: the
-- index dropped by its explicit name. The table itself is not touched here —
-- request_intake belongs to 000003, whose down drops it.

CREATE INDEX request_intake_request_id_idx
    ON public.request_intake (request_id);

COMMENT ON INDEX request_intake_request_id_idx IS
    'The reaper sweep''s lookup: finds a closed hold''s replay record by the request the hold names, so the expired close can finalise the record with the identity the sweep carried back. Non-unique by design — the table''s enforced key stays the replay pair (account_id, idempotency_key).';
