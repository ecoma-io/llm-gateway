-- The exact inverse of 000010_sessions.up.sql, in reverse creation order: the
-- `users` credential columns go first, then `sessions` drops whole. The
-- indexes and the unique constraint on `sessions` belong to `sessions` and
-- are not named here — an index on a dropped table is dropped with it, so an
-- explicit drop afterwards would name an object that is no longer there, which
-- is the 000009 down file's own rule and it applies unchanged.
--
-- Nothing here is CASCADE. Every object below is dropped explicitly and in
-- dependency order, so a drop that would strand something else fails loudly
-- instead of cascading into a neighbour's object. No IF EXISTS either, holding
-- the lane's convention: this runs against a database whose history says
-- 000010 was applied, so a schema missing these objects is an unexpected fact
-- the rollback should fail on rather than paper over.
--
-- One thing this rollback does NOT undo, and it is worth stating rather than
-- leaving to be discovered: dropping the credential columns DISCARDS EVERY
-- PASSWORD in the deployment. There is no inverse — a PBKDF2 digest is one
-- way by construction, and the plaintext was never stored, so a user rolled
-- back past this file has no credential left and must be re-invited. That is
-- what "down" means for this file, and it is why down files are executed by
-- deploy/postgres/verify.sh and by nothing that serves traffic.
--
-- The rows in `sessions` go with the table, and that is the one deletion the
-- lane's no-DELETE rule is not making about this schema: the table itself is
-- the rollback, and there is no state to move a session to.

ALTER TABLE control.users
    DROP CONSTRAINT users_credential_whole,
    DROP CONSTRAINT users_credential_iterations_positive,
    DROP CONSTRAINT users_credential_salt_shape,
    DROP CONSTRAINT users_credential_hash_shape,
    DROP COLUMN credential_iterations,
    DROP COLUMN credential_salt,
    DROP COLUMN credential_hash;

DROP TABLE control.sessions;
