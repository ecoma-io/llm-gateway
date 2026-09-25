-- The inverse of 000008_intake_request_lookup.up.sql: the index dropped by
-- its explicit name. Nothing else goes — the table belongs to 000003, whose
-- down drops it, and this migration only ever owned the lookup. After the
-- drop, a lookup of a replay record by request_id is a sequential scan of a
-- forever-retained table again — correct, and exactly the cost this migration
-- existed to keep off the reaper's sweep.

DROP INDEX public.request_intake_request_id_idx;
