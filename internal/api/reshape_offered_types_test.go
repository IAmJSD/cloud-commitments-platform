package api

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-go/providers/aws/recommendations"
	ec2svc "github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/ec2"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// lockedBuffer is an io.Writer safe for the logger's concurrent writes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureWarnings routes the default pkg/logging logger into a buffer at warn
// level for the rest of the test. Tests using it must not call t.Parallel.
func captureWarnings(t *testing.T) *lockedBuffer {
	t.Helper()
	out := &lockedBuffer{}
	prevOut := logging.SetOutput(out)
	prevLevel := logging.GetLevel()
	logging.SetLevelValue(logging.LevelWarn)
	t.Cleanup(func() {
		logging.SetOutput(prevOut)
		logging.SetLevelValue(prevLevel)
	})
	return out
}

const reshapeCatalogRegion = "eu-west-3"

// newCatalogReshapeHandler builds a reshape handler over one convertible
// 1x m5.4xlarge RI (32 normalized units) at 50% utilization (16 used). On
// the legacy ladder that reshapes to 1x m5.2xlarge; with a catalog lacking
// m5.2xlarge it must become 2x m5.xlarge.
func newCatalogReshapeHandler(t *testing.T, ec2 *fakeReshapeEC2Stub) *Handler {
	t.Helper()
	ec2.instances = []ec2svc.ConvertibleRI{
		{
			ReservedInstanceID: "ri-1", InstanceType: "m5.4xlarge", InstanceCount: 1, CurrencyCode: "USD", Duration: 31536000,
			FixedPrice: 876, // priced: an unpriced RI is excluded from reshape (#1188)
		},
	}
	mockStore := &MockConfigStore{}
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	mockStore.On("ListStoredRecommendations", mock.Anything, mock.Anything).
		Return([]config.RecommendationRecord(nil), nil)
	h := &Handler{
		config:            mockStore,
		auth:              &mockAuthForExchange{},
		reshapeEC2Factory: func(_ aws.Config) reshapeEC2Client { return ec2 },
		reshapeRecsFactory: func(_ aws.Config) reshapeRecsClient {
			return &fakeReshapeRecsStub{utilization: []recommendations.RIUtilization{
				{ReservedInstanceID: "ri-1", UtilizationPercent: 50.0},
			}}
		},
		reshapeAccountResolver: func(_ context.Context) (string, error) { return "", nil },
	}
	h.awsCfgOnce.Do(func() { h.awsCfg = aws.Config{Region: reshapeCatalogRegion} })
	return h
}

func reshapeOnce(t *testing.T, h *Handler) *ReshapeRecommendationsResponse {
	t.Helper()
	req := &events.LambdaFunctionURLRequest{
		Headers:               map[string]string{"authorization": "Bearer test-token"},
		QueryStringParameters: map[string]string{"lookback_days": "30", "threshold": "95.0"},
	}
	out, err := h.getReshapeRecommendations(context.Background(), req)
	require.NoError(t, err)
	resp, ok := out.(*ReshapeRecommendationsResponse)
	require.True(t, ok)
	require.Len(t, resp.Recommendations, 1)
	return resp
}

func TestGetReshapeRecommendations_CatalogLimitsTargetToOfferedSizes(t *testing.T) {
	ec2 := &fakeReshapeEC2Stub{offered: []string{"m5.4xlarge", "m5.xlarge", "c5.large"}}
	rec := reshapeOnce(t, newCatalogReshapeHandler(t, ec2)).Recommendations[0]

	assert.Equal(t, 1, ec2.offeredCalls, "the reshape handler must query the region's instance type catalog")
	assert.Equal(t, "m5.xlarge", rec.TargetInstanceType,
		"m5.2xlarge is not offered, so the target must step down to an offered size")
	assert.Equal(t, int32(2), rec.TargetCount)
}

func TestGetReshapeRecommendations_CatalogMissingFamilySurfacesReason(t *testing.T) {
	ec2 := &fakeReshapeEC2Stub{offered: []string{"c5.large", "c5.xlarge"}}
	rec := reshapeOnce(t, newCatalogReshapeHandler(t, ec2)).Recommendations[0]

	assert.Empty(t, rec.TargetInstanceType, "an unoffered family must not get a target")
	assert.Zero(t, rec.TargetCount)
	assert.Contains(t, rec.Reason, "instance family not offered in region (m5)")
}

// A catalog error or an empty catalog falls back to the legacy ladder with a
// warning naming the region, and the fallback is not cached: the next request
// queries the catalog again.
func TestGetReshapeRecommendations_CatalogUnavailableFallsBackWithWarning(t *testing.T) {
	cases := []struct {
		name    string
		ec2     *fakeReshapeEC2Stub
		warning string
	}{
		{
			name:    "catalog error",
			ec2:     &fakeReshapeEC2Stub{offeredErr: errors.New("UnauthorizedOperation: ec2:DescribeInstanceTypeOfferings")},
			warning: "failed, proceeding without the catalog: UnauthorizedOperation",
		},
		{
			name:    "empty catalog",
			ec2:     &fakeReshapeEC2Stub{offered: []string{}},
			warning: "came back empty, proceeding without the catalog",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureWarnings(t)
			h := newCatalogReshapeHandler(t, tc.ec2)

			for i := 1; i <= 2; i++ {
				rec := reshapeOnce(t, h).Recommendations[0]
				assert.Equal(t, "m5.2xlarge", rec.TargetInstanceType, "request %d must use the legacy ladder", i)
				assert.Equal(t, int32(1), rec.TargetCount, "request %d", i)
				assert.Equal(t, i, tc.ec2.offeredCalls, "request %d must query the catalog again, not reuse a cached fallback", i)
			}

			out := logs.String()
			assert.Contains(t, out, `region "`+reshapeCatalogRegion+`"`)
			assert.Contains(t, out, tc.warning)
		})
	}
}

func TestOfferedInstanceTypes(t *testing.T) {
	ctx := context.Background()
	t.Run("returns the catalog unchanged", func(t *testing.T) {
		logs := captureWarnings(t)
		got := OfferedInstanceTypes(ctx, "us-east-1", func(context.Context) ([]string, error) {
			return []string{"m5.large", "c5.xlarge"}, nil
		})
		assert.Equal(t, []string{"m5.large", "c5.xlarge"}, got)
		assert.Empty(t, logs.String(), "a usable catalog must not log a warning")
	})
	t.Run("error returns nil and warns", func(t *testing.T) {
		logs := captureWarnings(t)
		got := OfferedInstanceTypes(ctx, "us-east-1", func(context.Context) ([]string, error) {
			return []string{"m5.large"}, errors.New("throttled")
		})
		assert.Nil(t, got, "a partial result next to an error must not be used as the catalog")
		assert.Contains(t, logs.String(), `region "us-east-1" failed, proceeding without the catalog: throttled`)
	})
	t.Run("empty returns nil and warns", func(t *testing.T) {
		logs := captureWarnings(t)
		got := OfferedInstanceTypes(ctx, "ap-south-2", func(context.Context) ([]string, error) { return nil, nil })
		assert.Nil(t, got)
		assert.Contains(t, logs.String(), `region "ap-south-2" came back empty`)
	})
}
