package api

// handler_marketplace.go implements the Sell-on-Marketplace flow for Standard
// Reserved Instances (issue #292).
//
// Endpoints:
//   POST /api/purchases/{id}/marketplace-list    create a listing
//   POST /api/purchases/{id}/marketplace-cancel  cancel an active listing
//
// Both endpoints require the caller to hold sell-any:purchases (admin or a
// custom operator group) or sell-own:purchases for rows they purchased
// themselves. The purchase_id in the URL path is the AWS ReservedInstancesId
// stamped into purchase_history.purchase_id at completion.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	awsprovider "github.com/LeanerCloud/cloud-commitments-go/providers/aws"
	ec2svc "github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/ec2"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/credentials"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	smithy "github.com/aws/smithy-go"
)

// marketplaceEC2Client is the narrow EC2 interface the marketplace handlers
// need. Using a minimal interface keeps test stubs small.
type marketplaceEC2Client interface {
	CreateMarketplaceListing(ctx context.Context, req ec2svc.MarketplaceListingRequest) (ec2svc.MarketplaceListingResult, error)
	DescribeMarketplaceListing(ctx context.Context, listingID string) (ec2svc.MarketplaceListingResult, error)
	CancelMarketplaceListing(ctx context.Context, listingID string) (ec2svc.MarketplaceListingResult, error)
	// FetchOfferingClass calls AWS DescribeReservedInstances to determine
	// whether a given Reserved Instance is 'standard' or 'convertible'. Used
	// when the purchase_history row has no offering_class stored (pre-migration
	// 000087 rows and externally-created Standard RIs).
	FetchOfferingClass(ctx context.Context, reservedInstancesID string) (string, error)
}

// buildMarketplaceEC2Client honors the injected factory for tests, falling
// back to the direct AWS SDK constructor in production.
func (h *Handler) buildMarketplaceEC2Client(cfg aws.Config) marketplaceEC2Client {
	if h.marketplaceEC2Factory != nil {
		return h.marketplaceEC2Factory(cfg)
	}
	return awsprovider.NewEC2ClientDirect(cfg)
}

// loadMarketplaceAWSConfig returns an AWS config for the purchase's region that
// signs requests as the purchase's own cloud account, resolved through the same
// credential resolver the purchase path uses. A role_arn account without a role
// ARN is accepted only when it is the host account (credentials.ErrNotHostAccount
// otherwise). Rows without a cloud account (reachable only by sell-any callers
// with unrestricted scope) get the host's own credentials only when the row's
// AWS account id is confirmed as the host account; see
// loadMarketplaceHostAWSConfig.
func (h *Handler) loadMarketplaceAWSConfig(ctx context.Context, row *config.PurchaseHistoryRecord) (aws.Config, error) {
	base, err := h.loadAWSConfigWithRegion(ctx, row.Region)
	if err != nil {
		return aws.Config{}, fmt.Errorf("failed to load AWS config: %w", err)
	}
	if row.CloudAccountID == nil || strings.TrimSpace(*row.CloudAccountID) == "" {
		return loadMarketplaceHostAWSConfig(ctx, row, base)
	}
	account, err := h.config.GetCloudAccount(ctx, *row.CloudAccountID)
	if err != nil {
		return aws.Config{}, fmt.Errorf("failed to look up the purchase's cloud account: %w", err)
	}
	if account == nil {
		return aws.Config{}, NewClientError(400, "the purchase's cloud account no longer exists")
	}
	if account.Provider != "aws" {
		return aws.Config{}, NewClientError(400, "the purchase's cloud account is not an AWS account")
	}
	stsClient := sts.NewFromConfig(base)
	provider, err := credentials.ResolveAWSCredentialProviderWithOpts(ctx, account, h.credStore, stsClient,
		credentials.AWSResolveOptions{AmbientProvider: base.Credentials, AmbientSTS: stsClient})
	if err != nil {
		if errors.Is(err, credentials.ErrNotHostAccount) {
			return aws.Config{}, NewClientError(400, err.Error())
		}
		return aws.Config{}, fmt.Errorf("failed to resolve AWS credentials for the purchase's account: %w", err)
	}
	cfg := base.Copy()
	cfg.Credentials = provider
	return cfg, nil
}

