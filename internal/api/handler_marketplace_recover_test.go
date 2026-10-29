package api

// Tests for the issue #525 failure modes the cancel endpoint and the list
// path's record step close: a pending row left by a crashed or concurrent list
// attempt, a cancel whose DB write failed, an empty AWS status, and the
// detached context of the rollback release. They drive HandleRequest against
// the in-memory row and fake AWS of handler_marketplace_attempt_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	ec2svc "github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/ec2"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func (a *fakeMarketplaceAWS) describe(_ context.Context, id string) (ec2svc.MarketplaceListingResult, error) {
	for _, l := range a.listings {
		if l.id == id {
			return ec2svc.MarketplaceListingResult{ListingID: l.id, State: l.state}, nil
		}
	}
	return ec2svc.MarketplaceListingResult{}, errors.New("InvalidReservedInstancesListingId.NotFound")
}

func (f *fakeListingDB) GetMarketplaceListingAttempt(_ context.Context, _ string) (*config.MarketplaceListingAttempt, error) {
	return &config.MarketplaceListingAttempt{ListingID: f.row.ListingID, State: f.row.ListingState, ClientToken: f.token}, nil
}

// ResolveMarketplaceListingAttempt follows the Postgres store: it writes only
// while the row is as seen or already holds listingID.
func (f *fakeListingDB) ResolveMarketplaceListingAttempt(_ context.Context, _ string, seen config.MarketplaceListingAttempt, listingID, state string) (bool, error) {
	asSeen := strings.EqualFold(f.row.ListingState, seen.State) && f.row.ListingID == seen.ListingID && f.token == seen.ClientToken
	if !asSeen && (listingID == "" || f.row.ListingID != listingID) {
		return false, nil
	}
	f.row.ListingID, f.row.ListingState = listingID, state
	f.token, f.schedule = "", nil
	return true, nil
}

func marketplaceCancelHTTPRequest() *events.LambdaFunctionURLRequest {
	req := marketplaceListHTTPRequest()
	req.RequestContext.HTTP.Path = "/api/purchases/" + validMarketplacePurchaseID + "/marketplace-cancel"
	return req
}

// cancel drives POST /marketplace-cancel through HandleRequest and returns the
// status and the decoded body.
func (f *marketplaceFixture) cancel(t *testing.T) (int, map[string]string) {
	t.Helper()
	resp, err := f.h.HandleRequest(context.Background(), marketplaceCancelHTTPRequest())
	require.NoError(t, err)
	body := map[string]string{}
	if resp.StatusCode == 200 {
		require.NoError(t, json.Unmarshal([]byte(resp.Body), &body), resp.Body)
	}
	return resp.StatusCode, body
}

// crashAfterCreate leaves the row the way a process that died right after the
// AWS create leaves it: pending, holding the attempt's token, with a live
// listing on AWS that nothing recorded.
func (f *marketplaceFixture) crashAfterCreate(t *testing.T) {
	t.Helper()
	f.db.failRecords = 2
	f.aws.failCancel = true
	require.Equal(t, 502, f.list(t))
	require.Equal(t, config.ListingStatePending, f.db.row.ListingState)
	require.NotEmpty(t, f.db.token)
	require.Equal(t, 1, f.aws.live())
	f.aws.failCancel = false
}

// Failure mode 3 of issue #525: a pending row could not be canceled or
// recovered through the API (409 on main). A crash between the AWS create and
// the DB write leaves exactly such a row with a live listing on AWS. Cancel
// finds the listing by the persisted token, cancels it and records it.
func TestMarketplaceCancelHTTP_RecoversCrashedAttemptWithLiveListing(t *testing.T) {
	f := newMarketplaceFixture(t)
	f.crashAfterCreate(t)

	status, body := f.cancel(t)
	require.Equal(t, 200, status)
	assert.Equal(t, "ril-1", body["listing_id"])
	assert.Equal(t, config.ListingStateCancelled, body["listing_state"])
	assert.Equal(t, 0, f.aws.live(), "the crashed attempt's listing is canceled on AWS")
	assert.Equal(t, config.ListingStateCancelled, f.db.row.ListingState)
	assert.Equal(t, "ril-1", f.db.row.ListingID)
	assert.Empty(t, f.db.token, "the attempt is resolved")

	assert.Equal(t, 200, f.list(t), "the RI can be listed again")
	assert.Equal(t, "ril-2", f.db.row.ListingID)
	assert.Equal(t, 1, f.aws.live())
}

