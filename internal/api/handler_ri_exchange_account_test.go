package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	ec2svc "github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/ec2"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testRunningAccount is the placeholder STS account the stub resolver returns.
const testRunningAccount = "123456789012"

// Issue #1692: the convertible-RI list names the AWS account each RI belongs
// to, so the RI Exchange page can show it before an exchange.

func listRIsHandler(resolve func(context.Context) (string, error), ec2 *countingUtilEC2) *Handler {
	h := &Handler{
		auth:                       &mockAuthForExchange{},
		riInstancesAccountResolver: resolve,
		reshapeEC2Factory:          func(_ aws.Config) reshapeEC2Client { return ec2 },
	}
	// Pre-seal awsCfgOnce so the handler never runs LoadDefaultConfig.
	h.awsCfgOnce.Do(func() { h.awsCfg = aws.Config{Region: "us-east-1"} })
	return h
}

func listRIs(t *testing.T, h *Handler, params map[string]string) *ConvertibleRIsResponse {
	t.Helper()
	resp, err := h.listConvertibleRIs(context.Background(), &events.LambdaFunctionURLRequest{
		Headers:               map[string]string{"authorization": "Bearer test-token"},
		QueryStringParameters: params,
	})
	require.NoError(t, err)
	typed, ok := resp.(*ConvertibleRIsResponse)
	require.True(t, ok, "expected *ConvertibleRIsResponse, got %T", resp)
	return typed
}

func TestListConvertibleRIs_LabelsEachRIWithTheRunningAccount(t *testing.T) {
	ec2 := &countingUtilEC2{instances: []ec2svc.ConvertibleRI{
		{ReservedInstanceID: "ri-a", InstanceType: "t4g.nano"},
		{ReservedInstanceID: "ri-b", InstanceType: "m5.large"},
	}}
	h := listRIsHandler(func(context.Context) (string, error) { return testRunningAccount, nil }, ec2)

	resp := listRIs(t, h, nil)

	require.Len(t, resp.Instances, 2)
	for _, ri := range resp.Instances {
		assert.Equal(t, testRunningAccount, ri.AccountID)
	}
	// The embedded RI fields stay at the top level of each JSON object.
	raw, err := json.Marshal(resp)
	require.NoError(t, err)
	var decoded struct {
		Instances []map[string]any `json:"instances"`
	}
	require.NoError(t, json.Unmarshal(raw, &decoded))
	assert.Equal(t, "ri-a", decoded.Instances[0]["reserved_instance_id"])
	assert.Equal(t, "t4g.nano", decoded.Instances[0]["instance_type"])
	assert.Equal(t, testRunningAccount, decoded.Instances[0]["account_id"])
}

func TestListConvertibleRIs_MatchingAccountFilterKeepsTheLabel(t *testing.T) {
	ec2 := &countingUtilEC2{instances: []ec2svc.ConvertibleRI{{ReservedInstanceID: "ri-a"}}}
	h := listRIsHandler(func(context.Context) (string, error) { return testRunningAccount, nil }, ec2)

	resp := listRIs(t, h, map[string]string{"account_id": testRunningAccount})

	require.Len(t, resp.Instances, 1)
	assert.Equal(t, testRunningAccount, resp.Instances[0].AccountID)
}

// Without an account filter the account only labels the rows, so an STS
// failure must not hide the RIs; the label is left out instead.
func TestListConvertibleRIs_UnresolvedAccountWithoutFilterStillLists(t *testing.T) {
	ec2 := &countingUtilEC2{instances: []ec2svc.ConvertibleRI{{ReservedInstanceID: "ri-a"}}}
	h := listRIsHandler(func(context.Context) (string, error) { return "", errors.New("sts denied") }, ec2)

	resp := listRIs(t, h, nil)

	require.Len(t, resp.Instances, 1)
	assert.Empty(t, resp.Instances[0].AccountID)
	raw, err := json.Marshal(resp.Instances[0])
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "account_id")
	assert.Equal(t, 1, ec2.calls)
}