// marketplaceNoCloudAccountMessage is the tenant-visible refusal for a row with
// no cloud account whose AWS account is not the host account. Like
// credentials.ErrNotHostAccount it never carries the host account id.
const marketplaceNoCloudAccountMessage = "this purchase is not linked to a cloud account and its AWS account is not confirmed as the CUDly host account; it cannot be listed or canceled from here"

// loadMarketplaceHostAWSConfig handles a row with no cloud account: the
// ambient single-account deployment (purchase.SingleCloudAccountIDFromRecs
// returns nil) and legacy or account-deleted rows (cloud_account_id has no
// backfill and is ON DELETE SET NULL). The host's credentials can only act on
// the host account, so they are used only when the row's AWS account id
// passes the same host identity check the resolver applies to a role_arn
// account with no role ARN. Anything else, including an empty account id or a
// failed STS lookup, is refused before any EC2 client is built.
func loadMarketplaceHostAWSConfig(ctx context.Context, row *config.PurchaseHistoryRecord, base aws.Config) (aws.Config, error) {
	accountID := strings.TrimSpace(row.AccountID)
	if accountID == "" {
		logging.Warnf("marketplace: purchase %s has neither a cloud account nor an AWS account id; refusing to sign with host credentials", row.PurchaseID)
		return aws.Config{}, NewClientError(400, marketplaceNoCloudAccountMessage)
	}
	hostCheck := &config.CloudAccount{ID: "purchase " + row.PurchaseID, Provider: "aws", ExternalID: accountID}
	if err := credentials.VerifyHostAccount(ctx, hostCheck, sts.NewFromConfig(base)); err != nil {
		return aws.Config{}, NewClientError(400, marketplaceNoCloudAccountMessage)
	}
	return base, nil
}

// MarketplacePriceTier is the JSON-decodable shape accepted in the request body.
type MarketplacePriceTier struct {
	// TermMonths is the remaining-months count this tier covers.
	TermMonths int64 `json:"term_months"`
	// Price is the USD list price per unit for this tier.
	Price float64 `json:"price"`
}

// MarketplaceListRequest is the request body for POST .../marketplace-list.
// PriceSchedule is optional: when absent the handler computes a default from
// the row's upfront_cost, monthly_cost, term, and a 5% discount to attract
// buyers (documented in the response body so the caller can see what was used).
type MarketplaceListRequest struct {
	PriceSchedule []MarketplacePriceTier `json:"price_schedule,omitempty"`
}

// MarketplaceListResponse is the JSON response body for a successful listing.
type MarketplaceListResponse struct {
	ListingID     string                 `json:"listing_id"`
	ListingState  string                 `json:"listing_state"`
	PriceSchedule []MarketplacePriceTier `json:"price_schedule"`
	AWSFeePercent float64                `json:"aws_fee_percent"`
	Note          string                 `json:"note,omitempty"`
}

// validateMarketplaceListRequest performs the pre-flight checks shared by the
// listing flow: session auth, UUID validation, row lookup, offering_class /
// RBAC / duplicate-listing gates, and optional body decode. It returns the
// validated history row and the decoded request body. Extracted from
// marketplaceList to keep that handler's cyclomatic complexity in check.
func (h *Handler) validateMarketplaceListRequest(ctx context.Context, req *events.LambdaFunctionURLRequest, purchaseID string) (*config.PurchaseHistoryRecord, MarketplaceListRequest, error) {
	var body MarketplaceListRequest

	session, action, err := h.requireSessionPurchaseAction(ctx, req, auth.ActionSellAny, auth.ActionSellOwn)
	if err != nil {
		return nil, body, err
	}

	err = validateUUID(purchaseID)
	if err != nil {
		return nil, body, err
	}

	// Look up the purchase_history row to validate offering_class + get metadata.
	row, err := h.config.GetPurchaseHistoryByPurchaseID(ctx, purchaseID)
	if err != nil {
		return nil, body, fmt.Errorf("failed to look up purchase: %w", err)
	}
	if row == nil {
		return nil, body, NewClientError(404, "purchase not found")
	}

	// Reject explicitly non-standard classes before the RBAC call (fast path).
	// Empty offering_class is allowed here: the listing handler populates it
	// lazily from AWS before performing the definitive check.
	if isKnownNonStandardOfferingClass(row.OfferingClass) {
		return nil, body, NewClientError(400, "only Standard Reserved Instances can be listed on the AWS Marketplace; this purchase has offering_class="+row.OfferingClass)
	}

	// Enforce sell-any / sell-own RBAC.
	if err := h.authorizeSessionSell(ctx, session, action, row); err != nil {
		return nil, body, err
	}

	// Reject if a listing is already active to avoid duplicate listings.
	if strings.EqualFold(row.ListingState, config.ListingStateActive) {
		return nil, body, NewClientError(409, fmt.Sprintf("an active marketplace listing %s already exists for this RI; cancel it first", row.ListingID))
	}

	// Decode optional body.
	if req.Body != "" {
		if err := json.Unmarshal([]byte(req.Body), &body); err != nil {
			return nil, body, NewClientError(400, "invalid request body: "+err.Error())
		}
	}

	return row, body, nil
}