// A pending attempt whose create never reached AWS: cancel finds nothing,
// frees the row and keeps the token, so the next list request resumes the
// same attempt and AWS still deduplicates on it.
func TestMarketplaceCancelHTTP_PendingAttemptWithNothingOnAWSIsReleased(t *testing.T) {
	f := newMarketplaceFixture(t)
	f.db.row.ListingState = config.ListingStatePending
	f.db.token = "tok-crashed"
	f.db.schedule = []byte(`[{"term_months":36,"price":380}]`)

	status, body := f.cancel(t)
	require.Equal(t, 200, status)
	assert.Empty(t, body["listing_state"])
	assert.Contains(t, body["note"], "nothing was canceled")
	assert.Empty(t, f.db.row.ListingState, "the row is no longer stuck pending")
	assert.Equal(t, "tok-crashed", f.db.token, "the attempt token is kept")

	assert.Equal(t, 200, f.list(t))
	require.Len(t, f.aws.creates, 1)
	assert.Equal(t, "tok-crashed", f.aws.creates[0].ClientToken, "the next list resumes the same attempt")
	assert.Len(t, f.aws.listings, 1)
}

// A listing recorded in AWS's own pending state (or after its rollback cancel
// failed) has no token; cancel cancels the recorded listing.
func TestMarketplaceCancelHTTP_PendingRecordedListingIsCanceled(t *testing.T) {
	f := newMarketplaceFixture(t)
	f.aws.listings = []*fakeAWSListing{{id: "ril-p", state: config.ListingStatePending, token: "old"}}
	f.db.row.ListingID, f.db.row.ListingState = "ril-p", config.ListingStatePending

	status, body := f.cancel(t)
	require.Equal(t, 200, status)
	assert.Equal(t, config.ListingStateCancelled, body["listing_state"])
	assert.Equal(t, config.ListingStateCancelled, f.aws.listings[0].state)
	assert.Equal(t, config.ListingStateCancelled, f.db.row.ListingState)
	assert.Equal(t, "ril-p", f.db.row.ListingID)
}

// A pending claim from before migration 000105 has neither a token nor a
// listing; nothing on AWS can be tied to it, so cancel clears it.
func TestMarketplaceCancelHTTP_LegacyPendingClaimIsCleared(t *testing.T) {
	f := newMarketplaceFixture(t)
	f.db.row.ListingState = config.ListingStatePending
	assert.Equal(t, 409, f.list(t), "a legacy pending row blocks listing")

	status, _ := f.cancel(t)
	require.Equal(t, 200, status)
	assert.Empty(t, f.db.row.ListingState)
	assert.Empty(t, f.aws.creates)

	assert.Equal(t, 200, f.list(t))
	assert.Equal(t, 1, f.aws.live())
}

// Failure mode 4: the cancel succeeded on AWS but its DB write failed. The
// retry got AWS's error for canceling a canceled listing (400 on main) and the
// row stayed active forever. The retry now reads the listing back and records
// the canceled state.
func TestMarketplaceCancelHTTP_RetryAfterCancelPersistFailureRecordsIt(t *testing.T) {
	f := newMarketplaceFixture(t)
	require.Equal(t, 200, f.list(t))
	f.db.failRecords = 1

	status, _ := f.cancel(t)
	require.Equal(t, 500, status)
	require.Equal(t, 0, f.aws.live(), "AWS canceled the listing")
	require.Equal(t, config.ListingStateActive, f.db.row.ListingState, "the DB write was lost")

	status, body := f.cancel(t)
	require.Equal(t, 200, status, "the retry must record the cancel, not fail on the canceled listing")
	assert.Equal(t, config.ListingStateCancelled, body["listing_state"])
	assert.Equal(t, config.ListingStateCancelled, f.db.row.ListingState)
}

// Failure mode 5: a create response with an empty status was recorded as an
// empty state on main, which reads as "not listed", so the next request
// created a second listing. The status is read back from AWS instead.
func TestMarketplaceListHTTP_EmptyCreateStatusIsReadBack(t *testing.T) {
	f := newMarketplaceFixture(t)
	f.blankCreateStatus()

	require.Equal(t, 200, f.list(t))
	assert.Equal(t, config.ListingStateActive, f.db.row.ListingState)
	assert.Equal(t, 409, f.list(t), "the live listing blocks a second one")
	assert.Len(t, f.aws.listings, 1)
}

