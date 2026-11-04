package api

import (
	"context"
	"errors"
	"testing"
	"time"

	ec2svc "github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/ec2"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	smithy "github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// validMarketplacePurchaseID is a syntactically valid UUID accepted by
// validateUUID; the marketplace path keys on purchase_history.purchase_id.
const validMarketplacePurchaseID = "11111111-1111-1111-1111-111111111111"

// stubMarketplaceEC2 is a configurable stub implementing marketplaceEC2Client.
// Each field, when set, overrides the corresponding method's behavior and
// records the last request so tests can assert on what was sent to AWS.
type stubMarketplaceEC2 struct {
	createFn     func(ctx context.Context, req ec2svc.MarketplaceListingRequest) (ec2svc.MarketplaceListingResult, error)
	cancelFn     func(ctx context.Context, listingID string) (ec2svc.MarketplaceListingResult, error)
	describeFn   func(ctx context.Context, listingID string) (ec2svc.MarketplaceListingResult, error)
	fetchClassFn func(ctx context.Context, reservedInstancesID string) (string, error)

	lastCreateReq   ec2svc.MarketplaceListingRequest
	createCallCount int
	cancelCallCount int
	lastCancelID    string
	// describeCallCount counts DescribeMarketplaceListing calls (issue #525).
	describeCallCount int
}

func (s *stubMarketplaceEC2) CreateMarketplaceListing(ctx context.Context, req ec2svc.MarketplaceListingRequest) (ec2svc.MarketplaceListingResult, error) {
	s.createCallCount++
	s.lastCreateReq = req
	if s.createFn != nil {
		return s.createFn(ctx, req)
	}
	return ec2svc.MarketplaceListingResult{ListingID: "ril-default", State: config.ListingStateActive}, nil
}

func (s *stubMarketplaceEC2) DescribeMarketplaceListing(ctx context.Context, listingID string) (ec2svc.MarketplaceListingResult, error) {
	s.describeCallCount++
	if s.describeFn != nil {
		return s.describeFn(ctx, listingID)
	}
	return ec2svc.MarketplaceListingResult{ListingID: listingID, State: config.ListingStateActive}, nil
}

func (s *stubMarketplaceEC2) CancelMarketplaceListing(ctx context.Context, listingID string) (ec2svc.MarketplaceListingResult, error) {
	s.cancelCallCount++
	s.lastCancelID = listingID
	if s.cancelFn != nil {
		return s.cancelFn(ctx, listingID)
	}
	return ec2svc.MarketplaceListingResult{ListingID: listingID, State: config.ListingStateCancelled}, nil
}

func (s *stubMarketplaceEC2) FetchOfferingClass(ctx context.Context, reservedInstancesID string) (string, error) {
	if s.fetchClassFn != nil {
		return s.fetchClassFn(ctx, reservedInstancesID)
	}
	// Default: return "standard" so tests that set offering_class="" still proceed.
	return "standard", nil
}

// marketplaceTestAPIError is an smithy.APIError with a controllable fault so
// the error-mapping tests can exercise both the client- and server-fault paths.
type marketplaceTestAPIError struct {
	code    string
	message string
	fault   smithy.ErrorFault
}

func (e *marketplaceTestAPIError) Error() string                 { return e.code + ": " + e.message }
func (e *marketplaceTestAPIError) ErrorCode() string             { return e.code }
func (e *marketplaceTestAPIError) ErrorMessage() string          { return e.message }
func (e *marketplaceTestAPIError) ErrorFault() smithy.ErrorFault { return e.fault }

// newMarketplaceHandler wires a Handler with the supplied mocks and stub EC2
// client, pre-seeding the cached AWS config so loadAWSConfigWithRegion does not
// hit the real SDK.
func newMarketplaceHandler(cfgStore *MockConfigStore, authSvc *MockAuthService, ec2 *stubMarketplaceEC2) *Handler {
	h := &Handler{
		config:    cfgStore,
		credStore: &accountKeyStore{},
		auth:      authSvc,
		marketplaceEC2Factory: func(_ aws.Config) marketplaceEC2Client {
			return ec2
		},
	}
	h.awsCfgOnce.Do(func() { h.awsCfg = aws.Config{Region: "us-east-1"} })
	return h
}

func standardRow() *config.PurchaseHistoryRecord {
	acct := "acct-1"
	return &config.PurchaseHistoryRecord{
		PurchaseID:     validMarketplacePurchaseID,
		OfferingClass:  "standard",
		CloudAccountID: &acct,
		Region:         "us-east-1",
		Count:          3,
		// Term is stored in years (1 or 3) in purchase_history. The handler
		// multiplies by 12 before passing to computeRemainingMonths and
		// resolveMarketplacePriceSchedule, which both work in months.
		Term:        3,
		UpfrontCost: new(float64(1200)),
		Timestamp:   time.Now(),
	}
}

func marketplaceReq() *events.LambdaFunctionURLRequest {
	return &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer test-token"},
	}
}

func adminSession(t *testing.T, authSvc *MockAuthService) {
	t.Helper()
	t.Cleanup(func() { authSvc.AssertExpectations(t) })
	authSvc.On("ValidateSession", mock.Anything, "test-token").
		Return(&Session{UserID: "admin", Email: "admin@test.com"}, nil)
	authSvc.On("HasPermissionAPI", mock.Anything, "admin", auth.ActionSellAny, auth.ResourcePurchases).
		Return(true, nil).Maybe()
	authSvc.On("GetAllowedAccountsAPI", mock.Anything, "admin").Return([]string{"*"}, nil).Maybe()
	authSvc.On("HasPermissionForConstraintsAPI", mock.Anything, "admin", auth.ActionSellAny, auth.ResourcePurchases,
		[]auth.PermissionConstraints{{StrictScope: true, AccountIDs: []string{"acct-1"}, Regions: []string{"us-east-1"}}}).Return(true, nil).Maybe()
}

// --- Gap 3 handler tests (issue #292) ---

func TestMarketplaceList_ConvertibleRejected(t *testing.T) {
	cfgStore := &MockConfigStore{}
	authSvc := &MockAuthService{}
	adminSession(t, authSvc)

	row := standardRow()
	row.OfferingClass = "convertible"
	cfgStore.On("GetPurchaseHistoryByPurchaseID", mock.Anything, validMarketplacePurchaseID).
		Return(row, nil)

	h := newMarketplaceHandler(cfgStore, authSvc, &stubMarketplaceEC2{})
	_, err := h.marketplaceList(context.Background(), marketplaceReq(), validMarketplacePurchaseID)

	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok)
	assert.Equal(t, 400, ce.code)
	assert.Contains(t, err.Error(), "Standard")
	cfgStore.AssertExpectations(t)
}

