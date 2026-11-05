ALTER TABLE purchase_executions ADD COLUMN IF NOT EXISTS first_dispatch_at timestamptz NULL;
