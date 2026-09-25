-- The inverse of 000002_catalog_foundation.up.sql, in reverse creation
-- order, no CASCADE: members before the versions they snapshot, candidates
-- before the aliases they serve, every reference resolved by hand. The
-- suite executes this file against the real database, and a rollback that
-- has never run is a guess.
DROP TABLE alias_group_members;
DROP TABLE alias_group_versions;
DROP TABLE model_candidates;
DROP TABLE model_aliases;
DROP TABLE backends;
