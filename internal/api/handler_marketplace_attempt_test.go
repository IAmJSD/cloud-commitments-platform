package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	ec2svc "github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/ec2"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	sdkec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	sdkec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	smithy "github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// fakeListingDB is an in-memory purchase_history row that follows the claim,
// release and record semantics of the Postgres store, so the tests below drive
// the handler through several requests against one evolving row.
type fakeListingDB struct {
	*MockConfigStore
	row      config.PurchaseHistoryRecord
	token    string
	schedule []byte
	// failRecords makes the next N UpdatePurchaseHistoryListing calls fail.
	failRecords int
	// staleOnce, when set, is returned by the next GetPurchaseHistoryByPurchaseID.
	staleOnce *config.PurchaseHistoryRecord
}

func newFakeListingDB() *fakeListingDB {
	return &fakeListingDB{MockConfigStore: &MockConfigStore{}, row: *standardRow()}
}

func (f *fakeListingDB) GetPurchaseHistoryByPurchaseID(_ context.Context, _ string) (*config.PurchaseHistoryRecord, error) {
	if f.staleOnce != nil {
		stale := *f.staleOnce
		f.staleOnce = nil
		return &stale, nil
	}
	cp := f.row
	return &cp, nil
}

func (f *fakeListingDB) ClaimMarketplaceListingSlot(_ context.Context, _, clientToken string, priceSchedule []byte) (*config.MarketplaceListingClaim, error) {
	state := strings.ToLower(f.row.ListingState)
	if state == config.ListingStateActive || (state == config.ListingStatePending && f.token == "") {
		return nil, nil
	}
	resumed := f.token != ""
	if !resumed {
		f.token, f.schedule = clientToken, priceSchedule
	}
	prior := f.row.ListingState
	f.row.ListingState = config.ListingStatePending
	return &config.MarketplaceListingClaim{ListingID: f.row.ListingID, PriorState: prior, ClientToken: f.token, PriceSchedule: f.schedule, Resumed: resumed}, nil
}

func (f *fakeListingDB) ReleaseMarketplaceListingClaim(_ context.Context, _, clientToken, priorState string, keepAttempt bool) error {
	if strings.EqualFold(f.row.ListingState, config.ListingStatePending) && f.token == clientToken {
		f.row.ListingState = priorState
		if !keepAttempt {
			f.token, f.schedule = "", nil
		}
	}
	return nil
}

func (f *fakeListingDB) UpdatePurchaseHistoryListing(_ context.Context, _, listingID, state string) error {
	if f.failRecords > 0 {
		f.failRecords--
		return errors.New("db down")
	}
	if f.row.ListingID == listingID && isDeadListingState(f.row.ListingState) && !isDeadListingState(state) {
		return config.ErrMarketplaceListingEnded
	}
	f.row.ListingID, f.row.ListingState = listingID, state
	f.token, f.schedule = "", nil
	return nil
}

// recordCanceled marks a listing canceled in the row, like the cancel handler.
func (f *fakeListingDB) recordCanceled(listingID string) {
	f.row.ListingID, f.row.ListingState = listingID, config.ListingStateCancelled
}

type fakeAWSListing struct {
	id, state, token string
	count            int32
	schedule         []ec2svc.MarketplacePriceTier
}

// fakeMarketplaceAWS dedups CreateReservedInstancesListing on ClientToken like
// the real API: a repeat returns the existing listing (even a canceled one) and
// a repeat with different parameters is IdempotentParameterMismatch.
type fakeMarketplaceAWS struct {
	listings []*fakeAWSListing
	// loseResponses makes the next N creates succeed on AWS but fail for the caller.
	loseResponses int
	failCancel    bool
	creates       []ec2svc.MarketplaceListingRequest
	finds         int
}

func (a *fakeMarketplaceAWS) byToken(token string) *fakeAWSListing {
	for _, l := range a.listings {
		if l.token == token {
			return l
		}
	}
	return nil
}

