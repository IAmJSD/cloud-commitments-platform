-- Intentionally a no-op: the up migration cannot record which rows it cleared
-- without also keeping the stale class it exists to remove, and re-stamping
-- every NULL EC2 row 'convertible' would re-block Standard RIs (and overwrite
-- pre-000087 rows that were never stamped). Cleared rows that were already
-- re-fetched from AWS keep their authoritative class.
SELECT 1;
