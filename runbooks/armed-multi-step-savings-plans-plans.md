# Runbook: armed multi-step plans that include Savings Plans

Multi-step ramps do not support Savings Plans (platform#631). Step 1 suppresses
the whole SP recommendation for 7 days, so later steps find nothing and the ramp
strands. Plan create, update and arm now return HTTP 400 for an armed plan
(enabled and auto_purchase) with `total_steps > 1` and an enabled Savings Plans
service, and an already-armed one fails its next step with a visible failed row.

## Before deploying this change (read-only, run on DEV and prod)

Lists the armed plans that would start failing. Decide each hit first: re-create
it as a single-step (immediate) plan, remove the SP service, or disarm it.

```sql
SELECT p.id, p.name, p.ramp_schedule->>'type' AS ramp_type,
       (p.ramp_schedule->>'total_steps')::int AS total_steps,
       (p.ramp_schedule->>'current_step')::int AS current_step,
       p.next_execution_date,
       array_agg(s.key) AS sp_services
FROM purchase_plans p
CROSS JOIN LATERAL jsonb_each(p.services) AS s(key, val)
WHERE p.enabled AND p.auto_purchase
  AND COALESCE((p.ramp_schedule->>'total_steps')::int, 0) > 1
  AND COALESCE((s.val->>'enabled')::boolean, false)
  AND (s.val->>'service' = 'savingsplans' OR s.val->>'service' LIKE 'savings-plans-%')
GROUP BY p.id, p.name, p.ramp_schedule, p.next_execution_date;
```

Zero rows means nothing changes for existing plans.
