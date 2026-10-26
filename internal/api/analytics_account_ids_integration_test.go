//go:build integration

package api

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
)

// Issue #1429: with several accounts selected, /history/analytics and
// /history/breakdown total exactly the selected accounts, on real Postgres.
// Account B's rows carry only the external account id (cloud_account_id NULL).
func TestAnalyticsAccountIDs_SumsSelectedAccountsOnly_DB(t *testing.T) {
	ctx := context.Background()
	container := testhelpers.RequireMigratedDatabase(ctx, t, "../database/postgres/migrations")
	store := config.NewPostgresStore(container.DB)

	ids := map[string]string{}
	for name, ext := range map[string]string{"a": "810000000001", "b": "810000000002", "c": "810000000003"} {
		acct := config.CloudAccount{Name: name, Provider: "aws", ExternalID: ext, AWSAuthMode: "access_keys", Enabled: true}
		require.NoError(t, store.CreateCloudAccount(ctx, &acct))
		ids[name] = acct.ID
	}

	now := time.Now().UTC().Truncate(time.Second)
	seed := func(name, ext string, byUUID bool, savings float64) {
		rec := &config.PurchaseHistoryRecord{
			AccountID: ext, PurchaseID: "p-" + name, Timestamp: now, Provider: "aws", Service: "ec2",
			Region: "us-east-1", ResourceType: "m5.large", Count: 1, Term: 1, Payment: "no-upfront",
			UpfrontCost: new(float64), EstimatedSavings: savings,
		}
		if byUUID {
			rec.CloudAccountID = new(string)
			*rec.CloudAccountID = ids[name]
		}
		require.NoError(t, store.SavePurchaseHistory(ctx, rec))
	}
	seed("a", "810000000001", true, 10)
	seed("b", "810000000002", false, 20)
	seed("c", "810000000003", true, 40)

	newHandler := func(allowed []string) *Handler {
		auth := new(MockAuthService)
		auth.On("ValidateSession", mock.Anything, "tok").Return(&Session{UserID: "u-1"}, nil)
		auth.On("HasPermissionAPI", mock.Anything, "u-1", mock.Anything, mock.Anything).Return(true, nil)
		auth.On("GetAllowedAccountsAPI", mock.Anything, "u-1").Return(allowed, nil)
		return &Handler{auth: auth, config: store, analyticsClient: NewPostgresAnalyticsClient(container.DB)}
	}
	req := &events.LambdaFunctionURLRequest{Headers: map[string]string{"Authorization": "Bearer tok"}}
	window := map[string]string{
		"start": now.Add(-time.Hour).Format(time.RFC3339), "end": now.Add(time.Hour).Format(time.RFC3339),
	}
	query := func(h *Handler, accountIDs string) (*AnalyticsResponse, error) {
		p := map[string]string{"account_ids": accountIDs}
		for k, v := range window {
			p[k] = v
		}
		out, err := h.getHistoryAnalytics(ctx, req, p)
		if err != nil {
			return nil, err
		}
		return out.(*AnalyticsResponse), nil
	}

	admin := newHandler(nil)
	for _, tc := range []struct {
		ids  string
		want float64
	}{
		{ids["a"], 10},
		{ids["a"] + "," + ids["b"], 30},
		{ids["b"] + "," + ids["c"], 60},
		{ids["a"] + "," + ids["b"] + "," + ids["c"], 70},
	} {
		got, err := query(admin, tc.ids)
		require.NoError(t, err)
		assert.InDelta(t, tc.want, got.Summary.TotalMonthlySavings, 1e-9, tc.ids)
	}

	// A scoped user (scope entries are account IDs, #1034) sees the sum of
	// their accounts; a foreign id in the list refuses the whole request
	// (404), never a partial or all-account total.
	scoped := newHandler([]string{ids["a"], ids["b"]})
	got, err := query(scoped, ids["a"]+","+ids["b"])
	require.NoError(t, err)
	assert.InDelta(t, 30, got.Summary.TotalMonthlySavings, 1e-9)
	_, err = query(scoped, ids["a"]+","+ids["c"])
	require.Error(t, err)
	assert.True(t, IsNotFoundError(err))

	p := map[string]string{"account_ids": ids["b"] + "," + ids["c"], "dimension": "service"}
	for k, v := range window {
		p[k] = v
	}
	out, err := admin.getHistoryBreakdown(ctx, req, p)
	require.NoError(t, err)
	ec2 := out.(*BreakdownResponse).Data["ec2"]
	assert.Equal(t, 2, ec2.PurchaseCount)
	assert.InDelta(t, 60, ec2.TotalSavings, 1e-9)
}
