package config

import (
	"fmt"
	"time"
)

// ExecutionsByStatusesQuery selects executions for the History view's
// actionable class (issue #632). Statuses and Limit mean what they mean for
// GetExecutionsByStatuses. The other fields push the History filters and the
// caller's account scope into SQL, so the Limit cap applies to rows the caller
// can be shown rather than to every tenant's rows.
//
// Every predicate is a superset of the History handler's Go check
// (matchesExecution and the account scope), which still runs on the result:
// SQL may keep a row Go then drops, never the reverse.
type ExecutionsByStatusesQuery struct {
	// Start and End bound scheduled_date, inclusive. nil leaves a side open.
	Start *time.Time
	End   *time.Time
	// Scope, when non-nil, keeps only executions the account scope can allow.
	// nil is an unrestricted caller.
	Scope *ExecutionScope
	// Provider keeps executions with at least one recommendation for this
	// provider. "" applies no provider filter.
	Provider string
	// PlanID matches purchase_executions.plan_id exactly, as
	// HistoryExecutionsPageQuery.PlanID does. "" applies no plan filter.
	PlanID   string
	Statuses []string
	// AccountIDs keeps executions whose cloud_account_id is one of these
	// values, or, when cloud_account_id is NULL, that have a recommendation
	// whose cloud_account_id is one of them. Empty applies no account filter.
	AccountIDs []string
	Limit      int
}

// ExecutionScope is the SQL form of a restricted account scope.
type ExecutionScope struct {
	// UserID is the caller. An execution with no cloud_account_id is kept when
	// this user created it, because a row that cannot be attributed to one
	// account is shown to its creator only. "" matches a NULL creator.
	UserID string
	// AccountIDs lists every account value the scope can allow: the scope's
	// literal entries plus the UUID and external number of each registered
	// account it allows. Empty keeps no attributed execution (fail closed).
	AccountIDs []string
}

// executionRecsArray is the recommendations column as a JSON array. A
// non-array value (json.Marshal of a nil slice stores 'null') reads as empty,
// as encoding/json decodes it to an empty slice.
const executionRecsArray = `(CASE WHEN jsonb_typeof(recommendations) = 'array' THEN recommendations ELSE '[]'::jsonb END)`

// executionRecHasValue is true when a recommendation's JSON key (the lowercase
// tag json.Marshal writes for RecommendationRecord) equals one of the values
// bound at the given placeholder.
func executionRecHasValue(key string, arg int) string {
	return fmt.Sprintf(`EXISTS (SELECT 1 FROM jsonb_array_elements(%s) AS r WHERE r.value->>'%s' = ANY($%d))`,
		executionRecsArray, key, arg)
}

// executionAccountIn is true when the execution's account is one of the values
// at the given placeholder: cloud_account_id when set, else any
// recommendation's cloud_account_id. The handler's effective account is
// cloud_account_id, else the account every recommendation shares, so this is
// a superset of it.
func executionAccountIn(arg int) string {
	return fmt.Sprintf(`(cloud_account_id::text = ANY($%d) OR (cloud_account_id IS NULL AND %s))`,
		arg, executionRecHasValue("cloud_account_id", arg))
}

// executionsByStatusesWhere builds the WHERE conditions and bind args for q.
// $1 is the status list, as historyExecutionsPredicate expects.
func executionsByStatusesWhere(q *ExecutionsByStatusesQuery) (conds []string, args []any) {
	args = []any{q.Statuses}
	conds = []string{historyExecutionsPredicate}
	bind := func(v any) int {
		args = append(args, v)
		return len(args)
	}
	if q.Start != nil {
		conds = append(conds, fmt.Sprintf("scheduled_date >= $%d", bind(*q.Start)))
	}
	if q.End != nil {
		conds = append(conds, fmt.Sprintf("scheduled_date <= $%d", bind(*q.End)))
	}
	if q.PlanID != "" {
		conds = append(conds, fmt.Sprintf("plan_id = $%d", bind(q.PlanID)))
	}
	if q.Provider != "" {
		conds = append(conds, executionRecHasValue("provider", bind([]string{q.Provider})))
	}
	if len(q.AccountIDs) > 0 {
		conds = append(conds, executionAccountIn(bind(q.AccountIDs)))
	}
	if q.Scope != nil {
		accounts := bind(append([]string{}, q.Scope.AccountIDs...))
		user := bind(q.Scope.UserID)
		conds = append(conds, fmt.Sprintf(
			`(cloud_account_id::text = ANY($%d) OR (cloud_account_id IS NULL AND (COALESCE(created_by_user_id::text, '') = $%d OR %s)))`,
			accounts, user, executionRecHasValue("cloud_account_id", accounts)))
	}
	return conds, args
}