func TestMarketplaceList_SellOwnAllowed(t *testing.T) {
	cfgStore := &MockConfigStore{}
	authSvc := &MockAuthService{}
	ec2 := &stubMarketplaceEC2{}

	authSvc.On("ValidateSession", mock.Anything, "test-token").
		Return(&Session{UserID: "user-1"}, nil)
	authSvc.On("HasPermissionAPI", mock.Anything, "user-1", auth.ActionSellAny, auth.ResourcePurchases).
		Return(false, nil)
	authSvc.On("HasPermissionAPI", mock.Anything, "user-1", auth.ActionSellOwn, auth.ResourcePurchases).
		Return(true, nil)
	// allowed accounts cover the row's cloud account.
	authSvc.On("GetAllowedAccountsAPI", mock.Anything, "user-1").
		Return([]string{"acct-1"}, nil)
	authSvc.On("HasPermissionForConstraintsAPI", mock.Anything, "user-1", auth.ActionSellOwn, auth.ResourcePurchases,
		[]auth.PermissionConstraints{{StrictScope: true, AccountIDs: []string{"acct-1"}, Regions: []string{"us-east-1"}}}).Return(true, nil)

	cfgStore.On("GetPurchaseHistoryByPurchaseID", mock.Anything, validMarketplacePurchaseID).
		Return(standardRow(), nil)
	cfgStore.On("ClaimMarketplaceListingSlot", mock.Anything, validMarketplacePurchaseID, mock.Anything, mock.Anything).
		Return(&config.MarketplaceListingClaim{ClientToken: "tok"}, nil)
	cfgStore.On("UpdatePurchaseHistoryListing", mock.Anything, validMarketplacePurchaseID, "ril-default", config.ListingStateActive).
		Return(nil)

	h := newMarketplaceHandler(cfgStore, authSvc, ec2)
	resp, err := h.marketplaceList(context.Background(), marketplaceReq(), validMarketplacePurchaseID)

	require.NoError(t, err)
	typed, ok := resp.(*MarketplaceListResponse)
	require.True(t, ok)
	assert.Equal(t, "ril-default", typed.ListingID)
	// multi-count: the row has Count=3, so the outbound request must list all 3.
	assert.Equal(t, int32(3), ec2.lastCreateReq.InstanceCount)
	cfgStore.AssertExpectations(t)
	authSvc.AssertExpectations(t)
}

func TestMarketplaceList_SellOwnDeniedWrongAccount(t *testing.T) {
	cfgStore := &MockConfigStore{}
	t.Cleanup(func() { cfgStore.AssertExpectations(t) })
	authSvc := &MockAuthService{}

	authSvc.On("ValidateSession", mock.Anything, "test-token").
		Return(&Session{UserID: "user-1"}, nil)
	authSvc.On("HasPermissionAPI", mock.Anything, "user-1", auth.ActionSellAny, auth.ResourcePurchases).
		Return(false, nil)
	authSvc.On("HasPermissionAPI", mock.Anything, "user-1", auth.ActionSellOwn, auth.ResourcePurchases).
		Return(true, nil)
	// allowed accounts do NOT include acct-1.
	authSvc.On("GetAllowedAccountsAPI", mock.Anything, "user-1").
		Return([]string{"acct-other"}, nil)

	cfgStore.On("GetPurchaseHistoryByPurchaseID", mock.Anything, validMarketplacePurchaseID).
		Return(standardRow(), nil)

	h := newMarketplaceHandler(cfgStore, authSvc, &stubMarketplaceEC2{})
	_, err := h.marketplaceList(context.Background(), marketplaceReq(), validMarketplacePurchaseID)

	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok)
	assert.Equal(t, 403, ce.code)
	authSvc.AssertExpectations(t)
}

func TestMarketplaceList_DuplicateActiveListing409(t *testing.T) {
	cfgStore := &MockConfigStore{}
	t.Cleanup(func() { cfgStore.AssertExpectations(t) })
	authSvc := &MockAuthService{}
	adminSession(t, authSvc)

	row := standardRow()
	row.ListingState = config.ListingStateActive
	row.ListingID = "ril-existing"
	cfgStore.On("GetPurchaseHistoryByPurchaseID", mock.Anything, validMarketplacePurchaseID).
		Return(row, nil)

	h := newMarketplaceHandler(cfgStore, authSvc, &stubMarketplaceEC2{})
	_, err := h.marketplaceList(context.Background(), marketplaceReq(), validMarketplacePurchaseID)

	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok)
	assert.Equal(t, 409, ce.code)
	assert.Contains(t, err.Error(), "ril-existing")
}

func TestMarketplaceList_AWSClientFaultMapsTo400(t *testing.T) {
	cfgStore := &MockConfigStore{}
	t.Cleanup(func() { cfgStore.AssertExpectations(t) })
	authSvc := &MockAuthService{}
	adminSession(t, authSvc)
	cfgStore.On("GetPurchaseHistoryByPurchaseID", mock.Anything, validMarketplacePurchaseID).
		Return(standardRow(), nil)
	cfgStore.On("ClaimMarketplaceListingSlot", mock.Anything, validMarketplacePurchaseID, mock.Anything, mock.Anything).
		Return(&config.MarketplaceListingClaim{ClientToken: "tok"}, nil)
	// AWS create fails, so the claim must be released back to the unlisted state.
	cfgStore.On("ReleaseMarketplaceListingClaim", mock.Anything, validMarketplacePurchaseID, "tok", "", false).
		Return(nil)

	ec2 := &stubMarketplaceEC2{
		createFn: func(_ context.Context, _ ec2svc.MarketplaceListingRequest) (ec2svc.MarketplaceListingResult, error) {
			return ec2svc.MarketplaceListingResult{}, &marketplaceTestAPIError{
				code: "InvalidReservedInstancesId", message: "bad RI", fault: smithy.FaultClient,
			}
		},
	}

	h := newMarketplaceHandler(cfgStore, authSvc, ec2)
	_, err := h.marketplaceList(context.Background(), marketplaceReq(), validMarketplacePurchaseID)

	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok)
	assert.Equal(t, 400, ce.code)
	assert.Contains(t, err.Error(), "bad RI")
}

