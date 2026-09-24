-- The exact inverse of 000002_identity_foundation.up.sql, in reverse creation
-- order: tables drop children-first so the foreign keys they carry never
-- dangle, and the indexes drop with their tables. No CASCADE anywhere — the
-- up file refuses to let the schema be torn through, and so does the down.
-- No IF EXISTS either, holding the lane's first file's convention: this runs
-- against a database whose history says these tables were applied, so one
-- that lacks them is an unexpected fact the rollback should fail on rather
-- than paper over.
--
-- This drops the Control Plane's entire identity foundation: accounts, users
-- and every API key's ownership record. That is what "down" means for this
-- file, and it is why down files are executed by deploy/postgres/verify.sh
-- and by nothing that serves traffic.

BEGIN;

DROP TABLE control.api_keys;

DROP TABLE control.users;

DROP TABLE control.accounts;

COMMIT;
