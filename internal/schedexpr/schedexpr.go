// Package schedexpr parses the schedule expressions the deployment's
// scheduler uses to trigger recommendation collection and works out when
// the next run fires (issue #756).
//
// Three syntaxes are accepted, matching what the Terraform modules hand to
// their schedulers:
//
//   - EventBridge rate expressions, e.g. "rate(1 day)" (AWS Lambda/Fargate);
//   - EventBridge cron expressions with six fields, e.g.
//     "cron(0 2 * * ? *)" (AWS Lambda/Fargate);
//   - standard five-field cron, e.g. "0 2 * * *" (GCP Cloud Scheduler,
//     Kubernetes CronJobs, the Azure Logic App recurrence).
//
// Every scheduler involved evaluates its schedule in UTC, so Next works in
// UTC. The parser is deliberately strict: anything it does not fully
// understand is an error, and callers report "unknown" instead of a wrong
// time.
package schedexpr

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrEmpty is returned by Parse for an empty or blank expression, which the
// deployment uses to mean "scheduled collection is disabled".
var ErrEmpty = errors.New("empty schedule expression")

// Schedule is a parsed schedule expression.
type Schedule interface {
	// Next returns the first fire time strictly after t, in UTC. ok is
	// false when the fire time cannot be known from the expression alone
	// (rate expressions) or no fire time exists within the search horizon.
	Next(t time.Time) (next time.Time, ok bool)
}

// Parse parses expr in any of the supported syntaxes.
func Parse(expr string) (Schedule, error) {
	s := strings.TrimSpace(expr)
	if s == "" {
		return nil, ErrEmpty
	}
	if inner, ok := unwrap(s, "rate"); ok {
		return parseRate(inner)
	}
	if inner, ok := unwrap(s, "cron"); ok {
		return parseCron(strings.Fields(inner), eventBridgeDialect)
	}
	return parseCron(strings.Fields(s), unixDialect)
}

// unwrap returns the text between "<name>(" and the closing ")".
func unwrap(s, name string) (string, bool) {
	if !strings.HasPrefix(s, name+"(") || !strings.HasSuffix(s, ")") {
		return "", false
	}
	return s[len(name)+1 : len(s)-1], true
}

// rateSchedule is an EventBridge rate expression. EventBridge starts a rate
// schedule's clock when the rule is created, a moment the runtime cannot
// see, so the period is known but the phase is not: Next always reports
// unknown rather than guessing.
type rateSchedule struct {
	period time.Duration
}

// Next implements Schedule. See rateSchedule for why it is always unknown.
func (rateSchedule) Next(time.Time) (time.Time, bool) {
	return time.Time{}, false
}

var rateUnits = map[string]time.Duration{
	"minute": time.Minute,
	"hour":   time.Hour,
	"day":    24 * time.Hour,
}

// parseRate parses the "<value> <unit>" body of a rate expression with
// EventBridge's rules: a positive integer value, the singular unit for 1 and
// the plural unit for anything larger.
func parseRate(body string) (Schedule, error) {
	parts := strings.Fields(body)
	if len(parts) != 2 {
		return nil, fmt.Errorf("rate(%s): want \"<value> <unit>\"", body)
	}
	value, err := number(parts[0])
	if err != nil || value < 1 {
		return nil, fmt.Errorf("rate(%s): value must be a positive integer", body)
	}
	unit := parts[1]
	if value > 1 {
		if !strings.HasSuffix(unit, "s") {
			return nil, fmt.Errorf("rate(%s): unit must be plural for a value above 1", body)
		}
		unit = strings.TrimSuffix(unit, "s")
	}
	d, ok := rateUnits[unit]
	if !ok {
		return nil, fmt.Errorf("rate(%s): unit must be minute(s), hour(s) or day(s)", body)
	}
	return rateSchedule{period: time.Duration(value) * d}, nil
}
