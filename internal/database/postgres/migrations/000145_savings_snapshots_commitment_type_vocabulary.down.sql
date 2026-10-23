-- Revert 000145 (issue #339): restore the two-value commitment_type CHECK.
--
-- Rows are mapped back to the only labels the old collector could emit:
-- savings-plan becomes 'SavingsPlan' and everything else 'RI'. This is lossy on
-- purpose. GCP committed-use rows go back to 'RI', which is what the pre-#339
-- code wrote for them.

ALTER TABLE savings_snapshots DROP CONSTRAINT IF EXISTS valid_commitment_type;

UPDATE savings_snapshots
SET commitment_type = CASE
        WHEN commitment_type = 'savings-plan' THEN 'SavingsPlan'
        ELSE 'RI'
    END
WHERE commitment_type NOT IN ('RI', 'SavingsPlan');

ALTER TABLE savings_snapshots
    ADD CONSTRAINT valid_commitment_type CHECK (commitment_type IN ('RI', 'SavingsPlan'));
