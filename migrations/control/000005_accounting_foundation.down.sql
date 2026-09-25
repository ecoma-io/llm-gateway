-- The inverse of 000005_accounting_foundation.up.sql, in reverse creation
-- order, no CASCADE: the engine guards go first (the history tables' triggers
-- and the account_payg write-once pair), then the edge the guards protect,
-- then the tables — legs before settlements before buckets, the order the
-- foreign keys demand. Dropping a table drops its own triggers with it; the
-- account_payg trigger is dropped explicitly because that table stays.

DROP TRIGGER account_payg_bucket_write_once_guard ON control.account_payg;
DROP FUNCTION control.account_payg_bucket_write_once();
ALTER TABLE control.account_payg
    DROP CONSTRAINT account_payg_funding_bucket_fkey;

DROP TABLE control.ledger_entries;
DROP TABLE control.settlements;
DROP TABLE control.funding_buckets;

DROP FUNCTION control.ledger_entries_append_only();
DROP FUNCTION control.settlements_append_only();
