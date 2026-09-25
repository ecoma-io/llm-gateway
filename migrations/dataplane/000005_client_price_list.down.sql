-- The inverse of 000005_client_price_list.up.sql, in reverse creation order,
-- no CASCADE. These tables are configuration the operator wrote, not history
-- the ledger needs: dropping them removes the prices admission reads, and a
-- database without them refuses to price anything — the same refusal the
-- schema's two-step selection answers with when no effective revision or no
-- entry exists.

ALTER TABLE quota_projections
    DROP CONSTRAINT quota_projections_payg_scope_wildcard;
DROP TABLE client_price_list_entries;
DROP TABLE client_price_list_revisions;
