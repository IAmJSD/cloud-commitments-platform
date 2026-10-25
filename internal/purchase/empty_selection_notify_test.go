package purchase

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// unselectedRecs is a plan step whose recommendations are all deselected, so
// processPurchaseRecommendations short-circuits before any cloud API call.
func unselectedRecs() []config.RecommendationRecord {
	return []config.RecommendationRecord{
		{Provider: "aws", Service: "ec2", ResourceType: "m5.large", Region: "us-east-1", Count: 1, UpfrontCost: 300, Savings: 50},
		{Provider: "aws", Service: "rds", ResourceType: "db.r5.large", Region: "us-east-1", Count: 1, UpfrontCost: 600, Savings: 90},
	}
}

// TestExecutePurchase_SingleAccount_NothingSelected_SendsNoConfirmation is the
// #339 (A05-015) regression guard for the single-account path: a run with
// nothing selected buys nothing, so it must not email a "purchase confirmed"
// for zero commitments and $0.
func TestExecutePurchase_SingleAccount_NothingSelected_SendsNoConfirmation(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockEmail := new(MockEmailSender)
	mockFactory := new(MockProviderFactory)

	plan := &config.PurchasePlan{ID: "plan-339", Name: "Plan 339"}
	exec := &config.PurchaseExecution{
		ExecutionID:     "exec-339-single",
		PlanID:          "plan-339",
		StepNumber:      1,
		Recommendations: unselectedRecs(),
	}
	mockStore.On("GetPurchasePlan", mock.Anything, "plan-339").Return(plan, nil)
	mockEmail.On("SendPurchaseConfirmation", mock.Anything, mock.Anything).Return(nil).Maybe()

	manager := &Manager{
		config:          mockStore,
		email:           mockEmail,
		providerFactory: mockFactory,
		dashboardURL:    "https://dashboard.example.com",
	}

	require.NoError(t, manager.executePurchase(ctx, exec))

	mockEmail.AssertNotCalled(t, "SendPurchaseConfirmation", mock.Anything, mock.Anything)
	mockFactory.AssertNotCalled(t, "CreateAndValidateProvider", mock.Anything, mock.Anything, mock.Anything)
	for i := range exec.Recommendations {
		assert.False(t, exec.Recommendations[i].Purchased)
	}
}

// TestExecuteForAccount_NothingSelected_SendsNoConfirmation is the same #339
// guard for the multi-account fan-out path, which gated the confirmation on
// "no per-rec errors" and so also confirmed an account that bought nothing.
func TestExecuteForAccount_NothingSelected_SendsNoConfirmation(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockEmail := new(MockEmailSender)
	allowFailedNotification(mockEmail)
	mockFactory := new(MockProviderFactory)

	const acctID = "aaaaaaaa-0000-0000-0000-000000000339"
	account := config.CloudAccount{ID: acctID, Name: "Prod", Provider: "aws", ExternalID: "111111111111", AWSAuthMode: "access_keys"}
	baseExec := &config.PurchaseExecution{
		ExecutionID:     "exec-339-base",
		PlanID:          "plan-339",
		Source:          common.PurchaseSourceWeb,
		Recommendations: unselectedRecs(),
	}
	plan := &config.PurchasePlan{ID: "plan-339", Name: "Plan 339"}

	var savedExec *config.PurchaseExecution
	mockStore.SavePurchaseExecutionFn = func(_ context.Context, e *config.PurchaseExecution) error {
		c := *e
		savedExec = &c
		return nil
	}
	mockEmail.On("SendPurchaseConfirmation", mock.Anything, mock.Anything).Return(nil).Maybe()

	credStore := &MockCredentialStore{
		LoadRawFn: func(_ context.Context, _, _ string) ([]byte, error) {
			return []byte(`{"access_key_id":"AKIAIOSFODNN7EXAMPLE","secret_access_key":"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}`), nil
		},
	}

	manager := &Manager{
		config:          mockStore,
		email:           mockEmail,
		providerFactory: mockFactory,
		credStore:       credStore,
		dashboardURL:    "https://dashboard.example.com",
	}

	committed, err := manager.executeForAccount(ctx, baseExec, plan, account)
	require.NoError(t, err)
	assert.False(t, committed, "nothing was selected, so nothing committed")

	require.NotNil(t, savedExec, "the per-account row is still saved")
	mockEmail.AssertNotCalled(t, "SendPurchaseConfirmation", mock.Anything, mock.Anything)
	mockFactory.AssertNotCalled(t, "CreateAndValidateProvider", mock.Anything, mock.Anything, mock.Anything)
}
