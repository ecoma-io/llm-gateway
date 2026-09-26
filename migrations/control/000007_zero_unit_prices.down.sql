-- The inverse of 000007_zero_unit_prices.up.sql: restore the strict
-- positivity the price snapshot carried before it. The restoration is only
-- sound while no zero-priced consume leg exists — a ledger that has already
-- recorded a free-priced leg cannot re-tighten the guard without refusing
-- its own history, which is the down lane refusing to run rather than a
-- migration defect.

ALTER TABLE control.ledger_entries
    DROP CONSTRAINT ledger_entries_price_snapshot;

ALTER TABLE control.ledger_entries
    ADD CONSTRAINT ledger_entries_price_snapshot
    CHECK (
        (kind = 'consume') = (price_revision_id IS NOT NULL)
        AND (kind = 'consume') = (input_unit_price IS NOT NULL)
        AND (kind = 'consume') = (output_unit_price IS NOT NULL)
        AND (price_revision_id IS NULL OR char_length(price_revision_id) BETWEEN 1 AND 256)
        AND (input_unit_price IS NULL OR input_unit_price > 0)
        AND (output_unit_price IS NULL OR output_unit_price > 0)
    );
