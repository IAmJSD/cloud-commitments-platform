ALTER TABLE global_config
    DROP COLUMN IF EXISTS auto_purchase_enabled;

ALTER TABLE purchase_plans
    DROP COLUMN IF EXISTS max_step_commitment;
