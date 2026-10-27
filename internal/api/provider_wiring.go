package api

// provider_wiring.go is the composition file for the concrete
// cloud-commitments-go provider packages the RI exchange and Marketplace
// handlers use (platform#41). It is the only non-test file in this package
// that imports them: the handlers reach AWS and Azure through the narrow
// client interfaces they declare (reshapeEC2Client, reshapeRecsClient,
// targetOfferingsEC2Client, azureExchangeClient, marketplaceEC2Client), built
// by the constructors below, and name the provider data types through the
// aliases below. provider_wiring_imports_test.go enforces this.
//
// The aliases are Go type aliases, not new types, so values, JSON encoding and
// the tests that build the provider types directly are unchanged.

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/reservations/armreservations"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	awsprovider "github.com/LeanerCloud/cloud-commitments-go/providers/aws"
	"github.com/LeanerCloud/cloud-commitments-go/providers/aws/recommendations"
	ec2svc "github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/ec2"
	azurecompute "github.com/LeanerCloud/cloud-commitments-go/providers/azure/services/compute"
)

// Provider data types: AWS EC2 and Cost Explorer, Azure compute exchange and
// the Azure reservation term enum.
type (
	ec2ConvertibleRI             = ec2svc.ConvertibleRI
	ec2TargetOffering            = ec2svc.TargetOffering
	ec2ListTargetOfferingsParams = ec2svc.ListTargetOfferingsParams
	ec2MarketplaceListingRequest = ec2svc.MarketplaceListingRequest
	ec2MarketplaceListingResult  = ec2svc.MarketplaceListingResult
	ec2MarketplacePriceTier      = ec2svc.MarketplacePriceTier
	riUtilization                = recommendations.RIUtilization
	azureExchangeableReservation = azurecompute.ExchangeableReservation
	azureExchangeTarget          = azurecompute.ExchangeTarget
	azureExchangePreview         = azurecompute.ExchangePreview
	azureExchangeResult          = azurecompute.ExchangeResult
	azureCompatibleOffering      = azurecompute.CompatibleOffering
	azureReservationTerm         = armreservations.ReservationTerm
)

// errAzureUnsupportedAppliedScope is azurecompute.ErrUnsupportedAppliedScope:
// CalculateExchange refuses a source whose applied scope cannot carry over.
var errAzureUnsupportedAppliedScope = azurecompute.ErrUnsupportedAppliedScope

// loadDefaultAWSConfig loads the ambient AWS SDK config (env, shared files,
// instance or task role).
func loadDefaultAWSConfig(ctx context.Context) (aws.Config, error) {
	return awsconfig.LoadDefaultConfig(ctx)
}

// newReshapeEC2Client builds the EC2 client that lists convertible RIs.
func newReshapeEC2Client(cfg aws.Config) reshapeEC2Client {
	return awsprovider.NewEC2ClientDirect(cfg)
}

// newReshapeRecsClient builds the Cost Explorer recommendations adapter.
func newReshapeRecsClient(cfg aws.Config) reshapeRecsClient {
	return awsprovider.NewRecommendationsClientDirect(cfg)
}

// newTargetOfferingsEC2Client builds the EC2 client that lists convertible RIs
// and their exchange target offerings.
func newTargetOfferingsEC2Client(cfg aws.Config) targetOfferingsEC2Client {
	return awsprovider.NewEC2ClientDirect(cfg)
}

// newMarketplaceEC2Client builds the EC2 client for RI Marketplace listings.
func newMarketplaceEC2Client(cfg aws.Config) marketplaceEC2Client {
	return awsprovider.NewEC2ClientDirect(cfg)
}

// newAzureExchangeClient builds the Azure compute client for the exchange
// flow. Region is left empty: ListExchangeableReservations uses the
// tenant-wide armreservations API, which is not scoped to a region.
func newAzureExchangeClient(cred azcore.TokenCredential, subscriptionID string) azureExchangeClient {
	return azurecompute.NewClient(cred, subscriptionID, "")
}

// azureReservationTermFromString converts the HTTP-layer term string to the
// typed SDK enum, rejecting anything outside armreservations'
// PossibleReservationTermValues(). No fallback: an unrecognized term is a
// 400, never silently coerced to a default term (feedback_sdk_enum_string_literals).
func azureReservationTermFromString(s string) (azureReservationTerm, error) {
	term := armreservations.ReservationTerm(s)
	for _, t := range armreservations.PossibleReservationTermValues() {
		if t == term {
			return term, nil
		}
	}
	return "", fmt.Errorf("unsupported term %q", s)
}