func TestMarketplaceList_AWSServerFaultMapsTo502(t *testing.T) {
	cfgStore := &MockConfigStore{}
	t.Cleanup(func() { cfgStore.AssertExpectations(t) })
	authSvc := &MockAuthService{}
	adminSession(t, authSvc)
	cfgStore.On("GetPurchaseHistoryByPurchaseID", mock.Anything, validMarketplacePurchaseID).
		Return(standardRow(), nil)
	cfgStore.On("ClaimMarketplaceListingSlot", mock.Anything, validMarketplacePurchaseID, mock.Anything, mock.Anything).
		Return(&config.MarketplaceListingClaim{ClientToken: "tok"}, nil)
	// AWS create fails, so the claim must be released back to the unlisted state.
	cfgStore.On("ReleaseMarketplaceListingClaim", mock.Anything, validMarketplacePurchaseID, "tok", "", true).
		Return(nil)

	ec2 := &stubMarketplaceEC2{
		createFn: func(_ context.Context, _ ec2svc.MarketplaceListingRequest) (ec2svc.MarketplaceListingResult, error) {
			return ec2svc.MarketplaceListingResult{}, &marketplaceTestAPIError{
				code: "InternalError", message: "aws broke", fault: smithy.FaultServer,
			}
		},
	}

	h := newMarketplaceHandler(cfgStore, authSvc, ec2)
	_, err := h.marketplaceList(context.Background(), marketplaceReq(), validMarketplacePurchaseID)

	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok)
	assert.Equal(t, 502, ce.code)
}

func TestMarketplaceList_UnknownErrorMapsTo502(t *testing.T) {
	cfgStore := &MockConfigStore{}
	t.Cleanup(func() { cfgStore.AssertExpectations(t) })
	authSvc := &MockAuthService{}
	adminSession(t, authSvc)
	cfgStore.On("GetPurchaseHistoryByPurchaseID", mock.Anything, validMarketplacePurchaseID).
		Return(standardRow(), nil)
	cfgStore.On("ClaimMarketplaceListingSlot", mock.Anything, validMarketplacePurchaseID, mock.Anything, mock.Anything).
		Return(&config.MarketplaceListingClaim{ClientToken: "tok"}, nil)
	// AWS create fails, so the claim must be released back to the unlisted state.
	cfgStore.On("ReleaseMarketplaceListingClaim", mock.Anything, validMarketplacePurchaseID, "tok", "", true).
		Return(nil)

	ec2 := &stubMarketplaceEC2{
		createFn: func(_ context.Context, _ ec2svc.MarketplaceListingRequest) (ec2svc.MarketplaceListingResult, error) {
			return ec2svc.MarketplaceListingResult{}, errors.New("network reset")
		},
	}

	h := newMarketplaceHandler(cfgStore, authSvc, ec2)
	_, err := h.marketplaceList(context.Background(), marketplaceReq(), validMarketplacePurchaseID)

	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok)
	assert.Equal(t, 502, ce.code)
}

func TestMarketplaceList_DBFailureCompensatingRollback(t *testing.T) {
	cfgStore := &MockConfigStore{}
	authSvc := &MockAuthService{}
	adminSession(t, authSvc)
	cfgStore.On("GetPurchaseHistoryByPurchaseID", mock.Anything, validMarketplacePurchaseID).
		Return(standardRow(), nil)
	cfgStore.On("ClaimMarketplaceListingSlot", mock.Anything, validMarketplacePurchaseID, mock.Anything, mock.Anything).
		Return(&config.MarketplaceListingClaim{ClientToken: "tok"}, nil)
	// DB persist fails after the listing was created.
	cfgStore.On("UpdatePurchaseHistoryListing", mock.Anything, validMarketplacePurchaseID, "ril-default", config.ListingStateActive).
		Return(errors.New("db down"))
	// After the compensating cancel, the canceled listing is recorded so the
	// row is not left stuck in the pending state and the retry gets a new token.
	cfgStore.On("UpdatePurchaseHistoryListing", mock.Anything, validMarketplacePurchaseID, "ril-default", config.ListingStateCancelled).
		Return(nil)

	ec2 := &stubMarketplaceEC2{}
	h := newMarketplaceHandler(cfgStore, authSvc, ec2)
	_, err := h.marketplaceList(context.Background(), marketplaceReq(), validMarketplacePurchaseID)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "rolled back")
	// The compensating cancel must have fired against the just-created listing.
	assert.Equal(t, 1, ec2.cancelCallCount)
	assert.Equal(t, "ril-default", ec2.lastCancelID)
	cfgStore.AssertExpectations(t)
}

func TestMarketplaceList_DBFailureAndCancelFailureKeepsListing(t *testing.T) {
	for _, tc := range []struct {
		name      string
		recordErr error
		wantMsg   string
	}{
		{name: "listing recorded", wantMsg: "now recorded on this RI"},
		{name: "db still down", recordErr: errors.New("db still down"), wantMsg: "stays locked as pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgStore := &MockConfigStore{}
			authSvc := &MockAuthService{}
			adminSession(t, authSvc)
			cfgStore.On("GetPurchaseHistoryByPurchaseID", mock.Anything, validMarketplacePurchaseID).
				Return(standardRow(), nil)
			cfgStore.On("ClaimMarketplaceListingSlot", mock.Anything, validMarketplacePurchaseID, mock.Anything, mock.Anything).
				Return(&config.MarketplaceListingClaim{ClientToken: "tok"}, nil)
			cfgStore.On("UpdatePurchaseHistoryListing", mock.Anything, validMarketplacePurchaseID, "ril-default", config.ListingStateActive).
				Return(errors.New("db down")).Once()
			cfgStore.On("UpdatePurchaseHistoryListing", mock.Anything, validMarketplacePurchaseID, "ril-default", config.ListingStateActive).
				Return(tc.recordErr).Once()

			ec2 := &stubMarketplaceEC2{
				cancelFn: func(_ context.Context, _ string) (ec2svc.MarketplaceListingResult, error) {
					return ec2svc.MarketplaceListingResult{}, errors.New("throttled")
				},
			}
			h := newMarketplaceHandler(cfgStore, authSvc, ec2)
			_, err := h.marketplaceList(context.Background(), marketplaceReq(), validMarketplacePurchaseID)

			require.Error(t, err)
			ce, ok := IsClientError(err)
			require.True(t, ok, "operator must see the message, not a generic 500: %v", err)
			assert.Equal(t, 502, ce.code)
			assert.NotContains(t, ce.message, "rolled back")
			assert.Contains(t, ce.message, "ril-default")
			assert.Contains(t, ce.message, "throttled")
			assert.Contains(t, ce.message, tc.wantMsg)
			assert.Equal(t, 1, ec2.cancelCallCount)
			cfgStore.AssertNotCalled(t, "ReleaseMarketplaceListingClaim", mock.Anything, validMarketplacePurchaseID, mock.Anything, mock.Anything, mock.Anything)
			cfgStore.AssertExpectations(t)
		})
	}
}