func (a *fakeMarketplaceAWS) create(_ context.Context, req ec2svc.MarketplaceListingRequest) (ec2svc.MarketplaceListingResult, error) {
	a.creates = append(a.creates, req)
	l := a.byToken(req.ClientToken)
	switch {
	case l == nil:
		l = &fakeAWSListing{id: fmt.Sprintf("ril-%d", len(a.listings)+1), state: config.ListingStateActive, token: req.ClientToken, count: req.InstanceCount, schedule: req.PriceSchedule}
		a.listings = append(a.listings, l)
	case l.count != req.InstanceCount || fmt.Sprint(l.schedule) != fmt.Sprint(req.PriceSchedule):
		return ec2svc.MarketplaceListingResult{}, errors.New("IdempotentParameterMismatch")
	}
	if a.loseResponses > 0 {
		a.loseResponses--
		return ec2svc.MarketplaceListingResult{}, errors.New("RequestTimeout: response lost")
	}
	return ec2svc.MarketplaceListingResult{ListingID: l.id, State: l.state}, nil
}

func (a *fakeMarketplaceAWS) cancel(_ context.Context, id string) (ec2svc.MarketplaceListingResult, error) {
	if a.failCancel {
		return ec2svc.MarketplaceListingResult{}, errors.New("RequestLimitExceeded")
	}
	for _, l := range a.listings {
		if l.id == id {
			if isDeadListingState(l.state) {
				// Like AWS, a listing that already ended cannot be canceled.
				return ec2svc.MarketplaceListingResult{}, &marketplaceTestAPIError{code: "IncorrectState", message: "listing " + id + " is " + l.state, fault: smithy.FaultClient}
			}
			l.state = config.ListingStateCancelled
		}
	}
	return ec2svc.MarketplaceListingResult{ListingID: id, State: config.ListingStateCancelled}, nil
}

func (a *fakeMarketplaceAWS) FindMarketplaceListingByToken(_ context.Context, _, token string) (ec2svc.MarketplaceListingResult, bool, error) {
	a.finds++
	l := a.byToken(token)
	if l == nil {
		return ec2svc.MarketplaceListingResult{}, false, nil
	}
	return ec2svc.MarketplaceListingResult{ListingID: l.id, State: l.state}, true, nil
}

func (a *fakeMarketplaceAWS) DescribeMarketplaceListingStatus(_ context.Context, id string) (marketplaceListingStatus, error) {
	for _, l := range a.listings {
		if l.id == id {
			return marketplaceListingStatus{ListingID: l.id, State: l.state}, nil
		}
	}
	return marketplaceListingStatus{}, errMarketplaceListingNotFound
}

func (a *fakeMarketplaceAWS) live() int {
	n := 0
	for _, l := range a.listings {
		if l.state == config.ListingStateActive {
			n++
		}
	}
	return n
}

type marketplaceFixture struct {
	h   *Handler
	db  *fakeListingDB
	aws *fakeMarketplaceAWS
}

