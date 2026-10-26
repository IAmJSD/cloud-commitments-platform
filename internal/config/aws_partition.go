package config

import "strings"

// AWS partition names as they appear in the second segment of an ARN.
const (
	AWSPartitionStandard = "aws"
	AWSPartitionChina    = "aws-cn"
	AWSPartitionGovCloud = "aws-us-gov"
)

// AccountNoticeUnsupportedPartition is the AccountNotice code for an AWS
// account in a partition where CUDly cannot buy Savings Plans (issue #1194).
const AccountNoticeUnsupportedPartition = "unsupported_partition"

// awsChinaNoticeMessage is the user-facing text of the aws-cn notice.
const awsChinaNoticeMessage = "This is an AWS China (aws-cn) account. CUDly cannot buy Savings Plans " +
	"for it: AWS bills aws-cn Savings Plans in CNY and CUDly's purchase path is USD-only, so " +
	"Savings Plans purchases for this account are refused. Recommendations may also be " +
	"unavailable, because CUDly reaches Cost Explorer through us-east-1."

// AccountNotice is a derived, non-persisted warning about a cloud account,
// returned with the account so the UI can show it.
type AccountNotice struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// AWSPartitionFromARN returns the partition segment of an AWS ARN
// (arn:<partition>:<service>:...). It returns "" for inputs that are not
// ARNs or that name a partition other than aws, aws-cn and aws-us-gov.
func AWSPartitionFromARN(arn string) string {
	rest, ok := strings.CutPrefix(arn, "arn:")
	if !ok {
		return ""
	}
	partition, _, ok := strings.Cut(rest, ":")
	if !ok {
		return ""
	}
	switch partition {
	case AWSPartitionStandard, AWSPartitionChina, AWSPartitionGovCloud:
		return partition
	default:
		return ""
	}
}

// AWSPartition returns the AWS partition of the account, taken from its role
// ARN. It returns "" for non-AWS accounts and for AWS accounts with no role
// ARN (access_keys mode, or a self-account on ambient credentials), whose
// partition is not known without calling AWS.
func (a *CloudAccount) AWSPartition() string {
	if a == nil || a.Provider != "aws" {
		return ""
	}
	return AWSPartitionFromARN(strings.TrimSpace(a.AWSRoleARN))
}

// AWSPartitionNotices returns the notices for an AWS account in the given
// partition, or nil when the partition needs none.
func AWSPartitionNotices(partition string) []AccountNotice {
	if partition != AWSPartitionChina {
		return nil
	}
	return []AccountNotice{{Code: AccountNoticeUnsupportedPartition, Message: awsChinaNoticeMessage}}
}