// marketplaceList handles POST /api/purchases/{id}/marketplace-list.
// The {id} must be the purchase_history.purchase_id (AWS ReservedInstancesId).
func (h *Handler) marketplaceList(ctx context.Context, req *events.LambdaFunctionURLRequest, purchaseID string) (any, error) {
	row, body, err := h.validateMarketplaceListRequest(ctx, req, purchaseID)
	if err != nil {
		return nil, err
	}

	// purchase_history.term is stored in years (1 or 3); both
	// computeRemainingMonths and resolveMarketplacePriceSchedule operate in
	// months. computeRemainingMonths rejects a non-positive term and a missing
	// timestamp rather than pricing garbage on this money path.
	termMonths := row.Term * 12
	remainingMonths, err := computeRemainingMonths(row.Timestamp, termMonths, time.Now())
	if err != nil {
		return nil, err
	}

	// Validate and normalise the price schedule. Row-total UpfrontCost and the
	// instance Count are passed so pricing is computed per instance; recurring
	// (monthly) cost is deliberately excluded because the Marketplace buyer
	// assumes recurring charges post-transfer (issue #292 money-path review).
	schedule, err := resolveMarketplacePriceSchedule(body.PriceSchedule, remainingMonths, termMonths, row.Count, row.UpfrontCost)
	if err != nil {
		return nil, NewClientError(400, err.Error())
	}

	// Build per-provider AWS config for the region the RI lives in.
	cfg, err := h.loadMarketplaceAWSConfig(ctx, row)
	if err != nil {
		return nil, err
	}

	ec2Client := h.buildMarketplaceEC2Client(cfg)

	// Populate offering_class from AWS when the DB row has none. This covers
	// two cases: (1) pre-migration 000087 rows purchased by CUDly before this
	// column existed, and (2) externally-created Standard RIs whose
	// offering_class was never stamped. After population the value is persisted
	// so subsequent requests do not require an extra AWS API call.
	offeringClass := row.OfferingClass
	if offeringClass == "" {
		offeringClass, err = h.populateOfferingClass(ctx, purchaseID, ec2Client)
		if err != nil {
			return nil, err
		}
	}
	// Definitive offering_class gate: only Standard RIs can be listed.
	if !strings.EqualFold(offeringClass, string(ec2types.OfferingClassTypeStandard)) {
		return nil, NewClientError(400, "only Standard Reserved Instances can be listed on the AWS Marketplace; this purchase has offering_class="+offeringClass)
	}

	result, usedSchedule, err := h.reserveAndCreateListing(ctx, purchaseID, row, cfg, ec2Client, schedule)
	if err != nil {
		return nil, err
	}

	return &MarketplaceListResponse{
		ListingID:     result.ListingID,
		ListingState:  result.State,
		PriceSchedule: usedSchedule,
		AWSFeePercent: awsMarketplaceFeePercent,
		Note:          fmt.Sprintf("AWS charges a %d%% transaction fee on the listing proceeds. Net proceeds = ListingPrice * %.2f.", awsMarketplaceFeePercent, awsMarketplaceNetFactor),
	}, nil
}

// marketplaceCompensationTimeout bounds the detached cancel and recording write
// run after a created listing failed to persist.
const marketplaceCompensationTimeout = 10 * time.Second

