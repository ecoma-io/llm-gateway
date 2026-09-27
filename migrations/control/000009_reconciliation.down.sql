-- The inverse of 000009_reconciliation.up.sql, in the order that leaves a
-- clean re-apply: the index this migration added to a PRE-EXISTING table
-- first (by name, on the table it was added to — a down that omits this
-- leaves a stray index and a dirty re-apply, which is the one failure mode
-- this file exists to prevent), then the triggers on this migration's own
-- tables, then the trigger functions by name (DROP TABLE does not drop a
-- function), then the tables newest-first with no CASCADE. The indexes on
-- those tables go with the tables and are not named here: an index on a
-- dropped table is dropped with it, so an explicit drop afterwards would name
-- an object that is no longer there.
--
-- Nothing here is CASCADE. Every object below is dropped explicitly and in
-- dependency order, so a drop that would strand something else fails loudly
-- instead of cascading into a neighbour's object.

-- The index this migration added to applied_facts — a pre-existing table.
-- Dropping it first restores that table to the shape 000008 left it in: a
-- primary key and nothing else, which is the state the up migration's header
-- describes as the problem it exists to solve.
DROP INDEX control.applied_facts_applied_at_idx;

-- The findings' two guards, before their table: a trigger is dropped
-- explicitly, and the table drop below would take it silently otherwise.
DROP TRIGGER reconciliation_findings_no_delete ON control.reconciliation_findings;
DROP TRIGGER reconciliation_findings_identity_immutability ON control.reconciliation_findings;

-- The functions, by name. DROP TABLE does not drop a function — a plpgsql
-- trigger function has no dependency edge to its table, only its triggers do —
-- so leaving these behind would make the next up migration's CREATE FUNCTION
-- fail on the name.
DROP FUNCTION control.reject_finding_delete();
DROP FUNCTION control.reject_finding_identity_rewrite();

-- The tables, newest first. The findings' own three indexes — the partial
-- unique over status = 'open' and the two open-set readers — belong to
-- reconciliation_findings and go with it.
DROP TABLE control.reconciliation_findings;
DROP TABLE control.reconciliation_runs;
