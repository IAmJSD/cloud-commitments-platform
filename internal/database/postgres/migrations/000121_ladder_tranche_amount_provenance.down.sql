ALTER TABLE ladder_tranches DROP CONSTRAINT ladder_tranche_amount_provenance;
ALTER TABLE ladder_tranches
    DROP COLUMN commitment_rate,
    DROP COLUMN amount_basis;
