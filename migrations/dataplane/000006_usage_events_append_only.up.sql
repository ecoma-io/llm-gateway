-- Migrations lane: dataplane — the Data Plane runtime's `dataplane` database
-- (ADR 0006 §7). This file arms the one guard `usage_events` was missing: the
-- fact stream becomes append-only at the engine. A written fact can no longer
-- be updated or deleted by any writer — the BEFORE UPDATE OR DELETE rejection
-- trigger below refuses both statements at the row, the way
-- `reservation_allocations_immutability` already refuses a rewritten leg.
--
-- THE RULE THIS ENFORCES. docs/architecture/persistence.md states it as the
-- schema convention it is — "Append-only tables: `ledger_entries` and
-- `usage_events` have no `UPDATE` or `DELETE` path (ADR 0004, invariants 1–2)"
-- — and ADR 0004's first accounting invariant says the same thing about the
-- rows: "Usage history is immutable. UsageEvent rows are insert-only;
-- corrections are new linked facts." Until now that held by writer discipline
-- alone: no repository in the runtime issues an UPDATE or a DELETE against
-- the table, but the engine had no opinion, so a future writer (a migration,
-- an operator's psql session, a second process that never read the port) could
-- silently rewrite forever-retained money history. 000003's own doctrine —
-- the engine is the final guard, stated twice on purpose — already named the
-- shape: every invariant of this file is stated at the call site AND in the
-- schema. The fact stream carried its payload-shape half
-- (usage_events_payload_envelope) and lacked its rewrite half; this file adds
-- the missing twin.
--
-- THE PATTERN, AND WHY THIS SHAPE. The lane's immutability triggers come in
-- two kinds. requests and reservations fire only on rows ALREADY terminal —
-- a WHEN clause keeps the open rows' ordinary writes free. usage_events has
-- no open rows: a fact is born final (its whole lifecycle is the one INSERT
-- inside its settlement unit of work), which is the shape
-- reservation_allocations already has. So this trigger mirrors that one
-- exactly — row-level, no WHEN clause, no column list, one rejection
-- function whose message names the table through TG_TABLE_NAME and whose
-- search_path is pinned, because a trigger runs inside the writing session
-- and an unqualified name would resolve against whatever search_path the
-- caller set.
--
-- CORRECTIONS STAY A WRITE, NOT A REWRITE. The dedup uniques and the
-- corrects_append_seq pointer (000003) already reserve the correction path:
-- a wrong fact is superseded by a NEW fact referencing the original, never
-- by editing the original in place. This trigger is what stops "never" from
-- depending on everyone's goodwill — the correction vocabulary survives
-- untouched beside it, and the one insertion path a settlement needs stays
-- exactly as it was.
--
-- TWO THINGS THIS FILE DOES NOT DO, on purpose:
--
--   * 000003 is not edited. Applied migrations are immutable history
--     (deploy/postgres/README.md's safety model), so the payload-envelope
--     trigger keeps its `INSERT OR UPDATE OF payload` firing set. Its UPDATE
--     half is now unreachable — no UPDATE survives the guard below — which
--     makes it dead armour rather than a hole: a guard that can no longer be
--     reached still refuses what it always refused.
--   * The pattern's reach is inherited, not widened. A BEFORE UPDATE OR DELETE
--     row trigger fires for the two statements the convention refuses and for
--     nothing else, so TRUNCATE still empties the table — exactly as it still
--     empties reservation_allocations under the leg trigger this one mirrors,
--     and as session_replication_role = replica still silences every row
--     trigger in this file. Both are the privilege model's business rather
--     than the trigger's, which is what 000003's header says of its own
--     guards: "It can be bypassed by session_replication_role = replica; that
--     is the privilege model's business, not the trigger's." The production
--     contract that answers them is the runtime role's
--     (deploy/postgres/README.md: SELECT, INSERT, UPDATE and DELETE on that
--     plane's tables; no CREATE, no DDL, no superuser — TRUNCATE among the
--     privileges the DDL-owning migration role holds and the application role
--     does not). This migration claims the same reach as the guards beside it
--     and no more.
--
-- 000003_runtime_storage creates the fact stream. That file is immutable
-- applied history, and it left the stream without its immutability trigger,
-- so this file is where the missing guard belongs rather than an edit to a
-- recorded migration. The lane's own tail order is what the number follows:
-- 000004 is the Control-to-Data projection, 000005 the client price list, and
-- the next whole number in that lane is this one.
--
-- It adds what 000003 could not. A usage event is the record of what happened: the request that was served, the price that was applied, the units that were consumed, and the money that moved against them. Those four things are the settlement, and the Control Plane's ledger is derived from them — which means a fact that can be altered after the fact is a settlement that can be silently retracted, re-spent, or moved onto a different price, and every account downstream of it inherits the error. The runtime domain already refuses to expose a mutable handle to this data, and no repository in the plane issues an UPDATE or a DELETE against the table. This trigger is the third guard, and the only one that is the engine's: it holds for any writer, including a future migration, a repair script, or a hand-typed session in psql, none of which pass through the runtime's application code at all.
--
-- The rejection is unconditional, and that is the point. A fact is born
-- final — its whole lifecycle is the single INSERT inside the settlement
-- unit of work — so there is no open state to protect and no legitimate
-- UPDATE for a WHEN clause to let through, and a conditional one would be a
-- guard that can be walked around by a well-formed-looking row. The
-- correction path is not missing because it was forgotten: a wrong fact is
-- superseded by a new linked fact (ADR 0004 invariant 1), which 000003's
-- `corrects_append_seq` and the dedup partial uniques already model. A
-- correction is an append; it is never an edit.
--
-- The refusal is worded as a fact about the table rather than about the
-- statement, so a reader of an application log learns which table refused
-- rather than which call site, and it names the table through
-- TG_TABLE_NAME so the function is the table's rather than a copy of
-- itself. The search_path is pinned to pg_catalog because a trigger
-- function executes inside the writing session: an unqualified name would
-- resolve against whatever search_path that session happened to set, and a
-- guard that can be redirected by SET is not a guard.
--
-- The reach claimed here is exactly the reach the lane's existing
-- immutability triggers claim. This trigger fires on UPDATE and DELETE, the
-- two operations the persistence convention forbids for this table. It does
-- not fire on TRUNCATE, which is a DDL operation, not a row modification,
-- and it does not fire when the session sets
-- session_replication_role = replica. Both remain true of every trigger in
-- this file, and both are out of scope here by design: TRUNCATE and
-- superuser-equivalent replication-role switching are privilege questions,
-- answered by role grants and by who is trusted with a superuser, not by
-- schema objects. Narrowing this file's scope to those would exceed the
-- pattern it belongs to. The deployment note that applies is the one the
-- migration's own lane already states: triggers are a correctness guard, and
-- the row-level enforcement this file adds is that guard, not the whole of
-- append-only.
--
-- The inverse of this file is 000006_usage_events_append_only.down.sql: the
-- trigger dropped before the function that executes it, both by name.

CREATE FUNCTION public.reject_usage_event_rewrite() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
    AS $$
BEGIN
    RAISE EXCEPTION '% has a usage event that cannot be rewritten', TG_TABLE_NAME;
END;
$$;

CREATE TRIGGER usage_events_immutability
    BEFORE UPDATE OR DELETE ON public.usage_events
    FOR EACH ROW
    EXECUTE FUNCTION public.reject_usage_event_rewrite();
