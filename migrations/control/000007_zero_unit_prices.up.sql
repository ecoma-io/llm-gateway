-- Migrations lane: control — the Control Plane's `control` database
-- (ADR 0006 §7). This file amends the accounting foundation's price
-- provenance guard: a consume leg's two unit prices drop their strict
-- positivity to a floor of zero.
--
-- 000007 — zero unit prices on consume legs:
--
--   `ledger_entries_price_snapshot` required both copied unit prices to be
--   strictly positive. Nothing else in the money chain does: the catalog's
--   price-list entries admit zero (`client_price_list` lane, Data Plane),
--   the fact contract's price fields carry `minimum: 0`, and the ledger
--   domain's own `validatePriceSnapshot` accepts a zero as a real price —
--   "a model the deployment gives away is priced at nothing". The strict
--   check made that documented configuration unbookable: the first settled
--   fact priced under a free-input or free-output alias produces a legal
--   consume leg whose INSERT the engine refuses, which wedges the feed on
--   a fact no page may skip past.
--
--   Zero is a floor, not an absence — the snapshot columns stay NOT NULL on
--   consume legs (the pairing half of this constraint is unchanged), the
--   amount keeps its own positivity, and a leg that consumes nothing still
--   books no consume leg at all (`BuildSettle` writes one only for a
--   consumed amount above zero). What changes is only this: a price of
--   zero is a sentence about money the ledger can now record.

ALTER TABLE control.ledger_entries
    DROP CONSTRAINT ledger_entries_price_snapshot;

ALTER TABLE control.ledger_entries
    ADD CONSTRAINT ledger_entries_price_snapshot
    CHECK (
        (kind = 'consume') = (price_revision_id IS NOT NULL)
        AND (kind = 'consume') = (input_unit_price IS NOT NULL)
        AND (kind = 'consume') = (output_unit_price IS NOT NULL)
        AND (price_revision_id IS NULL OR char_length(price_revision_id) BETWEEN 1 AND 256)
        AND (input_unit_price IS NULL OR input_unit_price >= 0)
        AND (output_unit_price IS NULL OR output_unit_price >= 0)
    );
