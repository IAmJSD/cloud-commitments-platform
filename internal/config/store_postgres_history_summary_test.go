package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestExecutionSummaryQuery_DateBoundExemptsStatuses(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 7)
	exempt := []string{"pending", "notified"}

	query, args := executionSummaryQuery(HistorySummaryFilter{
		ExecutionStatuses:           []string{"pending", "failed"},
		ExecutionStart:              &start,
		ExecutionEnd:                &end,
		ExecutionDateExemptStatuses: exempt,
	})

	assert.Equal(t, []any{[]string{"pending", "failed"}, exempt, start, end}, args)
	assert.Contains(t, query, "(status = ANY($2) OR scheduled_date >= $3)")
	assert.Contains(t, query, "(status = ANY($2) OR scheduled_date <= $4)")
}

func TestExecutionSummaryQuery_NoExemptionKeepsPlainBound(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	query, args := executionSummaryQuery(HistorySummaryFilter{ExecutionStatuses: []string{"failed"}, ExecutionStart: &start})

	assert.Equal(t, []any{[]string{"failed"}, start}, args)
	assert.Contains(t, query, "AND scheduled_date >= $2")
	assert.NotContains(t, query, "status = ANY($2)")
}
