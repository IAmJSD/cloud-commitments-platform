-- Intentionally a no-op: the up migration cannot tell the plans it promoted
-- from plans created as 'new', and demoting them would re-block sizing of
-- ramps that have never bought anything.
SELECT 1;
