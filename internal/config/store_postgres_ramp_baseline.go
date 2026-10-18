package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// PlanRampQuantity is a quantity of one recommendation identity: whole units
// for count-based recs, the summed hourly_commitment for Savings Plans.
type PlanRampQuantity struct {
	Count  int     `json:"count"`
	Hourly float64 `json:"hourly,omitempty"`
}

// PlanRampBaseline maps RecIdentityKey to the base a ramp plan sizes against.
type PlanRampBaseline map[string]PlanRampQuantity

type rampLedgerState string

const (
	rampLedgerNew     rampLedgerState = "new"
	rampLedgerTrusted rampLedgerState = "trusted"
)

func (s rampLedgerState) allowsSizing(raw []byte, current int) bool {
	return s == rampLedgerTrusted && raw != nil || s == rampLedgerNew && raw == nil && current == 0
}

var ErrRampLedgerUntrusted = errors.New("this ramp's purchase history from before the retention ledger cannot be verified, so it cannot size further steps")

// SetPlanRampBaselineTx freezes a baseline only while retention can prove its lineage.
// The caller holds the plan lock; full-row plan writes cannot promote trust.
func (s *PostgresStore) SetPlanRampBaselineTx(ctx context.Context, tx pgx.Tx, planID string, baseline PlanRampBaseline) (bool, error) {
	if len(baseline) == 0 {
		return false, fmt.Errorf("refusing to freeze an empty ramp baseline for plan %s", planID)
	}
	raw, err := json.Marshal(baseline)
	if err != nil {
		return false, fmt.Errorf("failed to marshal ramp baseline of plan %s: %w", planID, err)
	}
	tag, err := tx.Exec(ctx,
		`UPDATE purchase_plans SET ramp_baseline = $2,
		 ramp_ledger_state = CASE WHEN ramp_ledger_protected(ramp_schedule) THEN 'trusted' ELSE ramp_ledger_state END
		 WHERE id = $1 AND ramp_baseline IS NULL
		 AND (COALESCE((ramp_schedule ->> 'total_steps')::int, 1) <= 1
		      OR (ramp_ledger_state = 'new' AND COALESCE((ramp_schedule ->> 'current_step')::int, 0) = 0))`, planID, raw)
	if err != nil {
		return false, fmt.Errorf("failed to write ramp baseline of plan %s: %w", planID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// GetPlanRampBaselineTx refuses cumulative sizing when retained history is untrusted.
// Fresh protected plans return nil until their baseline freezes under the plan lock.
func (s *PostgresStore) GetPlanRampBaselineTx(ctx context.Context, tx pgx.Tx, planID string) (PlanRampBaseline, error) {
	var raw []byte
	var state rampLedgerState
	var total, current int
	err := tx.QueryRow(ctx, `SELECT ramp_baseline, ramp_ledger_state,
		COALESCE((ramp_schedule ->> 'total_steps')::int, 1),
		COALESCE((ramp_schedule ->> 'current_step')::int, 0)
		FROM purchase_plans WHERE id = $1`, planID).Scan(&raw, &state, &total, &current)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: purchase plan %s", ErrNotFound, planID)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read ramp baseline of plan %s: %w", planID, err)
	}
	if total > 1 && !state.allowsSizing(raw, current) {
		return nil, ErrRampLedgerUntrusted
	}
	if raw == nil {
		return nil, nil
	}
	var baseline PlanRampBaseline
	if err := json.Unmarshal(raw, &baseline); err != nil {
		return nil, fmt.Errorf("failed to decode ramp baseline of plan %s: %w", planID, err)
	}
	return baseline, nil
}

// ListPlanCommittedTx returns, per recommendation identity, what the plan's
// executions already hold, committed or still in flight. It is the ledger the
// plan's next step subtracts from its cumulative target. It reads the
// executions themselves, never the suppressions, because a suppression expires
// while the capacity it covered is still held.
//
// A row counts unless it is provably unbought: canceled (both spellings),
// expired, or failed with NoSubmission set. A failed row without NoSubmission
// is an unknown outcome and counts, as does any status this code does not know
// (the fail-safe direction: over-counting only under-buys). Every selected rec
// of a counted row counts, including a failed rec of a partially_completed
// row, because a rec error can be a timeout after the provider accepted it.
//
// Within one step the root row of a fan-out, its per-account rows and a
// retry chain all carry the same recs, so each identity counts once per step
// as the largest quantity any counted row holds; steps then add up.
//
// The caller must hold the plan's lock (LockPurchasePlanTx) in tx or the
// answer is advisory.
func (s *PostgresStore) ListPlanCommittedTx(ctx context.Context, tx pgx.Tx, planID string) (map[string]PlanRampQuantity, error) {
	rows, err := tx.Query(ctx, `
		SELECT plan_id, execution_id, status, step_number, scheduled_date,
		       notification_sent, approval_token_hash, recommendations,
		       total_upfront_cost, estimated_savings, completed_at, error, expires_at,
		       cloud_account_id, source, approved_by, canceled_by, capacity_percent,
		       created_by_user_id, retry_execution_id, retry_attempt_n,
		       approval_token_expires_at,
		       executed_by_user_id, executed_at, pre_approval_skip_reason,
		       idempotency_key, scheduled_execution_at, no_submission, created_by_system
		FROM purchase_executions
		WHERE plan_id = $1`, planID)
	if err != nil {
		return nil, fmt.Errorf("failed to query executions of plan %s: %w", planID, err)
	}
	defer rows.Close()
	execs, err := scanExecutionRows(rows)
	if err != nil {
		return nil, err
	}
	return sumPlanCommitted(execs)
}

// holdsCapacity reports whether an execution row may hold bought or in-flight
// capacity. See ListPlanCommittedTx for the rule.
func holdsCapacity(e *PurchaseExecution) bool {
	switch e.Status {
	case StatusCanceled, LegacyStatusCanceled, StatusExpired:
		return false
	case StatusFailed:
		return !e.NoSubmission
	}
	return true
}

func sumPlanCommitted(execs []PurchaseExecution) (map[string]PlanRampQuantity, error) {
	type stepKey struct {
		identity string
		step     int
	}
	perStep := map[stepKey]PlanRampQuantity{}
	for i := range execs {
		e := &execs[i]
		if !holdsCapacity(e) {
			continue
		}
		held := map[string]PlanRampQuantity{}
		for j := range e.Recommendations {
			rec := &e.Recommendations[j]
			if !rec.Selected {
				continue
			}
			identity, err := RecIdentityKey(rec)
			if err != nil {
				return nil, fmt.Errorf("execution %s: %w", e.ExecutionID, err)
			}
			hourly, err := recHourlyCommitment(rec)
			if err != nil {
				return nil, fmt.Errorf("execution %s: %w", e.ExecutionID, err)
			}
			q := held[identity]
			q.Count += rec.Count
			q.Hourly += hourly
			held[identity] = q
		}
		for identity, q := range held {
			k := stepKey{identity, e.StepNumber}
			cur := perStep[k]
			cur.Count = max(cur.Count, q.Count)
			cur.Hourly = max(cur.Hourly, q.Hourly)
			perStep[k] = cur
		}
	}
	out := map[string]PlanRampQuantity{}
	for k, q := range perStep {
		sum := out[k.identity]
		sum.Count += q.Count
		sum.Hourly += q.Hourly
		out[k.identity] = sum
	}
	return out, nil
}

// recHourlyCommitment reads Details.hourly_commitment: 0 when the rec carries
// none, an error when it carries one that is not a number.
func recHourlyCommitment(rec *RecommendationRecord) (float64, error) {
	if len(rec.Details) == 0 {
		return 0, nil
	}
	var d struct {
		Hourly *float64 `json:"hourly_commitment"`
	}
	if err := json.Unmarshal(rec.Details, &d); err != nil {
		return 0, fmt.Errorf("recommendation details unreadable: %w", err)
	}
	if d.Hourly == nil {
		return 0, nil
	}
	return *d.Hourly, nil
}

// rampBaselineStale reports whether an edit changes what the frozen baseline
// describes: which services are bought (provider, service, term, payment of
// the enabled ones) or the ramp's shape (type, percent, steps, interval).
// Coverage and the current position are deliberately not part of it: the
// baseline holds demand counts, and the target scales with coverage. A stale
// baseline is cleared rather than reused, so the next step fails loud (every
// identity is skipped) instead of sizing against the wrong demand.
func rampBaselineStale(old, next *PurchasePlan) bool {
	shape := func(p *PurchasePlan) string {
		var svcs []string
		for k := range p.Services {
			svc := p.Services[k]
			if svc.Enabled {
				svcs = append(svcs, fmt.Sprintf("%s|%s|%d|%s", strings.ToLower(svc.Provider), svc.Service, svc.Term, svc.Payment))
			}
		}
		sort.Strings(svcs)
		r := p.RampSchedule
		return fmt.Sprintf("%s;%s|%v|%d|%d", strings.Join(svcs, ","), r.Type, r.PercentPerStep, r.TotalSteps, r.StepIntervalDays)
	}
	return shape(old) != shape(next)
}
