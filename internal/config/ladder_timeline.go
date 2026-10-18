package config

import (
	"context"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
)

const LadderTimelinePageSize = 100

type LadderTimelineCursor struct {
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
}

type LadderTimelineEvent struct {
	CreatedAt      time.Time            `json:"created_at"`
	ScheduledDate  time.Time            `json:"scheduled_date"`
	ExecutionID    *string              `json:"execution_id,omitempty"`
	ID             string               `json:"id"`
	RunID          string               `json:"run_id"`
	ConfigID       string               `json:"config_id"`
	AmountUSDHr    string               `json:"amount_usd_hr"`
	AmountBasis    *LadderAmountBasis   `json:"amount_basis,omitempty"`
	CommitmentRate *string              `json:"commitment_rate,omitempty"`
	ODUSDHr        string               `json:"od_usd_hr"`
	Revision       int64                `json:"revision"`
	Layer          ladder.LayerType     `json:"layer_type"`
	Term           ladder.Term          `json:"term"`
	Payment        ladder.PaymentOption `json:"payment_option"`
	Status         ladder.TrancheStatus `json:"status"`
	RunStatus      ladder.RunStatus     `json:"run_status"`
}

type LadderTimelinePage struct {
	Events       []LadderTimelineEvent `json:"events"`
	NextCursor   *LadderTimelineCursor `json:"next_cursor,omitempty"`
	TotalUSDHr   string                `json:"total_usd_hr"`
	TotalODUSDHr string                `json:"total_od_usd_hr"`
	TotalCount   int64                 `json:"total_count"`
}

type LadderTimelineRun struct {
	Actions           []LadderTimelineAction `json:"actions"`
	StartedAt         time.Time              `json:"started_at"`
	CreatedAt         time.Time              `json:"created_at"`
	BaselineUSDHr     *string                `json:"baseline_usd_hr"`
	TargetUSDHr       *string                `json:"target_usd_hr"`
	ExistingUSDHr     *string                `json:"existing_usd_hr"`
	GapUSDHr          *string                `json:"gap_usd_hr"`
	ID                string                 `json:"id"`
	ConfigID          string                 `json:"config_id"`
	TotalHourlyCommit string                 `json:"total_hourly_commit"`
	Status            ladder.RunStatus       `json:"status"`
}

type LadderTimelineAction struct {
	DataSources []string `json:"data_sources,omitempty"`
	Action      string   `json:"action"`
	Layer       string   `json:"layer"`
	Rationale   string   `json:"rationale"`
}

type LadderTimelineRunPage struct {
	Runs       []LadderTimelineRun   `json:"runs"`
	NextCursor *LadderTimelineCursor `json:"next_cursor,omitempty"`
	TotalCount int64                 `json:"total_count"`
}

type LadderTimelineStore interface {
	ListLadderTimeline(context.Context, string, string, *LadderTimelineCursor) (*LadderTimelinePage, error)
	ListLadderTimelineRuns(context.Context, string, string, *LadderTimelineCursor) (*LadderTimelineRunPage, error)
}
