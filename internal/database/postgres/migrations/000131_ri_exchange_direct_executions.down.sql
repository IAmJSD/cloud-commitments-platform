-- Reverse 000131: drop the direct-execute RI exchange audit trail (issue #82).
-- Lossy by design: every recorded direct execute is dropped with the table.
DROP TABLE IF EXISTS ri_exchange_direct_executions;
