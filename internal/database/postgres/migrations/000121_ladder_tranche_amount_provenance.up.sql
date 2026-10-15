ALTER TABLE ladder_tranches
    ADD COLUMN amount_basis TEXT,
    ADD COLUMN commitment_rate TEXT;

ALTER TABLE ladder_tranches ADD CONSTRAINT ladder_tranche_amount_provenance CHECK (
    (amount_basis IS NULL AND commitment_rate IS NULL)
    OR (amount_basis IS NOT NULL AND (
        (amount_basis = 'on_demand_equivalent' AND commitment_rate IS NULL)
        OR (amount_basis = 'commitment' AND commitment_rate IS NOT NULL AND commitment_rate <> '')
    ))
) NOT VALID;
