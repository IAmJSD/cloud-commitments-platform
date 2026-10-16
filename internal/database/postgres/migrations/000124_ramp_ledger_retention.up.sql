ALTER TABLE purchase_plans ADD COLUMN ramp_ledger_state TEXT NOT NULL DEFAULT 'legacy'
    CHECK (ramp_ledger_state IN ('legacy', 'new', 'trusted'));

CREATE FUNCTION ramp_ledger_protected(schedule JSONB) RETURNS BOOLEAN
LANGUAGE SQL IMMUTABLE AS $$
    SELECT COALESCE((schedule ->> 'total_steps')::INT, 1) > 1
       AND COALESCE((schedule ->> 'current_step')::INT, 0)
         < COALESCE((schedule ->> 'total_steps')::INT, 1)
$$;

-- Keep status classification aligned with config.holdsCapacity.
CREATE FUNCTION ramp_ledger_holds(status TEXT, no_submission BOOLEAN) RETURNS BOOLEAN
LANGUAGE SQL IMMUTABLE AS $$
    SELECT status NOT IN ('canceled', 'cancelled', 'expired')
       AND NOT (status = 'failed' AND no_submission)
$$;

-- Keep the same coverage-independent fields as config.rampBaselineStale.
CREATE FUNCTION ramp_ledger_shape(services JSONB, schedule JSONB) RETURNS JSONB
LANGUAGE SQL IMMUTABLE AS $$
    SELECT jsonb_build_array(
        (SELECT jsonb_agg(shape ORDER BY shape) FROM (
            SELECT jsonb_build_array(lower(value ->> 'provider'), value ->> 'service',
                COALESCE((value ->> 'term')::INT, 0), COALESCE(value ->> 'payment', '')) AS shape
            FROM jsonb_each(COALESCE(NULLIF(services, 'null'::JSONB), '{}'::JSONB))
            WHERE COALESCE((value ->> 'enabled')::BOOLEAN, FALSE)
        ) enabled),
        schedule ->> 'type', schedule -> 'percent_per_step',
        schedule -> 'total_steps', schedule -> 'step_interval_days')
$$;

CREATE FUNCTION guard_ramp_ledger_state() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.ramp_ledger_state <> 'new' OR NOT ramp_ledger_protected(NEW.ramp_schedule)
           OR COALESCE((NEW.ramp_schedule ->> 'current_step')::INT, 0) <> 0
           OR NEW.ramp_baseline IS NOT NULL THEN
            NEW.ramp_ledger_state := 'legacy';
        END IF;
    ELSIF OLD.ramp_ledger_state = 'legacy'
       OR NOT ramp_ledger_protected(NEW.ramp_schedule)
       OR COALESCE((NEW.ramp_schedule ->> 'current_step')::INT, 0)
          < COALESCE((OLD.ramp_schedule ->> 'current_step')::INT, 0)
       OR (ramp_ledger_shape(OLD.services, OLD.ramp_schedule)
           IS DISTINCT FROM ramp_ledger_shape(NEW.services, NEW.ramp_schedule)
           AND NOT (OLD.ramp_ledger_state = 'new' AND OLD.id = NEW.id
               AND OLD.ramp_baseline IS NULL AND NEW.ramp_baseline IS NULL
               AND COALESCE((OLD.ramp_schedule ->> 'current_step')::INT, 0) = 0
               AND COALESCE((NEW.ramp_schedule ->> 'current_step')::INT, 0) = 0
               AND ramp_ledger_protected(OLD.ramp_schedule)
               AND NOT EXISTS (SELECT 1 FROM purchase_executions e
                   WHERE e.plan_id = OLD.id AND ramp_ledger_holds(e.status, e.no_submission))))
       OR (OLD.ramp_baseline IS NOT NULL AND OLD.ramp_baseline IS DISTINCT FROM NEW.ramp_baseline) THEN
        NEW.ramp_ledger_state := 'legacy';
    ELSIF OLD.ramp_ledger_state = 'trusted' THEN
        NEW.ramp_ledger_state := 'trusted';
    ELSIF NEW.ramp_ledger_state = 'trusted' AND OLD.ramp_baseline IS NULL
       AND NEW.ramp_baseline IS NOT NULL
       AND COALESCE((NEW.ramp_schedule ->> 'current_step')::INT, 0) = 0 THEN
        NEW.ramp_ledger_state := 'trusted';
    ELSIF NEW.ramp_baseline IS NOT NULL THEN
        NEW.ramp_ledger_state := 'legacy';
    ELSE
        NEW.ramp_ledger_state := 'new';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER guard_ramp_ledger_state BEFORE INSERT OR UPDATE ON purchase_plans
FOR EACH ROW EXECUTE FUNCTION guard_ramp_ledger_state();

CREATE FUNCTION retain_ramp_ledger_execution() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF ramp_ledger_holds(OLD.status, OLD.no_submission)
       AND EXISTS (SELECT 1 FROM purchase_plans p
                   WHERE p.id = OLD.plan_id AND ramp_ledger_protected(p.ramp_schedule)) THEN
        RETURN NULL;
    END IF;
    RETURN OLD;
END
$$;

CREATE TRIGGER retain_ramp_ledger_execution BEFORE DELETE ON purchase_executions
FOR EACH ROW EXECUTE FUNCTION retain_ramp_ledger_execution();