// When the read-back fails too, the listing is recorded as pending: still
// blocking a second listing, and cancelable through the endpoint.
func TestMarketplaceListHTTP_EmptyCreateStatusUnreadableIsPending(t *testing.T) {
	f := newMarketplaceFixture(t)
	f.blankCreateStatus()
	stub := f.stub()
	stub.describeFn = func(context.Context, string) (ec2svc.MarketplaceListingResult, error) {
		return ec2svc.MarketplaceListingResult{}, errors.New("RequestLimitExceeded")
	}

	require.Equal(t, 200, f.list(t))
	assert.Equal(t, config.ListingStatePending, f.db.row.ListingState)
	assert.Equal(t, 409, f.list(t))
	assert.Len(t, f.aws.listings, 1)

	stub.describeFn = f.aws.describe
	status, _ := f.cancel(t)
	require.Equal(t, 200, status)
	assert.Equal(t, 0, f.aws.live())
	assert.Equal(t, config.ListingStateCancelled, f.db.row.ListingState)
}

// A cancel that lands while a list request is between its AWS create and its
// DB write cancels the listing it finds by the token. The list request's
// write must not then record the canceled listing as active.
func TestMarketplaceListHTTP_CancelDuringListIsNotUndone(t *testing.T) {
	f := newMarketplaceFixture(t)
	stub := f.stub()
	create := stub.createFn
	var cancelStatus int
	stub.createFn = func(ctx context.Context, req ec2svc.MarketplaceListingRequest) (ec2svc.MarketplaceListingResult, error) {
		res, err := create(ctx, req)
		cancelStatus, _ = f.cancel(t)
		return res, err
	}

	assert.Equal(t, 409, f.list(t), "the list request reports the concurrent cancel")
	assert.Equal(t, 200, cancelStatus)
	assert.Equal(t, 0, f.aws.live())
	assert.Equal(t, config.ListingStateCancelled, f.db.row.ListingState, "the canceled listing must not be recorded as active")
	assert.Equal(t, 1, stub.cancelCallCount, "no compensating cancel for a listing already canceled")
}

func (f *marketplaceFixture) stub() *stubMarketplaceEC2 {
	s, ok := f.h.buildMarketplaceEC2Client(aws.Config{}).(*stubMarketplaceEC2)
	if !ok {
		panic("marketplace fixture without a stub EC2 client")
	}
	return s
}

func (f *marketplaceFixture) blankCreateStatus() {
	s := f.stub()
	create := s.createFn
	s.createFn = func(ctx context.Context, req ec2svc.MarketplaceListingRequest) (ec2svc.MarketplaceListingResult, error) {
		res, err := create(ctx, req)
		res.State = ""
		return res, err
	}
}

// Failure mode 6: after the rollback cancel succeeded, the release that runs
// when recording the canceled listing fails used the request context, which
// is gone when the request timed out.
func TestMarketplaceList_RollbackReleaseUsesDetachedContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfgStore := &MockConfigStore{}
	authSvc := &MockAuthService{}
	adminSession(t, authSvc)
	cfgStore.On("GetPurchaseHistoryByPurchaseID", mock.Anything, validMarketplacePurchaseID).Return(standardRow(), nil)
	cfgStore.On("ClaimMarketplaceListingSlot", mock.Anything, validMarketplacePurchaseID, mock.Anything, mock.Anything).
		Return(&config.MarketplaceListingClaim{ClientToken: "tok"}, nil)
	cfgStore.On("UpdatePurchaseHistoryListing", mock.Anything, validMarketplacePurchaseID, "ril-default", config.ListingStateActive).
		Run(func(mock.Arguments) { cancel() }).Return(errors.New("db down"))
	cfgStore.On("UpdatePurchaseHistoryListing", mock.Anything, validMarketplacePurchaseID, "ril-default", config.ListingStateCancelled).
		Return(errors.New("db still down"))
	var releaseCtxErr error
	cfgStore.On("ReleaseMarketplaceListingClaim", mock.Anything, validMarketplacePurchaseID, "tok", "", false).
		Run(func(args mock.Arguments) {
			c, _ := args.Get(0).(context.Context)
			releaseCtxErr = c.Err()
		}).Return(nil).Once()

	h := newMarketplaceHandler(cfgStore, authSvc, &stubMarketplaceEC2{})
	_, err := h.marketplaceList(ctx, marketplaceReq(), validMarketplacePurchaseID)
	require.Error(t, err)
	cfgStore.AssertExpectations(t)
	assert.NoError(t, releaseCtxErr, "the release must not run on the canceled request context")
}