// keepUncanceledListing records a listing that could be neither persisted nor
// canceled; releasing the claim instead would hide it from the cancel endpoint.
func (h *Handler) keepUncanceledListing(ctx context.Context, purchaseID string, listing ec2svc.MarketplaceListingResult, cancelErr error) error {
	logging.Errorf("marketplace: rollback cancel for listing %s failed: %v", listing.ListingID, cancelErr)
	state := listing.State
	if state == "" {
		state = config.ListingStatePending
	}
	cancelDetail := cancelErr.Error()
	if isRoleAssumptionFailure(cancelErr) {
		cancelDetail = marketplaceRoleAssumptionMessage
	}
	msg := fmt.Sprintf("marketplace listing %s was created but could not be saved, and canceling it failed (%s); the listing may still be active on AWS", listing.ListingID, cancelDetail)
	if err := h.config.UpdatePurchaseHistoryListing(ctx, purchaseID, listing.ListingID, state); err != nil {
		logging.Errorf("marketplace: failed to record uncanceled listing %s for purchase %s (row stays %q): %v", listing.ListingID, purchaseID, config.ListingStatePending, err)
		return NewClientError(502, msg+"; the RI stays locked as pending until an operator reconciles the listing")
	}
	if state != config.ListingStateActive {
		return NewClientError(502, fmt.Sprintf("%s; it is recorded on this RI as %s and needs reconciliation (listing %s)", msg, state, listing.ListingID))
	}
	return NewClientError(502, msg+"; it is now recorded on this RI, so cancel it again")
}

// populateOfferingClass calls AWS DescribeReservedInstances to determine the
// offering class for an RI whose purchase_history row lacks one, then persists
// the result so subsequent requests are served from the DB. Returns the class
// string on success; returns an error if AWS reports no such RI.
//
// The DB stamp is best-effort: a write failure is logged but does not block the
// listing request because the in-memory class value is authoritative for this
// request.
func (h *Handler) populateOfferingClass(ctx context.Context, purchaseID string, ec2Client marketplaceEC2Client) (string, error) {
	class, err := ec2Client.FetchOfferingClass(ctx, purchaseID)
	if err != nil {
		// Route through mapAWSMarketplaceError so an AWS client fault (e.g. an
		// InvalidReservedInstancesID.NotFound for an RI that has expired or been
		// exchanged) surfaces as a 400, not a generic 500. Server-side AWS
		// failures still map to 502.
		return "", mapAWSMarketplaceError("offering_class not set and could not fetch from AWS", err)
	}
	if stampErr := h.config.StampOfferingClass(ctx, purchaseID, class); stampErr != nil {
		logging.Errorf("marketplace: failed to persist offering_class %q for purchase %s: %v", class, purchaseID, stampErr)
	}
	return class, nil
}

// marketplaceCancel handles POST /api/purchases/{id}/marketplace-cancel.
func (h *Handler) marketplaceCancel(ctx context.Context, req *events.LambdaFunctionURLRequest, purchaseID string) (any, error) {
	session, action, err := h.requireSessionPurchaseAction(ctx, req, auth.ActionSellAny, auth.ActionSellOwn)
	if err != nil {
		return nil, err
	}

	err = validateUUID(purchaseID)
	if err != nil {
		return nil, err
	}

	row, err := h.config.GetPurchaseHistoryByPurchaseID(ctx, purchaseID)
	if err != nil {
		return nil, fmt.Errorf("failed to look up purchase: %w", err)
	}
	if row == nil {
		return nil, NewClientError(404, "purchase not found")
	}

	err = h.authorizeSessionSell(ctx, session, action, row)
	if err != nil {
		return nil, err
	}

	if !strings.EqualFold(row.ListingState, config.ListingStateActive) {
		return nil, NewClientError(409, "no active listing found for this RI; current state: "+row.ListingState)
	}

	cfg, err := h.loadMarketplaceAWSConfig(ctx, row)
	if err != nil {
		return nil, err
	}

	ec2Client := h.buildMarketplaceEC2Client(cfg)

	result, err := ec2Client.CancelMarketplaceListing(ctx, row.ListingID)
	if err != nil {
		logging.Warnf("marketplace: CancelReservedInstancesListing for listing %s failed: %v", row.ListingID, err)
		return nil, mapAWSMarketplaceError("AWS cancel listing failed", err)
	}

	// The listing is already canceled in AWS; there is no compensating rollback
	// available if the DB write fails. Return an internal error so the caller
	// knows the state is out of sync and can retry or contact an administrator.
	if dbErr := h.config.UpdatePurchaseHistoryListing(ctx, purchaseID, result.ListingID, result.State); dbErr != nil {
		logging.Errorf("marketplace: listing canceled in AWS (%s) but DB update failed: %v; state is out of sync", result.ListingID, dbErr)
		return nil, fmt.Errorf("listing canceled in AWS but could not be persisted: %w", dbErr)
	}

	return map[string]string{"listing_id": result.ListingID, "listing_state": result.State}, nil
}

