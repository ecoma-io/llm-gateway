-- The inverse of 000008_fact_ingestion.up.sql: the consumer's three tables,
-- dropped newest-first. Dropping applied_facts before settlements is safe —
-- the foreign key points the other way — but the cursor is dropped last on
-- purpose regardless of order: losing the position is the one drop whose
-- consequence (the consumer re-reading from the beginning of what the Data
-- Plane still retains, replaying everything its idempotency absorbs) is
-- worth a sentence even in a down lane nothing runs in anger.

DROP TABLE control.quarantined_facts;
DROP TABLE control.applied_facts;
DROP TABLE control.ingestion_cursor;
