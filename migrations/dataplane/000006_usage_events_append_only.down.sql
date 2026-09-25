-- The exact inverse of 000006_usage_events_append_only.up.sql: the trigger
-- dropped before the function that executes it, each by its explicit name.
--
-- Both halves go, for the reason 000003_runtime_storage.down.sql states for
-- its own pair: a DROP TRIGGER removes the guard from the table it belongs
-- to, but a trigger's function is an object in its own right and no DROP
-- TABLE in this lane would remove it — an orphaned function left behind in
-- public is exactly the leftover a down is sworn not to leave. The table
-- itself is not touched here: usage_events belongs to 000003, whose down
-- drops it, and this migration only ever owned the guard.
--
-- Order matters and is the only subtlety: the trigger is dropped first
-- because it references the function, so removing the function first would
-- fail on the dependency (and PostgreSQL declines to drop a function a
-- trigger still executes, absent CASCADE — which this lane does not use).

DROP TRIGGER usage_events_immutability ON public.usage_events;
DROP FUNCTION public.reject_usage_event_rewrite();
