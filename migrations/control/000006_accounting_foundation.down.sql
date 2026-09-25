-- The inverse of 000006_accounting_foundation.up.sql, in reverse creation
-- order, no CASCADE: the engine guards go first (the history tables'
-- triggers and the account_payg guard set), then the edge the guards
-- protect, then the tables — legs before settlements before buckets, the
-- order the foreign keys demand. Dropping a table drops its own triggers
-- with it; the account_payg triggers and index are dropped explicitly
-- because that table stays, as are the ledger guard functions, which
-- outlive their table.

DROP TRIGGER account_payg_owner_guard ON control.account_payg;
DROP FUNCTION control.account_payg_bucket_owner();
DROP INDEX control.account_payg_funding_bucket_key;
DROP TRIGGER account_payg_bucket_write_once_guard ON control.account_payg;
DROP FUNCTION control.account_payg_bucket_write_once();
ALTER TABLE control.account_payg
    DROP CONSTRAINT account_payg_funding_bucket_fkey;

DROP TABLE control.ledger_entries;
DROP TABLE control.settlements;
DROP TABLE control.funding_buckets;

DROP FUNCTION control.ledger_entries_append_only();
DROP FUNCTION control.settlements_append_only();
DROP FUNCTION control.ledger_entries_release_references_hold();
DROP FUNCTION control.ledger_entries_adjustment_references_own_bucket();
