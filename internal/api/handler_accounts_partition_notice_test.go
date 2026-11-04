package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #1194: aws-cn accounts are onboarded (blocking them could strand
// existing ones), but every account response carries a visible
// unsupported_partition notice so the UI can tell the operator CUDly cannot
// buy Savings Plans for them.

const (
	cnRoleARN         = "arn:aws-cn:iam::123456789012:role/CUDly"
	commercialRoleARN = "arn:aws:iam::123456789012:role/CUDly"
	partitionTestID   = "11111111-1111-1111-1111-111111111111"
)

func requireUnsupportedPartitionNotice(t *testing.T, acct *config.CloudAccount) {
	t.Helper()
	require.Len(t, acct.Notices, 1)
	assert.Equal(t, config.AccountNoticeUnsupportedPartition, acct.Notices[0].Code)
	assert.Contains(t, acct.Notices[0].Message, "aws-cn")
	assert.Contains(t, acct.Notices[0].Message, "Savings Plans")

	raw, err := json.Marshal(acct)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"notices":[{"code":"unsupported_partition"`)
}

func requireNoNotices(t *testing.T, acct *config.CloudAccount) {
	t.Helper()
	assert.Empty(t, acct.Notices)
	raw, err := json.Marshal(acct)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"notices"`)
}

func roleARNAccountBody(roleARN string) string {
	return `{"name":"Acme","provider":"aws","external_id":"123456789012",` +
		`"aws_auth_mode":"role_arn","aws_role_arn":"` + roleARN + `",` +
		`"aws_external_id":"550e8400-e29b-41d4-a716-446655440000"}`
}

func TestCreateAccount_AWSChinaRoleARN_ReturnsUnsupportedPartitionNotice(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	setupAdminAuth(ctx, mockAuth)
	handler := &Handler{auth: mockAuth, config: setupAdminMock(ctx)}

	result, err := handler.createAccount(ctx, adminRequest(roleARNAccountBody(cnRoleARN)))
	require.NoError(t, err, "aws-cn accounts are still onboarded")
	requireUnsupportedPartitionNotice(t, result.(*config.CloudAccount))
}

func TestCreateAccount_CommercialRoleARN_HasNoNotice(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	setupAdminAuth(ctx, mockAuth)
	handler := &Handler{auth: mockAuth, config: setupAdminMock(ctx)}

	result, err := handler.createAccount(ctx, adminRequest(roleARNAccountBody(commercialRoleARN)))
	require.NoError(t, err)
	requireNoNotices(t, result.(*config.CloudAccount))
}

func TestUpdateAccount_AWSChinaRoleARN_ReturnsUnsupportedPartitionNotice(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	setupAdminAuth(ctx, mockAuth)
	existing := sampleAccount()
	handler := &Handler{auth: mockAuth, config: &mockConfigStoreAccounts{MockConfigStore: setupAdminMock(ctx), getResult: &existing}}

	result, err := handler.updateAccount(ctx, adminRequest(roleARNAccountBody(cnRoleARN)), partitionTestID)
	require.NoError(t, err)
	requireUnsupportedPartitionNotice(t, result.(*config.CloudAccount))
}

func TestGetAccount_AWSChinaRoleARN_ReturnsUnsupportedPartitionNotice(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	setupAdminAuth(ctx, mockAuth)
	acct := sampleAccount()
	acct.AWSAuthMode = "role_arn"
	acct.AWSRoleARN = cnRoleARN
	handler := &Handler{auth: mockAuth, config: &mockConfigStoreAccounts{MockConfigStore: setupAdminMock(ctx), getResult: &acct}}

	result, err := handler.getAccount(ctx, adminRequest(""), partitionTestID)
	require.NoError(t, err)
	requireUnsupportedPartitionNotice(t, result.(*config.CloudAccount))
}

// TestListAccounts_PartitionNotices covers the list view, including the
// self-account, which has no role ARN and takes CUDly's own partition.
func TestListAccounts_PartitionNotices(t *testing.T) {
	cases := []struct {
		name          string
		hostPartition string
		wantSelf      bool
	}{
		{"CUDly hosted in aws-cn flags its self-account", config.AWSPartitionChina, true},
		{"CUDly hosted in aws leaves its self-account alone", config.AWSPartitionStandard, false},
		{"unknown host partition leaves the self-account alone", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			mockAuth := new(MockAuthService)
			t.Cleanup(func() { mockAuth.AssertExpectations(t) })
			setupAdminAuth(ctx, mockAuth)

			cn := sampleAccount()
			cn.ID, cn.Name, cn.AWSAuthMode, cn.AWSRoleARN = "cn", "China", "role_arn", cnRoleARN
			commercial := sampleAccount()
			commercial.ID, commercial.Name, commercial.AWSAuthMode, commercial.AWSRoleARN = "com", "Commercial", "role_arn", commercialRoleARN
			self := sampleAccount()
			self.ID, self.Name, self.ExternalID, self.AWSAuthMode = "self", "CUDly host", "111111111111", "role_arn"
			azure := config.CloudAccount{ID: "az", Name: "Azure", Provider: "azure", ExternalID: "sub-1"}

			handler := &Handler{auth: mockAuth, config: &mockConfigStoreAccounts{
				MockConfigStore: setupAdminMock(ctx),
				listResult:      []config.CloudAccount{cn, commercial, self, azure},
			}}
			handler.sourceIdentityOnce.Do(func() {
				handler.sourceID = &sourceIdentity{
					Provider:  "aws",
					AccountID: "111111111111",
					Partition: tc.hostPartition,
				}
			})

			result, err := handler.listAccounts(ctx, adminRequest(""))
			require.NoError(t, err)
			got := result.([]config.CloudAccount)
			require.Len(t, got, 4)

			requireUnsupportedPartitionNotice(t, &got[0])
			requireNoNotices(t, &got[1])
			require.True(t, got[2].IsSelf)
			if tc.wantSelf {
				requireUnsupportedPartitionNotice(t, &got[2])
			} else {
				requireNoNotices(t, &got[2])
			}
			requireNoNotices(t, &got[3])
		})
	}
}