// sell-own uses account access rather than creator identity for purchase-history rows.
func (h *Handler) authorizeSessionSell(ctx context.Context, session *Session, action string, row *config.PurchaseHistoryRecord) error {
	scope, err := h.getAccountScope(ctx, session)
	if err != nil {
		return err
	}
	accountID := ""
	if row.CloudAccountID != nil {
		accountID = *row.CloudAccountID
	}
	if strings.TrimSpace(accountID) == "" {
		if action == auth.ActionSellOwn || !scope.AllowsAll() {
			return NewClientError(403, "permission denied: purchase has no cloud account attribution")
		}
	} else if !scope.Allows(accountID, "") {
		return NewClientError(403, "permission denied: purchase is outside your allowed accounts")
	}
	return h.requirePermissionConstraints(ctx, session, action, auth.ResourcePurchases, []auth.PermissionConstraints{{
		StrictScope: true, AccountIDs: knownScopeValue(accountID), Providers: knownScopeValue(row.Provider),
		Services: knownScopeValue(row.Service), Regions: knownScopeValue(row.Region),
	}})
}

// computeRemainingMonths returns the whole months remaining on an RI at now,
// given its purchase timestamp and total term in months (12 or 36). Absent
// data, an unsupported term and less than one month remaining are rejected with
// a 422 so the caller sees why: AWS requires at least one month remaining in the
// term to list a Standard RI on the Reserved Instance Marketplace.
func computeRemainingMonths(purchaseTime time.Time, termMonths int, now time.Time) (int, error) {
	if purchaseTime.IsZero() {
		return 0, NewClientError(422, "purchase has no timestamp; cannot compute remaining term for marketplace pricing")
	}
	if termMonths != 12 && termMonths != 36 {
		return 0, NewClientError(422, fmt.Sprintf("invalid term of %d months (expected 1 or 3 years); cannot compute remaining term for marketplace pricing", termMonths))
	}
	elapsedMonths := now.Sub(purchaseTime).Hours() / (24 * 30.4375)
	remaining := int(math.Floor(float64(termMonths) - elapsedMonths))
	if remaining < 1 {
		return 0, NewClientError(422, "this Reserved Instance has less than one month remaining (or has expired) and cannot be listed on the AWS Marketplace")
	}
	return remaining, nil
}

// awsMarketplaceFeePercent is the AWS Marketplace transaction fee percentage
// charged against listing proceeds. Published at
// https://aws.amazon.com/marketplace/ri-marketplace/faq/
const awsMarketplaceFeePercent = 12

// awsMarketplaceNetFactor is the fraction of list price the seller keeps
// after the AWS Marketplace fee: 1 - awsMarketplaceFeePercent/100.
// Derived from awsMarketplaceFeePercent so the two stay in sync automatically.
const awsMarketplaceNetFactor = 1 - awsMarketplaceFeePercent/100.0

// awsMarketplaceBuyerDiscountFactor is the discount applied to the computed
// residual RI value when building the default listing price. A 5% discount
// makes the listing attractive to buyers while still recovering most of the
// seller's remaining cost basis. Applied before AWS deducts its fee.
const awsMarketplaceBuyerDiscountFactor = 0.95

// awsMarketplaceMinPriceFloorFraction is the minimum ratio of prorated
// residual value that a caller-supplied price schedule must total across its
// tiers. At 5%, a schedule totalling less than 1/20 of what the default
// schedule would offer is rejected with a 400 so a user cannot accidentally
// (or maliciously) list an RI at $0 or a nominal amount. The default schedule
// uses awsMarketplaceBuyerDiscountFactor (95%) of residual; this floor is
// intentionally far below that to give sellers flexibility while preventing
// zero-price listings.
const awsMarketplaceMinPriceFloorFraction = 0.05

// awsMarketplaceClientFaultCodes is the set of AWS error codes that represent
// client-side faults for Marketplace listing operations. These map to 4xx
// responses so the caller receives an actionable message. Server-side AWS
// errors remain 5xx.
var awsMarketplaceClientFaultCodes = map[string]bool{
	"InvalidReservedInstancesId":            true,
	"InvalidReservedInstancesId.NotFound":   true,
	"InvalidParameterValue":                 true,
	"InvalidParameter":                      true,
	"IncorrectState":                        true,
	"InvalidReservedInstancesListingId":     true,
	"ReservedInstancesListingAlreadyExists": true,
	"SellerNotRegistered":                   true,
	"AuthFailure":                           true,
	"UnauthorizedOperation":                 true,
}

