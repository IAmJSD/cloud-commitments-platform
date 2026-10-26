-- Issue #339 (audit finding A06-019): savings_snapshots.commitment_type uses
-- the pkg/common CommitmentType vocabulary.
--
-- The collector labeled every commitment "RI" except a service literally named
-- "SavingsPlans", which purchase_history never stores (Savings Plans are
-- "savingsplans" or a "savings-plans-<type>" slug). So GCP committed-use
-- discounts and AWS Savings Plans were both stored as "RI", and the CHECK only
-- admitted the two literals that mapper could emit. The collector now derives
-- the type from (provider, service) as one of the pkg/common values:
-- reserved-instance, savings-plan, committed-use, reserved-capacity.
--
-- The CHECK keeps admitting the legacy 'RI' / 'SavingsPlan' literals so a
-- replica still on the old binary can write during a rolling deploy without
-- failing its collection run. The collector never emits them, and the backfill
-- below rewrites every existing legacy row.
--
-- The whole file runs in one transaction (golang-migrate), and ALTER TABLE
-- holds an ACCESS EXCLUSIVE lock on savings_snapshots and its partitions until
-- commit. A concurrent collector insert waits for the commit and is then
-- checked against the new constraint, which admits both vocabularies, so it
-- cannot fail because of this migration. All partitions are created with
-- PARTITION OF, so they inherit the constraint from the parent, and DROP/ADD on
-- the parent applies to every partition.

ALTER TABLE savings_snapshots DROP CONSTRAINT IF EXISTS valid_commitment_type;

-- Relabel existing rows from (provider, service), as the collector now does.
UPDATE savings_snapshots
SET commitment_type = CASE
        WHEN commitment_type = 'SavingsPlan' THEN 'savings-plan'
        WHEN lower(provider) = 'gcp' THEN 'committed-use'
        WHEN lower(service) IN (
            'savingsplans', 'savings-plans',
            'savings-plans-compute', 'savings-plans-ec2instance',
            'savings-plans-sagemaker', 'savings-plans-database'
        ) THEN 'savings-plan'
        ELSE 'reserved-instance'
    END
WHERE commitment_type IN ('RI', 'SavingsPlan');

ALTER TABLE savings_snapshots
    ADD CONSTRAINT valid_commitment_type CHECK (commitment_type IN (
        'reserved-instance', 'savings-plan', 'committed-use', 'reserved-capacity',
        'RI', 'SavingsPlan'
    ));