func TestMarketplaceList_CancelFailureRecordsNonActiveStateAsPending(t *testing.T) {
	for _, tc := range []struct {
		name       string
		awsState   string
		wantState  string
		wantMsg    string
		wantNotMsg string
	}{
		{name: "pending", awsState: config.ListingStatePending, wantState: config.ListingStatePending, wantMsg: "needs reconciliation", wantNotMsg: "cancel it again"},
		// An empty status is never recorded (issue #525): with the read-back
		// failing too, the listing is recorded as pending from the start.
		{name: "empty", awsState: "", wantState: config.ListingStatePending, wantMsg: "needs reconciliation", wantNotMsg: "cancel it again"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgStore := &MockConfigStore{}
			authSvc := &MockAuthService{}
			adminSession(t, authSvc)
			cfgStore.On("GetPurchaseHistoryByPurchaseID", mock.Anything, validMarketplacePurchaseID).
				Return(standardRow(), nil)
			cfgStore.On("ClaimMarketplaceListingSlot", mock.Anything, validMarketplacePurchaseID, mock.Anything, mock.Anything).
				Return(&config.MarketplaceListingClaim{ClientToken: "tok"}, nil)
			cfgStore.On("UpdatePurchaseHistoryListing", mock.Anything, validMarketplacePurchaseID, "ril-x", tc.wantState).
				Return(errors.New("db down")).Once()
			cfgStore.On("UpdatePurchaseHistoryListing", mock.Anything, validMarketplacePurchaseID, "ril-x", tc.wantState).
				Return(nil).Once()

			ec2 := &stubMarketplaceEC2{
				createFn: func(_ context.Context, _ ec2svc.MarketplaceListingRequest) (ec2svc.MarketplaceListingResult, error) {
					return ec2svc.MarketplaceListingResult{ListingID: "ril-x", State: tc.awsState}, nil
				},
				cancelFn: func(_ context.Context, _ string) (ec2svc.MarketplaceListingResult, error) {
					return ec2svc.MarketplaceListingResult{}, errors.New("throttled")
				},
				describeFn: func(_ context.Context, _ string) (ec2svc.MarketplaceListingResult, error) {
					return ec2svc.MarketplaceListingResult{}, errors.New("throttled")
				},
			}
			h := newMarketplaceHandler(cfgStore, authSvc, ec2)
			_, err := h.marketplaceList(context.Background(), marketplaceReq(), validMarketplacePurchaseID)

			ce, ok := IsClientError(err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, 502, ce.code)
			assert.Contains(t, ce.message, tc.wantMsg)
			assert.Contains(t, ce.message, "ril-x")
			assert.NotContains(t, ce.message, tc.wantNotMsg)
			cfgStore.AssertExpectations(t)
		})
	}
}

func TestMarketplaceList_CancelFailureAfterRequestContextCanceledStillRecords(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cfgStore := &MockConfigStore{}
	authSvc := &MockAuthService{}
	adminSession(t, authSvc)
	cfgStore.On("GetPurchaseHistoryByPurchaseID", mock.Anything, validMarketplacePurchaseID).
		Return(standardRow(), nil)
	cfgStore.On("ClaimMarketplaceListingSlot", mock.Anything, validMarketplacePurchaseID, mock.Anything, mock.Anything).
		Return(&config.MarketplaceListingClaim{ClientToken: "tok"}, nil)
	// The request context dies during the first write, as on a deadline.
	cfgStore.On("UpdatePurchaseHistoryListing", mock.Anything, validMarketplacePurchaseID, "ril-default", config.ListingStateActive).
		Run(func(mock.Arguments) { cancel() }).Return(errors.New("db down")).Once()
	liveCtx := mock.MatchedBy(func(c context.Context) bool { return c.Err() == nil })
	cfgStore.On("UpdatePurchaseHistoryListing", liveCtx, validMarketplacePurchaseID, "ril-default", config.ListingStateActive).
		Return(nil).Once()

	ec2 := &stubMarketplaceEC2{
		cancelFn: func(c context.Context, _ string) (ec2svc.MarketplaceListingResult, error) {
			require.NoError(t, c.Err(), "compensating cancel must not run on the canceled request context")
			return ec2svc.MarketplaceListingResult{}, errors.New("throttled")
		},
	}
	h := newMarketplaceHandler(cfgStore, authSvc, ec2)
	_, err := h.marketplaceList(ctx, marketplaceReq(), validMarketplacePurchaseID)

	ce, ok := IsClientError(err)
	require.True(t, ok, "%v", err)
	assert.Contains(t, ce.message, "now recorded on this RI")
	cfgStore.AssertExpectations(t)
}

func TestMarketplaceList_DefaultScheduleProrationMath(t *testing.T) {
	// remaining=12, term=12, count=1, upfront=1200 => per-unit residual
	// 1200*12/12/1 = 1200; default price = 1200*0.95 = 1140.
	schedule, err := resolveMarketplacePriceSchedule(nil, 12, 12, 1, new(float64(1200)))
	require.NoError(t, err)
	require.Len(t, schedule, 1)
	assert.Equal(t, int64(12), schedule[0].TermMonths)
	assert.InDelta(t, 1140.0, schedule[0].Price, 0.001)

	// Half the term elapsed, count=1: remaining=6, term=12, upfront=1200.
	// per-unit residual = 1200*6/12/1 = 600; recurring is excluded (the buyer
	// assumes it post-transfer); default price = 600*0.95 = 570.
	schedule, err = resolveMarketplacePriceSchedule(nil, 6, 12, 1, new(float64(1200)))
	require.NoError(t, err)
	require.Len(t, schedule, 1)
	assert.Equal(t, int64(6), schedule[0].TermMonths)
	assert.InDelta(t, 570.0, schedule[0].Price, 0.001)

	// Per-instance pricing: count=3 on the same $1200 row-total upfront divides
	// the residual by the instance count, so the default per-unit price is a
	// third of the count=1 price (1200/3=400 per-unit residual; 400*0.95=380),
	// NOT the row-total (which would 3x-overprice each listed unit).
	schedule, err = resolveMarketplacePriceSchedule(nil, 12, 12, 3, new(float64(1200)))
	require.NoError(t, err)
	require.Len(t, schedule, 1)
	assert.Equal(t, int64(12), schedule[0].TermMonths)
	assert.InDelta(t, 380.0, schedule[0].Price, 0.001)
}

func TestMarketplaceCancel_HappyPath(t *testing.T) {
	cfgStore := &MockConfigStore{}
	authSvc := &MockAuthService{}
	adminSession(t, authSvc)

	row := standardRow()
	row.ListingState = config.ListingStateActive
	row.ListingID = "ril-cancel"
	cfgStore.On("GetPurchaseHistoryByPurchaseID", mock.Anything, validMarketplacePurchaseID).
		Return(row, nil)
	cfgStore.On("UpdatePurchaseHistoryListing", mock.Anything, validMarketplacePurchaseID, "ril-cancel", config.ListingStateCancelled).
		Return(nil)

	ec2 := &stubMarketplaceEC2{}
	h := newMarketplaceHandler(cfgStore, authSvc, ec2)
	resp, err := h.marketplaceCancel(context.Background(), marketplaceReq(), validMarketplacePurchaseID)

	require.NoError(t, err)
	m, ok := resp.(map[string]string)
	require.True(t, ok)
	assert.Equal(t, config.ListingStateCancelled, m["listing_state"])
	assert.Equal(t, "ril-cancel", ec2.lastCancelID)
	cfgStore.AssertExpectations(t)
}