// The cancel's DB write runs on a detached context: AWS has already canceled
// the listing, so a caller that disconnects must not lose the record.
func TestMarketplaceCancel_RecordsOnDetachedContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfgStore := &MockConfigStore{}
	authSvc := &MockAuthService{}
	adminSession(t, authSvc)
	row := standardRow()
	row.ListingID, row.ListingState = "ril-cancel", config.ListingStateActive
	cfgStore.On("GetPurchaseHistoryByPurchaseID", mock.Anything, validMarketplacePurchaseID).Return(row, nil)
	var recordCtxErr error
	cfgStore.On("UpdatePurchaseHistoryListing", mock.Anything, validMarketplacePurchaseID, "ril-cancel", config.ListingStateCancelled).
		Run(func(args mock.Arguments) {
			c, _ := args.Get(0).(context.Context)
			recordCtxErr = c.Err()
		}).Return(nil).Once()

	ec2 := &stubMarketplaceEC2{cancelFn: func(_ context.Context, id string) (ec2svc.MarketplaceListingResult, error) {
		cancel() // the caller goes away while AWS cancels
		return ec2svc.MarketplaceListingResult{ListingID: id, State: config.ListingStateCancelled}, nil
	}}
	h := newMarketplaceHandler(cfgStore, authSvc, ec2)
	_, err := h.marketplaceCancel(ctx, marketplaceReq(), validMarketplacePurchaseID)
	require.NoError(t, err)
	cfgStore.AssertExpectations(t)
	assert.NoError(t, recordCtxErr)
}

// A cancel that AWS accepts without a status records it as canceled, never as
// an empty state that reads as "not listed".
func TestMarketplaceCancel_EmptyStatusRecordedAsCanceled(t *testing.T) {
	cfgStore := &MockConfigStore{}
	authSvc := &MockAuthService{}
	adminSession(t, authSvc)
	row := standardRow()
	row.ListingID, row.ListingState = "ril-cancel", config.ListingStateActive
	cfgStore.On("GetPurchaseHistoryByPurchaseID", mock.Anything, validMarketplacePurchaseID).Return(row, nil)
	cfgStore.On("UpdatePurchaseHistoryListing", mock.Anything, validMarketplacePurchaseID, "ril-cancel", config.ListingStateCancelled).
		Return(nil).Once()

	ec2 := &stubMarketplaceEC2{cancelFn: func(_ context.Context, id string) (ec2svc.MarketplaceListingResult, error) {
		return ec2svc.MarketplaceListingResult{ListingID: id}, nil
	}}
	h := newMarketplaceHandler(cfgStore, authSvc, ec2)
	_, err := h.marketplaceCancel(context.Background(), marketplaceReq(), validMarketplacePurchaseID)
	require.NoError(t, err)
	cfgStore.AssertExpectations(t)
}

