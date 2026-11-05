package purchase

import (
	"context"
	"fmt"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/jackc/pgx/v5"
)

// redshiftRedriveHorizon is how long after a lineage's first dispatch a
// successor may still be stamped as within horizon.
const redshiftRedriveHorizon = 24 * time.Hour

// isRedshiftRec reports whether rec dispatches to the AWS Redshift purchase
// path. It goes through mapServiceSlug, the table the executor dispatches on.
func isRedshiftRec(rec config.RecommendationRecord) bool {
	if rec.Provider != "" && rec.Provider != string(common.ProviderAWS) {
		return false
	}
	svc, ok := mapServiceSlug(rec.Service)
	return ok && (svc == common.ServiceRedshift || svc == common.ServiceDataWarehouse)
}

func hasRedshiftRec(exec *config.PurchaseExecution) bool {
	for i := range exec.Recommendations {
		if exec.Recommendations[i].Selected && isRedshiftRec(exec.Recommendations[i]) {
			return true
		}
	}
	return false
}

// stampRedshiftDispatch fixes the lineage's first dispatch time before any
// Redshift buy and records it on exec, so every dispatch of the lineage (and
// every fan-out account row, which copies exec) passes the same RequestedAt.
// Other executions are left untouched. A failure at attempt 0 provably
// submitted nothing; a successor may have, so its refusal is a plain error.
func (m *Manager) stampRedshiftDispatch(ctx context.Context, exec *config.PurchaseExecution) error {
	if !hasRedshiftRec(exec) {
		return nil
	}
	var res config.StampResult
	err := m.config.WithTx(ctx, func(tx pgx.Tx) error {
		var stampErr error
		res, stampErr = m.config.StampFirstDispatchTx(ctx, tx, exec.ExecutionID, redshiftRedriveHorizon)
		return stampErr
	})
	if err == nil && res.At == nil {
		err = fmt.Errorf("execution %s has no first dispatch time and none can be minted", exec.ExecutionID)
	}
	if err != nil {
		if exec.RetryAttemptN == 0 {
			return beforeSubmission(err)
		}
		return err
	}
	at := res.At.UTC()
	exec.FirstDispatchAt = &at
	return nil
}

// withDispatchTime defaults RequestedAt to now. A Redshift rec must arrive
// with the lineage stamp: defaulting would silently move go's dedupe window.
func withDispatchTime(rec config.RecommendationRecord, opts common.PurchaseOptions) (common.PurchaseOptions, error) {
	if !opts.RequestedAt.IsZero() {
		return opts, nil
	}
	if isRedshiftRec(rec) {
		return opts, fmt.Errorf("purchase[%s]: Redshift dispatch without a first dispatch time", opts.ExecutionID)
	}
	opts.RequestedAt = time.Now().UTC()
	return opts, nil
}