func TestMarketplaceCancel_NoActiveListing409(t *testing.T) {
	cfgStore := &MockConfigStore{}
	t.Cleanup(func() { cfgStore.AssertExpectations(t) })
	authSvc := &MockAuthService{}
	adminSession(t, authSvc)

	row := standardRow() // ListingState empty -> not active
	cfgStore.On("GetPurchaseHistoryByPurchaseID", mock.Anything, validMarketplacePurchaseID).
		Return(row, nil)

	h := newMarketplaceHandler(cfgStore, authSvc, &stubMarketplaceEC2{})
	_, err := h.marketplaceCancel(context.Background(), marketplaceReq(), validMarketplacePurchaseID)

	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok)
	assert.Equal(t, 409, ce.code)
}

// TestMarketplaceList_ConcurrentClaimLoses409 reproduces the concurrent-create
// race (issue #292 CR): the pre-flight listing_state read passes, but a parallel
// request has already reserved the listing slot, so the atomic claim loses. The
// handler must return 409 and must NOT call AWS (creating a second live listing
// for one RI would be a real double-listing on the money path). This test fails
// on the pre-guard code, which called AWS unconditionally after the read-check.
func TestMarketplaceList_ConcurrentClaimLoses409(t *testing.T) {
	cfgStore := &MockConfigStore{}
	authSvc := &MockAuthService{}
	adminSession(t, authSvc)

	cfgStore.On("GetPurchaseHistoryByPurchaseID", mock.Anything, validMarketplacePurchaseID).
		Return(standardRow(), nil)
	// A concurrent request already holds the slot: the atomic claim reports lost.
	cfgStore.On("ClaimMarketplaceListingSlot", mock.Anything, validMarketplacePurchaseID, mock.Anything, mock.Anything).
		Return(nil, nil)

	ec2 := &stubMarketplaceEC2{}
	h := newMarketplaceHandler(cfgStore, authSvc, ec2)
	_, err := h.marketplaceList(context.Background(), marketplaceReq(), validMarketplacePurchaseID)

	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok)
	assert.Equal(t, 409, ce.code)
	// The guard must fire before AWS: no listing may be created when the claim loses.
	assert.Equal(t, 0, ec2.createCallCount)
	cfgStore.AssertExpectations(t)
}

// TestMarketplaceList_ClaimErrorMapsToInternal verifies a claim DB error is
// surfaced (not swallowed) and blocks the AWS call, so a broken slot-reservation
// never silently proceeds to create an unguarded listing.
func TestMarketplaceList_ClaimErrorMapsToInternal(t *testing.T) {
	cfgStore := &MockConfigStore{}
	authSvc := &MockAuthService{}
	adminSession(t, authSvc)

	cfgStore.On("GetPurchaseHistoryByPurchaseID", mock.Anything, validMarketplacePurchaseID).
		Return(standardRow(), nil)
	cfgStore.On("ClaimMarketplaceListingSlot", mock.Anything, validMarketplacePurchaseID, mock.Anything, mock.Anything).
		Return(nil, errors.New("db down"))

	ec2 := &stubMarketplaceEC2{}
	h := newMarketplaceHandler(cfgStore, authSvc, ec2)
	_, err := h.marketplaceList(context.Background(), marketplaceReq(), validMarketplacePurchaseID)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "reserve marketplace listing slot")
	// A failed claim must not fall through to AWS.
	assert.Equal(t, 0, ec2.createCallCount)
	cfgStore.AssertExpectations(t)
}

// --- Regression tests for the three blocking defects fixed in this PR ---

// TestResolveMarketplacePriceSchedule_ZeroPriceRejected verifies Defect 3:
// Price=0 must be rejected with a 400 so a user cannot list an RI for free.
// This is the fail-before/pass-after regression test (the old code accepted
// Price >= 0, so Price=0 was accepted; the fix requires Price > 0).
func TestResolveMarketplacePriceSchedule_ZeroPriceRejected(t *testing.T) {
	_, err := resolveMarketplacePriceSchedule([]MarketplacePriceTier{
		{TermMonths: 6, Price: 0},
	}, 6, 12, 1, new(float64(1200)))
	require.Error(t, err, "Price=0 must be rejected")
	assert.Contains(t, err.Error(), "positive")
}

// TestResolveMarketplacePriceSchedule_BelowFloorRejected verifies Defect A
// (money): the floor is enforced PER TIER on the one-time sale Price, not
// Price*TermMonths. A $5 one-time price on a $1,200 per-unit residual with 12
// of 12 months remaining is 0.4% of residual and must be rejected (floor is 5%
// = $60). This FAILS on the pre-fix code, which multiplied Price*TermMonths
// (5*12 = $60) and compared against a $60 floor, letting it pass.
func TestResolveMarketplacePriceSchedule_BelowFloorRejected(t *testing.T) {
	_, err := resolveMarketplacePriceSchedule([]MarketplacePriceTier{
		{TermMonths: 12, Price: 5.0},
	}, 12, 12, 1, new(float64(1200)))
	require.Error(t, err, "a $5 one-time price on a $1,200 residual must be rejected by the per-tier floor")
	assert.Contains(t, err.Error(), "floor")
	assert.Contains(t, err.Error(), "residual")
}

// TestResolveMarketplacePriceSchedule_PerUnitFloor verifies Defect B (money):
// the floor basis is PER INSTANCE. On a $1,200 row-total upfront with count=3,
// the per-unit residual is $400, so the 5% floor is $20/unit. A $19 one-time
// price is rejected; a $21 price is accepted -- proving the floor divides the
// row total by count instead of applying the $1,200 row total per unit.
func TestResolveMarketplacePriceSchedule_PerUnitFloor(t *testing.T) {
	_, errBelow := resolveMarketplacePriceSchedule([]MarketplacePriceTier{
		{TermMonths: 12, Price: 19.0},
	}, 12, 12, 3, new(float64(1200)))
	require.Error(t, errBelow, "$19 is below the $20 per-unit floor (5%% of $400 per-unit residual)")
	assert.Contains(t, errBelow.Error(), "per-unit residual")

	schedule, errAbove := resolveMarketplacePriceSchedule([]MarketplacePriceTier{
		{TermMonths: 12, Price: 21.0},
	}, 12, 12, 3, new(float64(1200)))
	require.NoError(t, errAbove, "$21 clears the $20 per-unit floor")
	require.Len(t, schedule, 1)
	assert.InDelta(t, 21.0, schedule[0].Price, 0.001)
}

