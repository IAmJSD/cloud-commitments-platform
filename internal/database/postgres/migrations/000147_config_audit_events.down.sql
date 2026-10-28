-- Reverse 000147: drop the configuration audit trail (issue #1493).
-- Lossy by design: every recorded event is dropped with the table.
DROP TABLE IF EXISTS config_audit_events;
