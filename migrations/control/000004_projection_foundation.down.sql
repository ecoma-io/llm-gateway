-- The inverse of 000004_projection_foundation.up.sql, in reverse creation
-- order, no CASCADE. The backfilled rows are projection artifacts derived
-- entirely from control.accounts, so dropping the four tables loses nothing
-- the authoritative identity schema cannot rebuild — the projection is a
-- delivery pipeline's source-of-record, not a second authority.

DROP TABLE control.projection_accounts;
DROP TABLE control.projection_api_keys;
DROP TABLE control.projection_changes;
DROP TABLE control.projection_revision;