// TestResolveMarketplacePriceSchedule_ZeroDefaultPriceRejected verifies the
// fix for the $0 default listing bug: a no-upfront RI (upfrontCost <= 0)
// hitting the default-schedule branch (no supplied price_schedule) must
// return an error instead of silently computing a $0 listing price.
// marketplaceResidualPerUnit returns 0 whenever upfrontCost <= 0, and the
// pre-fix code sent that straight through as {Price: 0} -- the exact value
// the supplied-schedule branch above rejects with "price must be positive".
// This FAILS on the pre-fix code (no error, schedule[0].Price == 0) and
// PASSES after (explicit error, nil schedule).
func TestResolveMarketplacePriceSchedule_ZeroDefaultPriceRejected(t *testing.T) {
	schedule, err := resolveMarketplacePriceSchedule(nil, 12, 12, 1, new(float64))
	require.Error(t, err, "a no-upfront RI must not silently produce a $0 default listing")
	assert.Nil(t, schedule)
	assert.Contains(t, err.Error(), "cannot compute a default listing price")

	// Same failure mode when the term is unknown (originalTerm <= 0), e.g. an
	// imported/external row where the contract term could not be resolved.
	schedule, err = resolveMarketplacePriceSchedule(nil, 12, 0, 1, new(float64(1200)))
	require.Error(t, err, "an unknown term must not silently produce a $0 default listing")
	assert.Nil(t, schedule)
	assert.Contains(t, err.Error(), "cannot compute a default listing price")
}

// TestResolveMarketplacePriceSchedule_TermExceedsRemainingRejected verifies the
// Defect A term-range guardrail: a tier whose TermMonths exceeds the RI's
// remaining months is rejected outright (an out-of-range term would let a
// caller arbitrarily inflate the tier's implied value). remaining=6, tier
// term=12 -> rejected.
func TestResolveMarketplacePriceSchedule_TermExceedsRemainingRejected(t *testing.T) {
	_, err := resolveMarketplacePriceSchedule([]MarketplacePriceTier{
		{TermMonths: 12, Price: 1000},
	}, 6, 12, 1, new(float64(1200)))
	require.Error(t, err, "term_months exceeding remaining term must be rejected")
	assert.Contains(t, err.Error(), "exceeds the RI's remaining term")
}

// TestMarketplaceList_EmptyOfferingClassFetchedStandard verifies Defect 2:
// When purchase_history.offering_class is empty (pre-migration rows and
// externally-created RIs), the handler fetches the class from AWS, persists
// it, and proceeds when AWS reports "standard". This is the sell path becoming
// reachable for externally-created Standard RIs.
func TestMarketplaceList_EmptyOfferingClassFetchedStandard(t *testing.T) {
	cfgStore := &MockConfigStore{}
	authSvc := &MockAuthService{}
	adminSession(t, authSvc)

	// Row has no offering_class (simulates pre-migration or externally-created RI).
	row := standardRow()
	row.OfferingClass = ""
	cfgStore.On("GetPurchaseHistoryByPurchaseID", mock.Anything, validMarketplacePurchaseID).
		Return(row, nil)
	// The handler must stamp the fetched class back to the DB.
	cfgStore.On("StampOfferingClass", mock.Anything, validMarketplacePurchaseID, "standard").
		Return(nil)
	cfgStore.On("ClaimMarketplaceListingSlot", mock.Anything, validMarketplacePurchaseID, mock.Anything, mock.Anything).
		Return(&config.MarketplaceListingClaim{ClientToken: "tok"}, nil)
	cfgStore.On("UpdatePurchaseHistoryListing", mock.Anything, validMarketplacePurchaseID, "ril-default", config.ListingStateActive).
		Return(nil)

	// AWS reports this RI is "standard".
	ec2 := &stubMarketplaceEC2{
		fetchClassFn: func(_ context.Context, id string) (string, error) {
			assert.Equal(t, validMarketplacePurchaseID, id)
			return "standard", nil
		},
	}
	h := newMarketplaceHandler(cfgStore, authSvc, ec2)
	resp, err := h.marketplaceList(context.Background(), marketplaceReq(), validMarketplacePurchaseID)

	require.NoError(t, err, "standard RI with empty offering_class must be listable after lazy-populate")
	typed, ok := resp.(*MarketplaceListResponse)
	require.True(t, ok)
	assert.Equal(t, "ril-default", typed.ListingID)
	cfgStore.AssertExpectations(t)
}

// TestMarketplaceList_EmptyOfferingClassFetchedConvertible verifies Defect 2
// guard: when AWS reports the RI is "convertible", the handler must reject
// with 400 even if offering_class was empty in the DB (not silently proceed).
func TestMarketplaceList_EmptyOfferingClassFetchedConvertible(t *testing.T) {
	cfgStore := &MockConfigStore{}
	authSvc := &MockAuthService{}
	adminSession(t, authSvc)

	row := standardRow()
	row.OfferingClass = ""
	cfgStore.On("GetPurchaseHistoryByPurchaseID", mock.Anything, validMarketplacePurchaseID).
		Return(row, nil)
	// The handler stamps the fetched class back even when it is convertible.
	cfgStore.On("StampOfferingClass", mock.Anything, validMarketplacePurchaseID, "convertible").
		Return(nil)

	ec2 := &stubMarketplaceEC2{
		fetchClassFn: func(_ context.Context, _ string) (string, error) {
			return "convertible", nil
		},
	}
	h := newMarketplaceHandler(cfgStore, authSvc, ec2)
	_, err := h.marketplaceList(context.Background(), marketplaceReq(), validMarketplacePurchaseID)

	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok)
	assert.Equal(t, 400, ce.code)
	assert.Contains(t, err.Error(), "Standard")
	cfgStore.AssertExpectations(t)
}

// --- Regression tests for the #808 follow-up: term years->months unit conversion ---

