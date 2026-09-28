-- The inverse of 000012_analytics.up.sql, in the order that leaves a clean
-- re-apply: the indexes this migration added to PRE-EXISTING tables first (by
-- name, on the table each was added to — a down that omits these leaves stray
-- indexes and a dirty re-apply, which is the one failure mode this file exists
-- to prevent), then this migration's own table newest-first with no CASCADE.
-- The index on that table goes with it and is not named here: an index on a
-- dropped table is dropped with it, so an explicit drop afterwards would name
-- an object that is no longer there.
--
-- Nothing here is CASCADE. Every object below is dropped explicitly and in
-- dependency order, so a drop that would strand something else fails loudly
-- instead of cascading into a neighbour's object.
--
-- The analytics table's own dispositions are worth stating plainly, because
-- this is the only migration in the lane whose drop loses derived state rather
-- than transactional state. Nothing here is an authority: settlements,
-- ledger_entries, applied_facts and funding_buckets are untouched and remain
-- the money record, and this table is reconstructable from the facts'
-- allocation tails joined to the buckets they name. Dropping it is therefore
-- losing a cache, not losing a fact.

-- The two indexes on pre-existing tables, dropped first.
DROP INDEX control.ledger_entries_created_at_idx;
DROP INDEX control.settlements_created_at_idx;

-- The one table this migration creates.
DROP TABLE control.analytics_fact_dimensions;