// A cancel failure that is not about an ended listing still fails, and a
// still-active listing is not recorded as canceled.
func TestMarketplaceCancel_FailureOnLiveListingIsNotRecorded(t *testing.T) {
	cfgStore := &MockConfigStore{}
	t.Cleanup(func() { cfgStore.AssertExpectations(t) })
	authSvc := &MockAuthService{}
	adminSession(t, authSvc)
	row := standardRow()
	row.ListingID, row.ListingState = "ril-cancel", config.ListingStateActive
	cfgStore.On("GetPurchaseHistoryByPurchaseID", mock.Anything, validMarketplacePurchaseID).Return(row, nil)

	ec2 := &stubMarketplaceEC2{cancelFn: func(context.Context, string) (ec2svc.MarketplaceListingResult, error) {
		return ec2svc.MarketplaceListingResult{}, errors.New("RequestLimitExceeded")
	}}
	h := newMarketplaceHandler(cfgStore, authSvc, ec2)
	_, err := h.marketplaceCancel(context.Background(), marketplaceReq(), validMarketplacePurchaseID)
	ce, ok := IsClientError(err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, 502, ce.code)
	assert.Equal(t, 1, ec2.describeCallCount)
	cfgStore.AssertNotCalled(t, "UpdatePurchaseHistoryListing", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// A pending row that changed between the purchase lookup and the attempt read
// is not touched.
func TestMarketplaceCancel_PendingRowThatChangedIs409(t *testing.T) {
	cfgStore := &MockConfigStore{}
	t.Cleanup(func() { cfgStore.AssertExpectations(t) })
	authSvc := &MockAuthService{}
	adminSession(t, authSvc)
	row := standardRow()
	row.ListingState = config.ListingStatePending
	cfgStore.On("GetPurchaseHistoryByPurchaseID", mock.Anything, validMarketplacePurchaseID).Return(row, nil)
	cfgStore.On("GetMarketplaceListingAttempt", mock.Anything, validMarketplacePurchaseID).
		Return(&config.MarketplaceListingAttempt{ListingID: "ril-new", State: config.ListingStateActive}, nil)

	ec2 := &stubMarketplaceEC2{}
	h := newMarketplaceHandler(cfgStore, authSvc, ec2)
	_, err := h.marketplaceCancel(context.Background(), marketplaceReq(), validMarketplacePurchaseID)
	ce, ok := IsClientError(err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, 409, ce.code)
	assert.Zero(t, ec2.cancelCallCount)
}

// The recovery write is fenced: if the row moved on (another request recorded
// a different listing), cancel reports a conflict instead of overwriting it.
func TestMarketplaceCancel_FencedRecoveryWriteLosesIs409(t *testing.T) {
	cfgStore := &MockConfigStore{}
	authSvc := &MockAuthService{}
	adminSession(t, authSvc)
	row := standardRow()
	row.ListingState = config.ListingStatePending
	seen := config.MarketplaceListingAttempt{State: config.ListingStatePending}
	cfgStore.On("GetPurchaseHistoryByPurchaseID", mock.Anything, validMarketplacePurchaseID).Return(row, nil)
	cfgStore.On("GetMarketplaceListingAttempt", mock.Anything, validMarketplacePurchaseID).Return(&seen, nil)
	cfgStore.On("ResolveMarketplaceListingAttempt", mock.Anything, validMarketplacePurchaseID, seen, "", "").Return(false, nil).Once()

	h := newMarketplaceHandler(cfgStore, authSvc, &stubMarketplaceEC2{})
	_, err := h.marketplaceCancel(context.Background(), marketplaceReq(), validMarketplacePurchaseID)
	ce, ok := IsClientError(err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, 409, ce.code)
	cfgStore.AssertExpectations(t)
}

// The record of a listing whose rollback cancel failed loses to a cancel that
// recorded the listing canceled meanwhile: that is reported as the concurrent
// cancel, not as a row stuck pending.
func TestMarketplaceList_UncanceledRecordAfterConcurrentCancelIs409(t *testing.T) {
	cfgStore := &MockConfigStore{}
	authSvc := &MockAuthService{}
	adminSession(t, authSvc)
	cfgStore.On("GetPurchaseHistoryByPurchaseID", mock.Anything, validMarketplacePurchaseID).Return(standardRow(), nil)
	cfgStore.On("ClaimMarketplaceListingSlot", mock.Anything, validMarketplacePurchaseID, mock.Anything, mock.Anything).
		Return(&config.MarketplaceListingClaim{ClientToken: "tok"}, nil)
	cfgStore.On("UpdatePurchaseHistoryListing", mock.Anything, validMarketplacePurchaseID, "ril-default", config.ListingStateActive).
		Return(errors.New("db down")).Once()
	cfgStore.On("UpdatePurchaseHistoryListing", mock.Anything, validMarketplacePurchaseID, "ril-default", config.ListingStateActive).
		Return(config.ErrMarketplaceListingEnded).Once()

	ec2 := &stubMarketplaceEC2{cancelFn: func(context.Context, string) (ec2svc.MarketplaceListingResult, error) {
		return ec2svc.MarketplaceListingResult{}, errors.New("RequestLimitExceeded")
	}}
	h := newMarketplaceHandler(cfgStore, authSvc, ec2)
	_, err := h.marketplaceList(context.Background(), marketplaceReq(), validMarketplacePurchaseID)
	ce, ok := IsClientError(err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, 409, ce.code)
	assert.Contains(t, ce.message, "canceled by another request")
	cfgStore.AssertExpectations(t)
}
