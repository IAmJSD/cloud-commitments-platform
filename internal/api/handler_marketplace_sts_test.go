package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #561: transient STS AssumeRole failures (throttling, 5xx, network
// errors, timeouts) must not be reported as the fixed 400 that tells the
// caller to fix the role; only permanent failures keep that 400.

const (
	marketplaceAssumeRoleThrottleXML = `<ErrorResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><Error><Type>Sender</Type><Code>Throttling</Code><Message>Rate exceeded for ` + marketplaceTenantRoleARN + `</Message></Error><RequestId>r</RequestId></ErrorResponse>`
	marketplaceAssumeRoleServerXML   = `<ErrorResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><Error><Type>Receiver</Type><Code>InternalFailure</Code><Message>internal error assuming ` + marketplaceTenantRoleARN + `</Message></Error><RequestId>r</RequestId></ErrorResponse>`
)

// failingAssumeRoleSTS fails every AssumeRole call at the transport level and
// answers every other STS call as the host account.
type failingAssumeRoleSTS struct{ err error }

func (f failingAssumeRoleSTS) Do(r *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(r.Body)
	if strings.Contains(string(b), "Action=AssumeRole") {
		return nil, f.err
	}
	return hostSTSTransport{}.Do(r)
}

// noRetry keeps the mocked SDK from backing off between attempts.
func noRetry() aws.Retryer { return aws.NopRetryer{} }

func activeListingRow() *config.PurchaseHistoryRecord {
	row := standardRow()
	row.ListingState = config.ListingStateActive
	row.ListingID = "ril-cancel"
	return row
}

func assertNoRoleLeak(t *testing.T, ce *clientError) {
	t.Helper()
	assert.NotEqual(t, marketplaceRoleAssumptionMessage, ce.message)
	assert.NotContains(t, ce.message, marketplaceHostAccount)
	assert.NotContains(t, ce.message, "222222222222")
	assert.NotContains(t, ce.message, "arn:aws:")
	assert.NotContains(t, ce.message, "Rate exceeded")
	assert.NotContains(t, ce.message, "internal error")
}

func TestMarketplaceCancel_TransientAssumeRoleFailureIsRetryable(t *testing.T) {
	cases := []struct {
		name       string
		client     aws.HTTPClient
		wantCode   int
		retryAfter bool
	}{
		{"throttling", assumeRoleSTS{400, marketplaceAssumeRoleThrottleXML}, 503, true},
		{"sts 5xx", assumeRoleSTS{500, marketplaceAssumeRoleServerXML}, 502, false},
		{"sts 503 empty body", assumeRoleSTS{503, ""}, 502, false},
		{"network error", failingAssumeRoleSTS{errors.New("dial tcp: connection refused")}, 502, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := realEC2Handler(t, activeListingRow(), assumeRoleSTS{})
			h.awsCfg.HTTPClient = tc.client
			h.awsCfg.Retryer = noRetry

			_, err := h.marketplaceCancel(context.Background(), marketplaceReq(), validMarketplacePurchaseID)

			ce, ok := IsClientError(err)
			require.True(t, ok, "expected a client error, got %v", err)
			assert.Equal(t, tc.wantCode, ce.code)
			assertNoRoleLeak(t, ce)
			if tc.retryAfter {
				assert.Equal(t, "5", ce.headers["Retry-After"])
			}
		})
	}
}

func TestMarketplaceList_ThrottledAssumeRoleIsRetryable(t *testing.T) {
	h := realEC2Handler(t, standardRow(), assumeRoleSTS{400, marketplaceAssumeRoleThrottleXML})
	h.awsCfg.Retryer = noRetry

	_, err := h.marketplaceList(context.Background(), marketplaceReq(), validMarketplacePurchaseID)

	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a client error, got %v", err)
	assert.Equal(t, 503, ce.code)
	assert.Equal(t, "5", ce.headers["Retry-After"])
	assertNoRoleLeak(t, ce)
}

// The access-denied class must keep the fixed 400 with retries disabled too.
func TestMarketplaceCancel_DeniedAssumeRoleKeeps400(t *testing.T) {
	h := realEC2Handler(t, activeListingRow(), assumeRoleSTS{403, marketplaceAssumeRoleDenyXML})
	h.awsCfg.Retryer = noRetry

	_, err := h.marketplaceCancel(context.Background(), marketplaceReq(), validMarketplacePurchaseID)

	assertTenantSafeRoleDenial(t, err)
}

func stsAssumeRoleErr(inner error) error {
	opErr := &smithy.OperationError{ServiceID: "STS", OperationName: "AssumeRole", Err: inner}
	return &smithy.OperationError{
		ServiceID:     "EC2",
		OperationName: "CancelReservedInstancesListing",
		Err:           fmt.Errorf("failed to refresh cached credentials, %w", opErr),
	}
}

func TestMapAWSMarketplaceError_AssumeRoleClasses(t *testing.T) {
	cases := []struct {
		name     string
		inner    error
		wantCode int
	}{
		{"access denied", &smithy.GenericAPIError{Code: "AccessDenied", Message: "no " + marketplaceTenantRoleARN, Fault: smithy.FaultClient}, 400},
		{"region disabled", &smithy.GenericAPIError{Code: "RegionDisabledException", Message: "sts disabled", Fault: smithy.FaultClient}, 400},
		{"unclassified error", errors.New("something odd about " + marketplaceTenantRoleARN), 400},
		{"throttling", &smithy.GenericAPIError{Code: "Throttling", Message: "Rate exceeded", Fault: smithy.FaultClient}, 503},
		{"request limit", &smithy.GenericAPIError{Code: "RequestLimitExceeded", Message: "Rate exceeded"}, 503},
		{"server fault", &smithy.GenericAPIError{Code: "InternalFailure", Message: "internal error", Fault: smithy.FaultServer}, 502},
		{"request timeout code", &smithy.GenericAPIError{Code: "RequestTimeout", Message: "internal error"}, 502},
		{"deadline exceeded", fmt.Errorf("request canceled, %w", context.DeadlineExceeded), 502},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ce, ok := IsClientError(mapAWSMarketplaceError("AWS cancel listing failed", stsAssumeRoleErr(tc.inner)))

			require.True(t, ok)
			assert.Equal(t, tc.wantCode, ce.code)
			assert.NotContains(t, ce.message, "arn:aws:")
			if tc.wantCode == 400 {
				assert.Equal(t, marketplaceRoleAssumptionMessage, ce.message)
				return
			}
			assertNoRoleLeak(t, ce)
		})
	}
}
