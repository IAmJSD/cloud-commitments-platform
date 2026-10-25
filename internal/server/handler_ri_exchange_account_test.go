package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/LeanerCloud/cloud-commitments-go/providers/aws/recommendations"
	ec2svc "github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/ec2"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/testutil"
)

// fakeCallerIdentity is a canned STS GetCallerIdentity response.
type fakeCallerIdentity struct {
	out *sts.GetCallerIdentityOutput
	err error
}

func (f fakeCallerIdentity) GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	return f.out, f.err
}

// resolveAccountID must fail instead of inventing an "unknown" account ID,
// which used to be written onto every RI exchange audit row the run created
// (#326, A06-025).
func TestResolveAccountID_RefusesToFabricateAnAccount(t *testing.T) {
	cases := map[string]fakeCallerIdentity{
		"STS error":     {err: errors.New("ExpiredToken: the security token included in the request is expired")},
		"nil output":    {},
		"nil account":   {out: &sts.GetCallerIdentityOutput{}},
		"empty account": {out: &sts.GetCallerIdentityOutput{Account: aws.String("")}},
	}
	for name, fake := range cases {
		t.Run(name, func(t *testing.T) {
			accountID, err := resolveAccountID(context.Background(), fake)

			testutil.AssertError(t, err)
			testutil.AssertEqual(t, "", accountID)
		})
	}
}

func TestResolveAccountID_ReturnsTheCallerAccount(t *testing.T) {
	fake := fakeCallerIdentity{out: &sts.GetCallerIdentityOutput{Account: aws.String("123456789012")}}

	accountID, err := resolveAccountID(context.Background(), fake)

	testutil.AssertNoError(t, err)
	testutil.AssertEqual(t, "123456789012", accountID)
}

// A reshape run without a resolved account ID must stop before it lists RIs
// or writes any exchange record, so no audit row carries a blank account.
func TestExecuteRIExchangeReshape_RefusesEmptyAccountID(t *testing.T) {
	ctx := testutil.TestContext(t)
	writes := 0
	store := &mockConfigStoreForExchange{
		saveRIExchangeRecordFunc: func(context.Context, *config.RIExchangeRecord) error {
			writes++
			return nil
		},
		reserveRIExchangeFunc: func(context.Context, *config.RIExchangeRecord, string, string) (string, error) {
			writes++
			return "", errors.New("unexpected reservation")
		},
	}
	app := &Application{Config: store}
	cfg := &config.GlobalConfig{
		RIExchangeEnabled:              true,
		RIExchangeMode:                 "auto",
		RIExchangeUtilizationThreshold: 95.0,
		RIExchangeLookbackDays:         30,
	}

	listed := false
	clients := riExchangeClients{
		listConvertibleRIs: func(context.Context) ([]ec2svc.ConvertibleRI, error) {
			listed = true
			return nil, nil
		},
		getRIUtilization: func(context.Context, int) ([]recommendations.RIUtilization, error) {
			return nil, nil
		},
		exchangeClient: &mockExchangeClient{},
		accountID:      "",
		region:         "us-east-1",
	}

	result, err := app.executeRIExchangeReshape(ctx, cfg, clients)

	testutil.AssertError(t, err)
	testutil.AssertTrue(t, strings.Contains(err.Error(), "account ID"), "expected an account ID error, got: "+err.Error())
	testutil.AssertTrue(t, result == nil, "expected no result")
	testutil.AssertTrue(t, !listed, "convertible RIs must not be listed without an account ID")
	testutil.AssertEqual(t, 0, writes)
}
