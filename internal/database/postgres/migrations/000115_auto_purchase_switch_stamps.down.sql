ALTER TABLE global_config
    DROP COLUMN IF EXISTS auto_purchase_changed_by,
    DROP COLUMN IF EXISTS auto_purchase_changed_at,
    DROP COLUMN IF EXISTS auto_purchase_changed_via;
