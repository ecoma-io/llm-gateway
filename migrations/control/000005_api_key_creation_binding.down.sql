-- The exact inverse of 000005_api_key_creation_binding.up.sql, in reverse
-- creation order: the prefix binding goes first, then the composite creator
-- edge gives way to the single-column edge 000002 shipped — same name, same
-- reference — and the users unique constraint that existed only to carry
-- the composite reference goes last, after nothing references it. No
-- CASCADE anywhere, and no IF EXISTS, holding the lane's convention: this
-- runs against a database whose history says 000005 was applied, so a
-- schema missing these constraints is an unexpected fact the rollback
-- should fail on rather than paper over.
--
-- What "down" means here: the schema again trusts the application alone for
-- same-account creation and the prefix's binding to its own id. It is why
-- down files run in suites and in nothing that serves traffic.

ALTER TABLE control.api_keys
    DROP CONSTRAINT api_keys_prefix_id_consistency;

ALTER TABLE control.api_keys
    DROP CONSTRAINT api_keys_created_by_account_id_fkey;

ALTER TABLE control.api_keys
    ADD CONSTRAINT api_keys_created_by_fkey
    FOREIGN KEY (created_by)
    REFERENCES control.users (id);

ALTER TABLE control.users
    DROP CONSTRAINT users_id_account_id_key;
