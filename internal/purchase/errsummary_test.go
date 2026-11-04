package purchase

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"google.golang.org/api/googleapi"
)

const fakeAWS403 = "operation error EC2: PurchaseReservedInstancesOffering, https response error StatusCode: 403, " +
	"RequestID: 11111111-2222-3333-4444-555555555555, api error UnauthorizedOperation: You are not authorized to perform this operation. " +
	"User: arn:aws:sts::123456789012:assumed-role/cudly-role/session is not authorized. " +
	"Encoded authorization failure message: AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"

func TestSanitizeStoredError_AWS403(t *testing.T) {
	out := SanitizeStoredError(fakeAWS403)
	assert.Equal(t, ErrCategoryMessage(ErrCategoryAccessDenied), out)
	for _, bad := range []string{"arn:", "123456789012", "RequestID", "Encoded"} {
		assert.NotContains(t, out, bad)
	}
}

func TestSanitizeStoredError_Samples(t *testing.T) {
	cases := map[string]string{
		"RESPONSE 403: 403 Forbidden\nERROR CODE: AuthorizationFailed":          ErrCategoryAccessDenied,
		"googleapi: Error 403: Required permission denied, forbidden":           ErrCategoryAccessDenied,
		"operation error EC2: X, api error InsufficientInstanceCapacity: none":  ErrCategoryUnavailable,
		"operation error RDS: X, api error ReservedDBInstanceQuotaExceeded: no": ErrCategoryLimit,
		"operation error EC2: X, api error SomethingOdd: boom":                  ErrCategoryUnknown,
	}
	for in, cat := range cases {
		assert.Equal(t, ErrCategoryMessage(cat), SanitizeStoredError(in), in)
	}
}

func TestSanitizeStoredError_PartialRun(t *testing.T) {
	in := partialRunPrefix + "rec a: " + fakeAWS403 + "; rec b failed locally; " + "api error LimitExceeded: x arn:aws:iam::123456789012:role/r"
	out := SanitizeStoredError(in)
	assert.True(t, strings.HasPrefix(out, partialRunPrefix))
	assert.Equal(t, partialRunPrefix+ErrCategoryMessage(ErrCategoryAccessDenied), out)
	assert.NotContains(t, out, "123456789012")
}

func TestSummarizeProviderError_Typed(t *testing.T) {
	cases := []struct {
		err error
		cat string
	}{
		{&smithy.GenericAPIError{Code: "UnauthorizedOperation"}, ErrCategoryAccessDenied},
		{&smithy.GenericAPIError{Code: "AccessDenied"}, ErrCategoryAccessDenied},
		{&smithy.GenericAPIError{Code: "ReservedInstancesLimitExceeded"}, ErrCategoryLimit},
		{&smithy.GenericAPIError{Code: "InsufficientInstanceCapacity"}, ErrCategoryUnavailable},
		{&azcore.ResponseError{ErrorCode: "AuthorizationFailed", StatusCode: 403}, ErrCategoryAccessDenied},
		{&googleapi.Error{Code: 404}, ErrCategoryUnavailable},
		{fmt.Errorf("wrapped: %w", &googleapi.Error{Code: 403}), ErrCategoryAccessDenied},
		{errors.New("boom"), ErrCategoryUnknown},
	}
	for _, c := range cases {
		cat, msg := SummarizeProviderError(c.err)
		assert.Equal(t, c.cat, cat, c.err.Error())
		assert.Equal(t, ErrCategoryMessage(c.cat), msg)
		assert.NotEmpty(t, msg)
	}
}
