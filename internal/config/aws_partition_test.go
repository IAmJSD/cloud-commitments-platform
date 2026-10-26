package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCloudAccountAWSPartition(t *testing.T) {
	cases := []struct {
		name    string
		account *CloudAccount
		want    string
	}{
		{"nil account", nil, ""},
		{"aws-cn role ARN", &CloudAccount{Provider: "aws", AWSRoleARN: "arn:aws-cn:iam::123456789012:role/CUDly"}, AWSPartitionChina},
		{"padded aws-cn role ARN", &CloudAccount{Provider: "aws", AWSRoleARN: "  arn:aws-cn:iam::123456789012:role/CUDly "}, AWSPartitionChina},
		{"GovCloud role ARN", &CloudAccount{Provider: "aws", AWSRoleARN: "arn:aws-us-gov:iam::123456789012:role/CUDly"}, AWSPartitionGovCloud},
		{"commercial role ARN", &CloudAccount{Provider: "aws", AWSRoleARN: "arn:aws:iam::123456789012:role/CUDly"}, AWSPartitionStandard},
		{"no role ARN", &CloudAccount{Provider: "aws", AWSAuthMode: "access_keys"}, ""},
		{"unknown partition", &CloudAccount{Provider: "aws", AWSRoleARN: "arn:aws-iso:iam::123456789012:role/CUDly"}, ""},
		{"non-AWS account ignores the ARN field", &CloudAccount{Provider: "azure", AWSRoleARN: "arn:aws-cn:iam::123456789012:role/CUDly"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.account.AWSPartition())
		})
	}
}

func TestAWSPartitionNotices(t *testing.T) {
	notices := AWSPartitionNotices(AWSPartitionChina)
	if assert.Len(t, notices, 1) {
		assert.Equal(t, AccountNoticeUnsupportedPartition, notices[0].Code)
		assert.Contains(t, notices[0].Message, "Savings Plans")
	}
	for _, p := range []string{"", AWSPartitionStandard, AWSPartitionGovCloud} {
		assert.Nil(t, AWSPartitionNotices(p), "partition %q", p)
	}
}
