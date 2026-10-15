-- Reverse 000123: drop the fan-out shape pins (issue #97).
--
-- Lossy by design: every pinned shape is dropped, and re-applying the up
-- migration restores only the "multi" pins its backfill can prove from
-- per-account rows. "single" pins are not recoverable, so their lineages fall
-- back to the live plan accounts until their next submission pins them again.
DROP TABLE IF EXISTS purchase_execution_fan_out_shapes;
