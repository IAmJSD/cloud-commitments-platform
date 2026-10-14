package purchase

import (
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/mock"
)

// expectStepRows registers the plan lock and the per-step row listing that
// getOrCreateExecution reads under the ramp lock.
func expectStepRows(m *MockConfigStore, plan *config.PurchasePlan, rows ...config.PurchaseExecution) {
	locked := *plan
	locked.Enabled = true
	m.On("LockPurchasePlanTx", mock.Anything, mock.Anything, plan.ID).Return(&locked, nil)
	m.On("ListExecutionsForPlanStepTx", mock.Anything, mock.Anything, plan.ID, plan.RampSchedule.CurrentStep+1).Return(rows, nil)
}