const marketplaceRoleAssumptionMessage = "could not assume the role configured for this account"

// isRoleAssumptionFailure reports whether err came from the STS AssumeRole call
// that the credential provider makes lazily inside an EC2 request. Its text
// names the host account and role ARNs, so it must never reach the caller.
func isRoleAssumptionFailure(err error) bool {
	return roleAssumptionOperationError(err) != nil
}

// roleAssumptionOperationError returns the STS AssumeRole operation error in
// err's chain, or nil when there is none.
func roleAssumptionOperationError(err error) *smithy.OperationError {
	for ; err != nil; err = errors.Unwrap(err) {
		var opErr *smithy.OperationError
		if errors.As(err, &opErr) && opErr.ServiceID == "STS" && strings.HasPrefix(opErr.OperationName, "AssumeRole") {
			return opErr
		}
	}
	return nil
}

// mapAWSMarketplaceError maps an AWS SDK error to an appropriate ClientError.
// AWS client-fault errors (4xx-category codes) produce a 4xx response with the
// original AWS message so the caller gets actionable feedback. All other errors
// produce a 502 (AWS-side failure). STS AssumeRole failures are classified by
// roleAssumptionClientError and never echo the AWS text.
func mapAWSMarketplaceError(opMsg string, err error) error {
	if opErr := roleAssumptionOperationError(err); opErr != nil {
		return roleAssumptionClientError(opMsg, opErr)
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		if awsMarketplaceClientFaultCodes[apiErr.ErrorCode()] || apiErr.ErrorFault() == smithy.FaultClient {
			return NewClientError(400, apiErr.ErrorMessage())
		}
	}
	return NewClientError(502, opMsg+": "+err.Error())
}

// isKnownNonStandardOfferingClass reports true when offering_class is
// explicitly set to a non-standard value (e.g. "convertible"). An empty string
// returns false because the class is not yet known and will be resolved lazily.
func isKnownNonStandardOfferingClass(class string) bool {
	return class != "" && !strings.EqualFold(class, string(ec2types.OfferingClassTypeStandard))
}

// marketplaceResidualPerUnit returns the prorated per-instance upfront residual
// of an RI: the still-unconsumed portion of the upfront cost, divided by the
// instance count. This is the seller's recoverable basis on the RI Marketplace.
//
// Two money-path invariants (issue #292 review) are baked in here so every
// caller shares them:
//   - Per instance: purchase_history.UpfrontCost is the row total for all
//     `count` instances, but Marketplace prices are per instance, so divide by
//     count. A legacy/corrupt count <= 0 is treated as 1 (matching
//     reserveAndCreateListing's floor-at-1) so a single unit still prices.
//   - Upfront only: recurring (monthly) cost is excluded because the buyer
//     assumes recurring charges after transfer -- the seller recovers only the
//     upfront remainder. Including it would overprice partial-upfront RIs and
//     make no-upfront RIs unpriceable.
//
// Returns 0 when the term or upfront basis is non-positive (a $0 no-upfront RI
// or a fully-elapsed term), signaling callers to skip the per-unit price floor.
func marketplaceResidualPerUnit(remainingMonths, originalTerm, count int, upfrontCost float64) float64 {
	if remainingMonths <= 0 || originalTerm <= 0 || upfrontCost <= 0 {
		return 0
	}
	units := count
	if units <= 0 {
		units = 1
	}
	upfrontRemaining := upfrontCost * (float64(remainingMonths) / float64(originalTerm))
	return upfrontRemaining / float64(units)
}

