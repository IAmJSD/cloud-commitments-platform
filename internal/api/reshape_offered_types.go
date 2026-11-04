package api

import (
	"context"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
)

// OfferedInstanceTypes returns the EC2 instance types offered in region, for
// exchange.WithOfferedInstanceTypes and RunAutoExchangeParams.OfferedInstanceTypes.
// fetch is normally ec2svc.Client.GetValidResourceTypes
// (DescribeInstanceTypeOfferings).
//
// On a fetch error or an empty catalog it logs a warning naming the region and
// returns nil, so reshape analysis proceeds on the legacy size ladder instead of
// failing the request. Nothing is cached, so the next call queries the catalog
// again rather than reusing the fallback.
func OfferedInstanceTypes(ctx context.Context, region string, fetch func(context.Context) ([]string, error)) []string {
	types, err := fetch(ctx)
	if err != nil {
		logging.Warnf("reshape: EC2 instance type offerings for region %q failed, proceeding without the catalog: %v", region, err)
		return nil
	}
	if len(types) == 0 {
		logging.Warnf("reshape: EC2 instance type offerings for region %q came back empty, proceeding without the catalog", region)
		return nil
	}
	return types
}