// TestComputeRemainingMonths exercises the helper directly, confirming it treats
// its termMonths parameter as months (not years).
func TestComputeRemainingMonths(t *testing.T) {
	now := time.Now()
	const month = time.Duration(float64(24*30.4375) * float64(time.Hour))
	remaining := func(purchase time.Time, termMonths int) int {
		t.Helper()
		r, err := computeRemainingMonths(purchase, termMonths, now)
		require.NoError(t, err)
		return r
	}
	requireUnprocessable := func(purchase time.Time, termMonths int, name string) {
		t.Helper()
		_, err := computeRemainingMonths(purchase, termMonths, now)
		require.Error(t, err, name)
		ce, ok := IsClientError(err)
		require.True(t, ok, name)
		assert.Equal(t, 422, ce.code, name)
	}

	// Absent timestamp or unsupported term -> 422 (issues #98, #449).
	requireUnprocessable(time.Time{}, 36, "zero time")
	for _, term := range []int{0, -1, 24, 48} {
		requireUnprocessable(now, term, "term months")
	}

	assert.Equal(t, 36, remaining(now, 36), "fresh 36-month RI")
	assert.Equal(t, 12, remaining(now, 12), "fresh 12-month RI")
	assert.InDelta(t, 30, remaining(now.Add(-6*30*24*time.Hour), 36), 2, "36-month RI bought 6 months ago")

	// The one-month cutoff AWS requires: exactly 1.0 month remaining lists,
	// just under it is rejected, and an expired RI is rejected.
	assert.Equal(t, 1, remaining(now.Add(-35*month), 36), "exactly 1.0 month remaining lists")
	assert.Equal(t, 1, remaining(now.Add(-35*month+time.Hour), 36), "just above 1.0 month remaining lists")
	requireUnprocessable(now.Add(-35*month-time.Minute), 36, "just under 1.0 month remaining")
	requireUnprocessable(now.Add(-36*month), 36, "remaining exactly zero")
	requireUnprocessable(now.Add(-40*month), 36, "expired")
}

func marketplaceListHTTPRequest() *events.LambdaFunctionURLRequest {
	return &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer test-token", "content-type": "application/json"},
		Body:    "{}",
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{
				Method: "POST",
				Path:   "/api/purchases/" + validMarketplacePurchaseID + "/marketplace-list",
			},
		},
	}
}

// TestMarketplaceListHTTP_UnlistableTermsReturn422 drives the real HandleRequest
// path (issue #449): an expired RI, a missing timestamp and a non-positive term
// must reach the client as a 422 with a reason, never a 500, and must not
// claim a listing slot or call AWS.
func TestMarketplaceListHTTP_UnlistableTermsReturn422(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mutate   func(r *config.PurchaseHistoryRecord)
		wantBody string
	}{
		{"expired 3-year RI", func(r *config.PurchaseHistoryRecord) {
			r.Timestamp = time.Now().Add(-37 * 31 * 24 * time.Hour)
		}, "expired"},
		{"expired 1-year RI", func(r *config.PurchaseHistoryRecord) {
			r.Term = 1
			r.Timestamp = time.Now().Add(-13 * 31 * 24 * time.Hour)
		}, "expired"},
		{"term elapsed by an hour", func(r *config.PurchaseHistoryRecord) {
			r.Timestamp = time.Now().Add(-time.Duration(36*30.4375*24+1) * time.Hour)
		}, "expired"},
		{"zero timestamp", func(r *config.PurchaseHistoryRecord) { r.Timestamp = time.Time{} }, "timestamp"},
		{"under one month remaining", func(r *config.PurchaseHistoryRecord) {
			r.Timestamp = time.Now().Add(-1090 * 24 * time.Hour)
		}, "less than one month"},
		{"term 2 years", func(r *config.PurchaseHistoryRecord) { r.Term = 2 }, "expected 1 or 3 years"},
		{"term 4 years", func(r *config.PurchaseHistoryRecord) { r.Term = 4 }, "expected 1 or 3 years"},
		{"zero term", func(r *config.PurchaseHistoryRecord) { r.Term = 0 }, "expected 1 or 3 years"},
		{"negative term", func(r *config.PurchaseHistoryRecord) { r.Term = -1 }, "expected 1 or 3 years"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgStore := &MockConfigStore{}
			t.Cleanup(func() { cfgStore.AssertExpectations(t) })
			authSvc := &MockAuthService{}
			adminSession(t, authSvc)
			authSvc.On("ValidateCSRFToken", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
			row := standardRow()
			tc.mutate(row)
			cfgStore.On("GetPurchaseHistoryByPurchaseID", mock.Anything, validMarketplacePurchaseID).Return(row, nil)

			ec2 := &stubMarketplaceEC2{}
			h := newMarketplaceHandler(cfgStore, authSvc, ec2)
			resp, err := h.HandleRequest(context.Background(), marketplaceListHTTPRequest())

			require.NoError(t, err)
			assert.Equal(t, 422, resp.StatusCode, "body: %s", resp.Body)
			assert.Contains(t, resp.Body, tc.wantBody)
			assert.Zero(t, ec2.createCallCount, "no AWS call for an unlistable RI")
			cfgStore.AssertNotCalled(t, "ClaimMarketplaceListingSlot", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
		})
	}
}