// checkSuppliedScheduleFloor validates a caller-supplied price schedule against
// two money-path guardrails (issue #292):
//  1. no tier's TermMonths may exceed the RI's remaining months -- an
//     out-of-range term would inflate the tier's implied value arbitrarily; and
//  2. each tier's one-time sale Price must be at least
//     awsMarketplaceMinPriceFloorFraction of the per-unit residual prorated to
//     that tier's term.
//
// AWS's PriceScheduleSpecification.Price is the ONE-TIME sale price a buyer pays
// when TermMonths remain (NOT a per-month rate), so the floor is applied to
// Price directly -- never Price*TermMonths, which would over-credit a cheap
// schedule up to ~term-fold and let a near-zero listing slip past the floor.
// When the per-unit residual is zero (a $0 no-upfront RI) the price floor is
// skipped, but the term-range check still applies.
func checkSuppliedScheduleFloor(tiers []MarketplacePriceTier, remainingMonths, originalTerm, count int, upfrontCost float64) error {
	if remainingMonths <= 0 {
		return nil
	}
	perUnit := marketplaceResidualPerUnit(remainingMonths, originalTerm, count, upfrontCost)
	for i, t := range tiers {
		if t.TermMonths > int64(remainingMonths) {
			return fmt.Errorf(
				"price_schedule[%d]: term_months (%d) exceeds the RI's remaining term (%d months)",
				i, t.TermMonths, remainingMonths)
		}
		if perUnit <= 0 {
			continue
		}
		proratedResidual := perUnit * (float64(t.TermMonths) / float64(remainingMonths))
		floor := proratedResidual * awsMarketplaceMinPriceFloorFraction
		if t.Price < floor {
			return fmt.Errorf(
				"price_schedule[%d]: price (%.2f) is below the minimum floor (%.2f, %.0f%% of the %.2f per-unit residual for %d remaining months); raise the price or omit the schedule to use the default",
				i, t.Price, floor, awsMarketplaceMinPriceFloorFraction*100, proratedResidual, t.TermMonths)
		}
	}
	return nil
}

// resolveMarketplacePriceSchedule returns a normalised price schedule for the
// given RI. A caller-supplied schedule is validated (positive term/price, the
// term-range and per-unit price-floor guardrails in checkSuppliedScheduleFloor)
// and returned unchanged. When the caller omitted the schedule (nil / empty) a
// single-tier default is computed from the per-unit upfront residual.
//
// remainingMonths must be the actual remaining months (from computeRemainingMonths,
// NOT the raw term, which would overprice older RIs). originalTerm is the full
// contract term in months. count is the row's instance count (pricing is per
// instance). upfrontCost is the row-total upfront paid across all instances.
func resolveMarketplacePriceSchedule(supplied []MarketplacePriceTier, remainingMonths, originalTerm, count int, upfrontCost *float64) ([]MarketplacePriceTier, error) {
	if upfrontCost == nil {
		return nil, fmt.Errorf("upfront cost is unknown; cannot price this marketplace listing")
	}
	if len(supplied) > 0 {
		for i, t := range supplied {
			if t.TermMonths <= 0 {
				return nil, fmt.Errorf("price_schedule[%d]: term_months must be a positive integer", i)
			}
			if t.Price <= 0 {
				return nil, fmt.Errorf("price_schedule[%d]: price must be positive (received %.4f); use a non-zero listing price", i, t.Price)
			}
		}
		if err := checkSuppliedScheduleFloor(supplied, remainingMonths, originalTerm, count, *upfrontCost); err != nil {
			return nil, err
		}
		return supplied, nil
	}

	// Default: a single tier priced at the per-unit upfront residual discounted
	// by awsMarketplaceBuyerDiscountFactor. Per instance (Count-divided) and
	// upfront-only (recurring is the buyer's post-transfer obligation). The
	// awsMarketplaceFeePercent% AWS fee is deducted by the Marketplace on top.
	// The upfront cost is prorated by (remaining/original) so an older RI is not
	// overpriced (a 12-month RI at month 6 retains only half its upfront value).
	if remainingMonths <= 0 {
		remainingMonths = 1 // defensive: should not happen for an active RI
	}
	listPrice := marketplaceResidualPerUnit(remainingMonths, originalTerm, count, *upfrontCost) * awsMarketplaceBuyerDiscountFactor
	if listPrice <= 0 {
		// A no-upfront RI (upfrontCost <= 0) or an unknown/elapsed term
		// (originalTerm <= 0) makes marketplaceResidualPerUnit return 0, which
		// would otherwise produce a $0 listing sent straight to AWS -- the same
		// value the supplied-schedule branch above rejects with "price must be
		// positive". Fail loud instead of silently listing the RI for free.
		return nil, fmt.Errorf("cannot compute a default listing price for this RI (no upfront cost or unknown term to prorate); supply an explicit price_schedule")
	}
	return []MarketplacePriceTier{
		{TermMonths: int64(remainingMonths), Price: listPrice},
	}, nil
}