func newMarketplaceFixture(t *testing.T) *marketplaceFixture {
	t.Helper()
	db := newFakeListingDB()
	authSvc := &MockAuthService{}
	adminSession(authSvc)
	authSvc.On("ValidateCSRFToken", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	fake := &fakeMarketplaceAWS{}
	// The token and schedule must be on the row before AWS sees the request.
	create := func(ctx context.Context, req ec2svc.MarketplaceListingRequest) (ec2svc.MarketplaceListingResult, error) {
		require.Equal(t, db.token, req.ClientToken, "the token must be persisted before the AWS call")
		var stored []MarketplacePriceTier
		require.NoError(t, json.Unmarshal(db.schedule, &stored), "the schedule must be persisted before the AWS call")
		require.Equal(t, toAWSPriceSchedule(stored), req.PriceSchedule)
		return fake.create(ctx, req)
	}
	stub := &stubMarketplaceEC2{createFn: create, cancelFn: fake.cancel, describeFn: fake.describe}
	h := newMarketplaceHandler(db.MockConfigStore, authSvc, stub)
	h.config = db
	h.marketplaceFinderFactory = func(aws.Config) marketplaceListingFinder { return fake }
	return &marketplaceFixture{h: h, db: db, aws: fake}
}

// list drives POST /marketplace-list through HandleRequest and returns the status.
func (f *marketplaceFixture) list(t *testing.T) int {
	t.Helper()
	resp, err := f.h.HandleRequest(context.Background(), marketplaceListHTTPRequest())
	require.NoError(t, err)
	return resp.StatusCode
}

// monthsLater moves the purchase back one month and a bit so the remaining
// months, and with them the default price schedule, change on the next request.
func (f *marketplaceFixture) monthsLater(t *testing.T) {
	t.Helper()
	before, err := computeRemainingMonths(f.db.row.Timestamp, f.db.row.Term*12, time.Now())
	require.NoError(t, err)
	f.db.row.Timestamp = f.db.row.Timestamp.Add(-731 * time.Hour)
	after, err := computeRemainingMonths(f.db.row.Timestamp, f.db.row.Term*12, time.Now())
	require.NoError(t, err)
	require.NotEqual(t, before, after, "the clock must cross a month boundary")
}

// Issue #525: the first attempt created the listing on AWS but the response was
// lost; the operator retries after the remaining months changed (a month
// boundary), so the default schedule differs. The retry must not create a
// second listing.
func TestMarketplaceListHTTP_RetryAcrossMonthBoundaryDoesNotDuplicate(t *testing.T) {
	f := newMarketplaceFixture(t)
	f.aws.loseResponses = 1

	assert.Equal(t, 502, f.list(t), "the lost response is reported")
	require.Len(t, f.aws.listings, 1)
	firstSchedule := f.aws.creates[0].PriceSchedule
	f.monthsLater(t)

	assert.Equal(t, 200, f.list(t), "the retry reconciles the listing the first attempt created")
	assert.Len(t, f.aws.listings, 1, "AWS must hold exactly one listing")
	assert.Equal(t, 1, f.aws.live())
	assert.Equal(t, "ril-1", f.db.row.ListingID)
	assert.Equal(t, config.ListingStateActive, f.db.row.ListingState)
	assert.Empty(t, f.db.token, "recording the listing resolves the attempt")
	assert.Equal(t, 1, f.aws.finds)
	assert.Equal(t, firstSchedule, f.aws.creates[0].PriceSchedule)
}

// The same retry when AWS has no listing for the token (the first create never
// reached AWS): the retry creates it with the persisted token and schedule, not
// ones re-derived from the clock.
func TestMarketplaceListHTTP_RetryReusesPersistedTokenAndSchedule(t *testing.T) {
	f := newMarketplaceFixture(t)
	// The first create never reaches AWS: fail before the fake records a listing.
	calls := 0
	f.h.marketplaceEC2Factory = func(aws.Config) marketplaceEC2Client {
		return &stubMarketplaceEC2{createFn: func(ctx context.Context, req ec2svc.MarketplaceListingRequest) (ec2svc.MarketplaceListingResult, error) {
			calls++
			if calls == 1 {
				f.aws.creates = append(f.aws.creates, req)
				return ec2svc.MarketplaceListingResult{}, errors.New("connection reset")
			}
			return f.aws.create(ctx, req)
		}}
	}

	assert.Equal(t, 502, f.list(t))
	require.Len(t, f.aws.creates, 1)
	f.monthsLater(t)

	assert.Equal(t, 200, f.list(t))
	require.Len(t, f.aws.creates, 2)
	assert.Equal(t, f.aws.creates[0].ClientToken, f.aws.creates[1].ClientToken, "the retry must resend the persisted token")
	assert.Equal(t, f.aws.creates[0].PriceSchedule, f.aws.creates[1].PriceSchedule, "the retry must resend the persisted schedule, not one re-derived from the clock")
	assert.Len(t, f.aws.listings, 1)
}

// A crash after the AWS create leaves the row pending with the token. The
// persist step fails and so does the compensating cancel (and the record of the
// uncanceled listing), as in a process that dies right there. The next request
// resumes the pending row, finds the listing on AWS and records it.
func TestMarketplaceListHTTP_CrashBetweenCreateAndPersistIsReconciled(t *testing.T) {
	f := newMarketplaceFixture(t)
	f.db.failRecords = 2
	f.aws.failCancel = true

	assert.Equal(t, 502, f.list(t))
	assert.Equal(t, config.ListingStatePending, f.db.row.ListingState, "the crashed attempt leaves the claim in place")
	assert.NotEmpty(t, f.db.token)
	f.monthsLater(t)

	assert.Equal(t, 200, f.list(t))
	assert.Len(t, f.aws.listings, 1)
	assert.Equal(t, 1, f.aws.live())
	assert.Equal(t, "ril-1", f.db.row.ListingID)
	assert.Equal(t, config.ListingStateActive, f.db.row.ListingState)
}

// Reconcile finding a listing that is already dead records it and asks for a
// retry; the next attempt starts with a fresh token.
func TestMarketplaceListHTTP_ReconcileFindsCanceledListing(t *testing.T) {
	f := newMarketplaceFixture(t)
	f.aws.loseResponses = 1
	assert.Equal(t, 502, f.list(t))
	_, err := f.aws.cancel(context.Background(), "ril-1")
	require.NoError(t, err)

	assert.Equal(t, 502, f.list(t), "the dead listing is reported, not replayed as success")
	assert.Equal(t, config.ListingStateCancelled, f.db.row.ListingState)
	assert.Equal(t, "ril-1", f.db.row.ListingID)
	assert.Empty(t, f.db.token)

	assert.Equal(t, 200, f.list(t))
	assert.Len(t, f.aws.listings, 2)
	assert.Equal(t, "ril-2", f.db.row.ListingID)
}

// A definitive AWS rejection created nothing, so its token is dropped and the
// operator's next request (possibly with another schedule) starts fresh.
func TestMarketplaceListHTTP_ClientFaultDropsTheAttempt(t *testing.T) {
	f := newMarketplaceFixture(t)
	f.h.marketplaceEC2Factory = func(aws.Config) marketplaceEC2Client {
		return &stubMarketplaceEC2{createFn: func(context.Context, ec2svc.MarketplaceListingRequest) (ec2svc.MarketplaceListingResult, error) {
			return ec2svc.MarketplaceListingResult{}, &marketplaceTestAPIError{code: "InvalidParameterValue", message: "bad price", fault: smithy.FaultClient}
		}}
	}
	assert.Equal(t, 400, f.list(t))
	assert.Empty(t, f.db.token)
	assert.Empty(t, f.db.row.ListingState)
}

// The row was read before another request claimed, listed and canceled it, so
// the stale copy knows nothing of the canceled listing. The token must not be
// derived from that copy: with a persisted random token the stale request gets
// its own listing instead of AWS replaying the canceled one.
func TestMarketplaceListHTTP_StaleRowAfterCancelGetsNewListing(t *testing.T) {
	f := newMarketplaceFixture(t)
	stale := f.db.row
	assert.Equal(t, 200, f.list(t))
	_, err := f.aws.cancel(context.Background(), "ril-1")
	require.NoError(t, err)
	f.db.recordCanceled("ril-1")

	f.db.staleOnce = &stale
	assert.Equal(t, 200, f.list(t), "the stale request must create its own listing")
	assert.Len(t, f.aws.listings, 2)
	assert.Equal(t, "ril-2", f.db.row.ListingID)
	assert.Equal(t, 1, f.aws.live())
}

// After a persist failure and a successful compensating cancel the canceled
// listing is recorded and the token dropped, so the retry creates a new one.
func TestMarketplaceListHTTP_RetryAfterRolledBackPersistFailureGetsNewListing(t *testing.T) {
	f := newMarketplaceFixture(t)
	f.db.failRecords = 1

	assert.Equal(t, 500, f.list(t))
	assert.Equal(t, config.ListingStateCancelled, f.db.row.ListingState)
	assert.Empty(t, f.db.token)

	assert.Equal(t, 200, f.list(t))
	assert.Equal(t, "ril-2", f.db.row.ListingID)
	assert.Equal(t, 1, f.aws.live())
}

// A create response in a dead state is an error and the dead listing is recorded.
func TestMarketplaceListHTTP_ReturnedCanceledStateIsAnError(t *testing.T) {
	f := newMarketplaceFixture(t)
	f.h.marketplaceEC2Factory = func(aws.Config) marketplaceEC2Client {
		return &stubMarketplaceEC2{createFn: func(context.Context, ec2svc.MarketplaceListingRequest) (ec2svc.MarketplaceListingResult, error) {
			return ec2svc.MarketplaceListingResult{ListingID: "ril-dead", State: config.ListingStateCancelled}, nil
		}}
	}
	assert.Equal(t, 502, f.list(t))
	assert.Equal(t, "ril-dead", f.db.row.ListingID)
	assert.Empty(t, f.db.token)
}

func TestMarketplaceListHTTP_ReconcileLookupFailureKeepsTheAttempt(t *testing.T) {
	f := newMarketplaceFixture(t)
	f.aws.loseResponses = 1
	assert.Equal(t, 502, f.list(t))
	token := f.db.token
	f.h.marketplaceFinderFactory = func(aws.Config) marketplaceListingFinder { return failingFinder{} }

	assert.Equal(t, 502, f.list(t))
	assert.Equal(t, token, f.db.token, "an unresolved attempt keeps its token")
	assert.Len(t, f.aws.creates, 1, "no create while the earlier attempt is unresolved")
	assert.NotEqual(t, config.ListingStatePending, f.db.row.ListingState)
}

type failingFinder struct{}

func (failingFinder) FindMarketplaceListingByToken(context.Context, string, string) (ec2svc.MarketplaceListingResult, bool, error) {
	return ec2svc.MarketplaceListingResult{}, false, errors.New("RequestLimitExceeded")
}

func (failingFinder) DescribeMarketplaceListingStatus(context.Context, string) (marketplaceListingStatus, error) {
	return marketplaceListingStatus{}, errors.New("RequestLimitExceeded")
}

func TestMarketplaceListHTTP_CorruptStoredScheduleFailsLoud(t *testing.T) {
	f := newMarketplaceFixture(t)
	f.db.token, f.db.schedule = "tok", []byte("[]")
	f.db.row.ListingState = config.ListingStatePending

	assert.Equal(t, 500, f.list(t))
	assert.Empty(t, f.aws.creates)
	assert.Equal(t, "tok", f.db.token)
}

// The schedule is stored as JSON that decodes back to the same tiers.
func TestMarketplaceListHTTP_PersistsTheSchedule(t *testing.T) {
	f := newMarketplaceFixture(t)
	f.aws.loseResponses = 1
	assert.Equal(t, 502, f.list(t))

	var stored []MarketplacePriceTier
	require.NoError(t, json.Unmarshal(f.db.schedule, &stored))
	assert.Equal(t, toAWSPriceSchedule(stored), f.aws.creates[0].PriceSchedule)
	assert.Equal(t, f.aws.creates[0].ClientToken, f.db.token)
}

type fakeDescribeListings struct {
	out []sdkec2types.ReservedInstancesListing
	err error
	in  *sdkec2.DescribeReservedInstancesListingsInput
}

func (f *fakeDescribeListings) DescribeReservedInstancesListings(_ context.Context, in *sdkec2.DescribeReservedInstancesListingsInput, _ ...func(*sdkec2.Options)) (*sdkec2.DescribeReservedInstancesListingsOutput, error) {
	f.in = in
	return &sdkec2.DescribeReservedInstancesListingsOutput{ReservedInstancesListings: f.out}, f.err
}

func TestAWSMarketplaceListingFinder(t *testing.T) {
	api := &fakeDescribeListings{out: []sdkec2types.ReservedInstancesListing{
		{ClientToken: aws.String("other"), ReservedInstancesListingId: aws.String("ril-other"), Status: sdkec2types.ListingStatusActive},
		{ClientToken: aws.String("tok"), ReservedInstancesListingId: aws.String("ril-mine"), Status: sdkec2types.ListingStatusCancelled},
	}}
	finder := awsMarketplaceListingFinder{api: api}

	got, found, err := finder.FindMarketplaceListingByToken(context.Background(), "ri-1", "tok")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, ec2svc.MarketplaceListingResult{ListingID: "ril-mine", State: string(sdkec2types.ListingStatusCancelled)}, got)
	assert.Equal(t, "ri-1", aws.ToString(api.in.ReservedInstancesId))

	_, found, err = finder.FindMarketplaceListingByToken(context.Background(), "ri-1", "missing")
	require.NoError(t, err)
	assert.False(t, found)

	api.err = errors.New("boom")
	_, _, err = finder.FindMarketplaceListingByToken(context.Background(), "ri-1", "tok")
	require.Error(t, err)
}