// TestMarketplaceListHTTP_ValidRIStillLists guards the boundary on the other
// side: a fresh RI and one with a bit over one month remaining both list (200).
func TestMarketplaceListHTTP_ValidRIStillLists(t *testing.T) {
	for _, tc := range []struct {
		name      string
		timestamp time.Time
	}{
		{"fresh", time.Now()},
		{"just over one month remaining", time.Now().Add(-1050 * 24 * time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgStore := &MockConfigStore{}
			t.Cleanup(func() { cfgStore.AssertExpectations(t) })
			authSvc := &MockAuthService{}
			adminSession(t, authSvc)
			authSvc.On("ValidateCSRFToken", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
			row := standardRow()
			row.Timestamp = tc.timestamp
			cfgStore.On("GetPurchaseHistoryByPurchaseID", mock.Anything, validMarketplacePurchaseID).Return(row, nil)
			cfgStore.On("ClaimMarketplaceListingSlot", mock.Anything, validMarketplacePurchaseID, mock.Anything, mock.Anything).Return(&config.MarketplaceListingClaim{ClientToken: "tok"}, nil)
			cfgStore.On("UpdatePurchaseHistoryListing", mock.Anything, validMarketplacePurchaseID, "ril-default", config.ListingStateActive).Return(nil)

			ec2 := &stubMarketplaceEC2{}
			h := newMarketplaceHandler(cfgStore, authSvc, ec2)
			resp, err := h.HandleRequest(context.Background(), marketplaceListHTTPRequest())

			require.NoError(t, err)
			assert.Equal(t, 200, resp.StatusCode, "body: %s", resp.Body)
			assert.Equal(t, 1, ec2.createCallCount)
		})
	}
}

// TestMarketplaceList_TermYearsConvertedToMonths is the end-to-end regression
// test for the #808 follow-up money bug. purchase_history.term is stored in
// years (1 or 3); the handler was passing it unchanged to computeRemainingMonths
// (which expects months) and to resolveMarketplacePriceSchedule (originalTerm
// documented as months). For a 3-year RI sold 6 months in:
//
//	pre-fix:  remainingMonths = max(1, 3-6) = 1;  default price ~= $1,140 (3600*1/3*0.95)
//	post-fix: remainingMonths ~= 30;               default price ~= $2,850 (3600*30/36*0.95)
//
// A caller-supplied schedule with term_months=30 was rejected pre-fix because
// remainingMonths was 1 (30 > 1); post-fix it is accepted.
func TestMarketplaceList_TermYearsConvertedToMonths(t *testing.T) {
	t.Run("default schedule price reflects true remaining value", func(t *testing.T) {
		cfgStore := &MockConfigStore{}
		authSvc := &MockAuthService{}
		adminSession(t, authSvc)

		row := standardRow()
		row.Term = 3                                             // 3-year RI (stored in years)
		row.Timestamp = time.Now().Add(-6 * 30 * 24 * time.Hour) // purchased ~6 months ago
		row.UpfrontCost = new(float64(3600))
		row.Count = 1

		cfgStore.On("GetPurchaseHistoryByPurchaseID", mock.Anything, validMarketplacePurchaseID).
			Return(row, nil)
		cfgStore.On("ClaimMarketplaceListingSlot", mock.Anything, validMarketplacePurchaseID, mock.Anything, mock.Anything).
			Return(&config.MarketplaceListingClaim{ClientToken: "tok"}, nil)
		cfgStore.On("UpdatePurchaseHistoryListing", mock.Anything, validMarketplacePurchaseID, "ril-default", config.ListingStateActive).
			Return(nil)

		ec2 := &stubMarketplaceEC2{}
		h := newMarketplaceHandler(cfgStore, authSvc, ec2)
		resp, err := h.marketplaceList(context.Background(), marketplaceReq(), validMarketplacePurchaseID)

		require.NoError(t, err)
		typed, ok := resp.(*MarketplaceListResponse)
		require.True(t, ok)

		require.Len(t, typed.PriceSchedule, 1)
		// remainingMonths must be ~30 (36 total - 6 elapsed), not 1 (the pre-fix value
		// produced by treating 3 years as 3 months and flooring the negative result).
		assert.InDelta(t, 30, int(typed.PriceSchedule[0].TermMonths), 2,
			"schedule term_months should reflect real remaining months (~30), not the pre-fix value of 1")
		// Default price: residual = 3600*(30/36)=3000, discounted by 0.95 = ~2850.
		// Pre-fix price: residual = 3600*(1/3)=1200, price = 1200*0.95 = 1140.
		assert.InDelta(t, 2850.0, typed.PriceSchedule[0].Price, 150,
			"default price should be ~$2,850 (30/36 of $3,600 * 0.95), not ~$1,140 (the pre-fix value)")
		assert.Greater(t, typed.PriceSchedule[0].Price, 2000.0,
			"default price must be well above the pre-fix ~$1,140 value")

		// The AWS PriceScheduleSpecification.Term must reflect the real remaining months.
		require.Len(t, ec2.lastCreateReq.PriceSchedule, 1)
		assert.InDelta(t, 30, int(ec2.lastCreateReq.PriceSchedule[0].Term), 2,
			"AWS PriceScheduleSpecification.Term must be real remaining months (~30), not years")

		cfgStore.AssertExpectations(t)
	})

	t.Run("supplied schedule with real remaining term accepted post-fix", func(t *testing.T) {
		// Pre-fix: remainingMonths=1 (years=3 misread as months), so tier term=30 > 1
		// triggered "term_months exceeds the RI's remaining term". Post-fix:
		// remainingMonths=30 and term=36, so 30 <= 30 is valid and $2,500 clears the
		// 5% floor (~$150 on a $3,000 prorated residual).
		schedule, err := resolveMarketplacePriceSchedule([]MarketplacePriceTier{
			{TermMonths: 30, Price: 2500},
		}, 30, 36, 1, new(float64(3600)))
		require.NoError(t, err,
			"a {TermMonths:30, Price:2500} schedule must be accepted when remainingMonths=30 (post-fix)")
		require.Len(t, schedule, 1)
		assert.Equal(t, int64(30), schedule[0].TermMonths)
		assert.InDelta(t, 2500.0, schedule[0].Price, 0.001)
	})

	t.Run("pre-fix: supplied schedule with term 30 rejected when remaining is 1", func(t *testing.T) {
		// This sub-test documents the pre-fix breakage: when remainingMonths was
		// computed as 1 (3 years misread as 3 months, then 3-6=-3, floored to 1),
		// a legitimate 30-month schedule was rejected. This test FAILS on pre-fix
		// code and PASSES after the fix (because post-fix remainingMonths=30 makes
		// a 30-month tier valid, so the call below -- which directly exercises the
		// old broken inputs -- must still reject it).
		_, err := resolveMarketplacePriceSchedule([]MarketplacePriceTier{
			{TermMonths: 30, Price: 2500},
		}, 1, 3, 1, new(float64(3600)))
		require.Error(t, err,
			"pre-fix inputs (remainingMonths=1, originalTerm=3 months) must reject a 30-month tier")
		assert.Contains(t, err.Error(), "exceeds the RI's remaining term")
	})
}

// TestMarketplaceList_InvalidTermReturnsError verifies the no-silent-fallbacks
// rule: a purchase_history row with term=0 (invalid -- valid years are 1 or 3)
// must produce an explicit error, not a garbage price computed from 0*12=0 months.
func TestMarketplaceList_InvalidTermReturnsError(t *testing.T) {
	cfgStore := &MockConfigStore{}
	authSvc := &MockAuthService{}
	adminSession(t, authSvc)

	row := standardRow()
	row.Term = 0 // invalid: purchase_history.term must be 1 or 3 (years)
	cfgStore.On("GetPurchaseHistoryByPurchaseID", mock.Anything, validMarketplacePurchaseID).
		Return(row, nil)

	h := newMarketplaceHandler(cfgStore, authSvc, &stubMarketplaceEC2{})
	_, err := h.marketplaceList(context.Background(), marketplaceReq(), validMarketplacePurchaseID)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid term")
	cfgStore.AssertExpectations(t)
}

// TestMarketplaceList_ZeroTimestampReturnsError: a purchase_history row with no
// timestamp must be refused, not priced as if 1 month of the term remained
// (issue #98: a $36k 3-year RI would otherwise be listed at ~$950).
func TestMarketplaceList_ZeroTimestampReturnsError(t *testing.T) {
	cfgStore := &MockConfigStore{}
	authSvc := &MockAuthService{}
	adminSession(t, authSvc)

	row := standardRow()
	row.Timestamp = time.Time{}
	cfgStore.On("GetPurchaseHistoryByPurchaseID", mock.Anything, validMarketplacePurchaseID).
		Return(row, nil)

	ec2 := &stubMarketplaceEC2{}
	h := newMarketplaceHandler(cfgStore, authSvc, ec2)
	_, err := h.marketplaceList(context.Background(), marketplaceReq(), validMarketplacePurchaseID)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no timestamp")
	assert.Zero(t, ec2.createCallCount, "no listing may be created for an unpriceable RI")
	cfgStore.AssertExpectations(t)
}
