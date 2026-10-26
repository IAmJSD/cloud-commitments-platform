package api

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	ec2svc "github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/ec2"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	sdkec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	sdkec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// fakeListingStatusReader answers DescribeMarketplaceListingStatus from a map,
// standing in for DescribeReservedInstancesListings. No AWS call is made.
type fakeListingStatusReader struct {
	statuses map[string]marketplaceListingStatus
	errs     map[string]error
	reads    []string
}

func (f *fakeListingStatusReader) FindMarketplaceListingByToken(context.Context, string, string) (ec2svc.MarketplaceListingResult, bool, error) {
	return ec2svc.MarketplaceListingResult{}, false, errors.New("the poller must not look listings up by token")
}

func (f *fakeListingStatusReader) DescribeMarketplaceListingStatus(_ context.Context, listingID string) (marketplaceListingStatus, error) {
	f.reads = append(f.reads, listingID)
	if err := f.errs[listingID]; err != nil {
		return marketplaceListingStatus{}, err
	}
	st, ok := f.statuses[listingID]
	if !ok {
		return marketplaceListingStatus{}, errMarketplaceListingNotFound
	}
	return st, nil
}

type pollerFixture struct {
	h     *Handler
	store *MockConfigStore
	aws   *fakeListingStatusReader
	// built holds the AWS config each describe client was built from.
	built []aws.Config
}

func newPollerFixture(t *testing.T, rows ...config.PurchaseHistoryRecord) *pollerFixture {
	t.Helper()
	return newPollerFixtureWithStore(t, &MockConfigStore{}, rows...)
}

// newPollerFixtureWithStore lets a test keep its own handle on the store, so
// its AssertNotCalled checks go through MockConfigStore's call log.
func newPollerFixtureWithStore(t *testing.T, store *MockConfigStore, rows ...config.PurchaseHistoryRecord) *pollerFixture {
	t.Helper()
	f := &pollerFixture{
		store: store,
		aws:   &fakeListingStatusReader{statuses: map[string]marketplaceListingStatus{}, errs: map[string]error{}},
	}
	f.store.On("ListMarketplaceListingsToPoll", mock.Anything).Return(rows, nil).Maybe()
	f.store.On("GetCloudAccount", mock.Anything, "acct-1").Return(memberAccount(), nil).Maybe()
	f.h = &Handler{
		config:    f.store,
		credStore: &accountKeyStore{},
		marketplaceFinderFactory: func(cfg aws.Config) marketplaceListingFinder {
			f.built = append(f.built, cfg)
			return f.aws
		},
		marketplaceEC2Factory: func(aws.Config) marketplaceEC2Client {
			t.Error("the poller must not build the listing/cancel EC2 client")
			return &stubMarketplaceEC2{}
		},
	}
	f.h.awsCfgOnce.Do(func() {
		f.h.awsCfg = aws.Config{
			Region:      "us-east-1",
			Credentials: awscreds.NewStaticCredentialsProvider(marketplaceHostKeyID, "host-secret", ""),
			HTTPClient:  hostSTSTransport{},
		}
	})
	return f
}

// captureSettle records every settlement written and answers applied.
func (f *pollerFixture) captureSettle(applied bool) *[]config.MarketplaceListingSettlement {
	var got []config.MarketplaceListingSettlement
	f.store.On("SettleMarketplaceListing", mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
		got = append(got, args.Get(1).(config.MarketplaceListingSettlement))
	}).Return(applied, nil)
	return &got
}

func listedRow(purchaseID, listingID string) config.PurchaseHistoryRecord {
	acct := "acct-1"
	return config.PurchaseHistoryRecord{
		AccountID: "acct-aws", PurchaseID: purchaseID, Provider: "aws", Region: "eu-west-1",
		CloudAccountID: &acct, ListingID: listingID, ListingState: config.ListingStateActive,
	}
}

var pollerCreateDate = time.Date(2026, 9, 1, 12, 30, 0, 0, time.UTC)

func TestPollMarketplaceListings_ClosedRecordsProceedsAndFee(t *testing.T) {
	f := newPollerFixture(t, listedRow("ri-1", "ril-1"))
	f.aws.statuses["ril-1"] = marketplaceListingStatus{ListingID: "ril-1", State: config.ListingStateClosed,
		CreatedAt: pollerCreateDate, SoldCount: 2, UnitPrice: 1000, PriceKnown: true}
	settled := f.captureSettle(true)

	res, err := f.h.PollMarketplaceListings(context.Background())

	require.NoError(t, err)
	assert.Equal(t, MarketplacePollResult{Found: 1, Settled: 1}, *res)
	require.Len(t, *settled, 1)
	got := (*settled)[0]
	assert.Equal(t, "ri-1", got.PurchaseID)
	assert.Equal(t, "ril-1", got.ListingID)
	assert.Equal(t, config.ListingStateClosed, got.State)
	require.NotNil(t, got.ListedAt)
	assert.True(t, pollerCreateDate.Equal(*got.ListedAt))
	// 2 sold at $1000: AWS keeps 12% ($240), the seller receives $1760.
	require.NotNil(t, got.ProceedsReceived)
	require.NotNil(t, got.FeePaid)
	assert.InDelta(t, 1760, *got.ProceedsReceived, 1e-9)
	assert.InDelta(t, 240, *got.FeePaid, 1e-9)
}

// The listing is read with the purchase account's credentials, in its region.
func TestPollMarketplaceListings_SignsAsThePurchaseAccount(t *testing.T) {
	f := newPollerFixture(t, listedRow("ri-1", "ril-1"))
	f.aws.statuses["ril-1"] = marketplaceListingStatus{ListingID: "ril-1", State: config.ListingStateActive}

	_, err := f.h.PollMarketplaceListings(context.Background())

	require.NoError(t, err)
	require.Len(t, f.built, 1)
	assert.Equal(t, marketplaceMemberKeyID, signingKeyID(t, f.built[0]))
	assert.Equal(t, "eu-west-1", f.built[0].Region)
}

func TestPollMarketplaceListings_ActiveStampsListedAtOnly(t *testing.T) {
	store := &MockConfigStore{}
	f := newPollerFixtureWithStore(t, store, listedRow("ri-1", "ril-1"))
	f.aws.statuses["ril-1"] = marketplaceListingStatus{ListingID: "ril-1", State: config.ListingStateActive,
		CreatedAt: pollerCreateDate, SoldCount: 1, UnitPrice: 10, PriceKnown: true}
	f.store.On("RecordMarketplaceListingActive", mock.Anything, "ri-1", "ril-1", pollerCreateDate).Return(true, nil).Once()

	res, err := f.h.PollMarketplaceListings(context.Background())

	require.NoError(t, err)
	assert.Equal(t, MarketplacePollResult{Found: 1, Active: 1}, *res)
	store.AssertExpectations(t)
	store.AssertNotCalled(t, "SettleMarketplaceListing")
}

func pendingRow(purchaseID, listingID string) config.PurchaseHistoryRecord {
	row := listedRow(purchaseID, listingID)
	row.ListingState = config.ListingStatePending
	return row
}

// A listing recorded as pending (AWS gave no status when it was created) that
// AWS now reports active is recorded as active, even without a CreateDate
// (issue #1494).
func TestPollMarketplaceListings_PendingRowBecomesActive(t *testing.T) {
	for _, created := range []time.Time{{}, pollerCreateDate} {
		t.Run(fmt.Sprintf("create date %v", !created.IsZero()), func(t *testing.T) {
			store := &MockConfigStore{}
			f := newPollerFixtureWithStore(t, store, pendingRow("ri-1", "ril-1"))
			f.aws.statuses["ril-1"] = marketplaceListingStatus{ListingID: "ril-1", State: config.ListingStateActive, CreatedAt: created}
			f.store.On("RecordMarketplaceListingActive", mock.Anything, "ri-1", "ril-1", created).Return(true, nil).Once()

			res, err := f.h.PollMarketplaceListings(context.Background())

			require.NoError(t, err)
			assert.Equal(t, MarketplacePollResult{Found: 1, Active: 1}, *res)
			store.AssertExpectations(t)
			store.AssertNotCalled(t, "SettleMarketplaceListing")
		})
	}
}

// An active row whose CreateDate AWS did not report needs no write.
func TestPollMarketplaceListings_ActiveRowWithoutCreateDateNeedsNoWrite(t *testing.T) {
	store := &MockConfigStore{}
	f := newPollerFixtureWithStore(t, store, listedRow("ri-1", "ril-1"))
	f.aws.statuses["ril-1"] = marketplaceListingStatus{ListingID: "ril-1", State: config.ListingStateActive}

	res, err := f.h.PollMarketplaceListings(context.Background())

	require.NoError(t, err)
	assert.Equal(t, MarketplacePollResult{Found: 1, Active: 1}, *res)
	store.AssertNotCalled(t, "RecordMarketplaceListingActive")
}

// A pending row whose listing sold out before any poll is settled like an
// active one (issue #1494).
func TestPollMarketplaceListings_PendingRowSettles(t *testing.T) {
	f := newPollerFixture(t, pendingRow("ri-1", "ril-1"))
	f.aws.statuses["ril-1"] = marketplaceListingStatus{ListingID: "ril-1", State: config.ListingStateClosed,
		CreatedAt: pollerCreateDate, SoldCount: 1, UnitPrice: 1000, PriceKnown: true}
	settled := f.captureSettle(true)

	res, err := f.h.PollMarketplaceListings(context.Background())

	require.NoError(t, err)
	assert.Equal(t, MarketplacePollResult{Found: 1, Settled: 1}, *res)
	require.Len(t, *settled, 1)
	assert.Equal(t, config.ListingStateClosed, (*settled)[0].State)
	assert.InDelta(t, 880, *(*settled)[0].ProceedsReceived, 1e-9)
}

func TestPollMarketplaceListings_CanceledAfterPartialSale(t *testing.T) {
	f := newPollerFixture(t, listedRow("ri-1", "ril-1"))
	f.aws.statuses["ril-1"] = marketplaceListingStatus{ListingID: "ril-1", State: config.ListingStateCancelled,
		CreatedAt: pollerCreateDate, SoldCount: 1, UnitPrice: 500.10, PriceKnown: true}
	settled := f.captureSettle(true)

	res, err := f.h.PollMarketplaceListings(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 1, res.Settled)
	require.Len(t, *settled, 1)
	got := (*settled)[0]
	assert.Equal(t, config.ListingStateCancelled, got.State)
	assert.InDelta(t, 440.09, *got.ProceedsReceived, 1e-9)
	assert.InDelta(t, 60.01, *got.FeePaid, 1e-9)
}

// Nothing sold: the settlement is zero and needs no price.
func TestPollMarketplaceListings_CanceledUnsoldSettlesAtZero(t *testing.T) {
	f := newPollerFixture(t, listedRow("ri-1", "ril-1"))
	f.aws.statuses["ril-1"] = marketplaceListingStatus{ListingID: "ril-1", State: config.ListingStateCancelled}
	settled := f.captureSettle(true)

	res, err := f.h.PollMarketplaceListings(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 1, res.Settled)
	got := (*settled)[0]
	assert.Nil(t, got.ListedAt, "an unknown CreateDate keeps the stored listed_at")
	require.NotNil(t, got.ProceedsReceived)
	assert.Zero(t, *got.ProceedsReceived)
	assert.Zero(t, *got.FeePaid)
}

// Sold instances without an identifiable price: the state is recorded, the
// proceeds are not guessed.
func TestPollMarketplaceListings_UnknownPriceIsNotGuessed(t *testing.T) {
	f := newPollerFixture(t, listedRow("ri-1", "ril-1"))
	f.aws.statuses["ril-1"] = marketplaceListingStatus{ListingID: "ril-1", State: config.ListingStateClosed, SoldCount: 3}
	settled := f.captureSettle(true)

	res, err := f.h.PollMarketplaceListings(context.Background())

	require.NoError(t, err)
	assert.Equal(t, MarketplacePollResult{Found: 1, Unpriced: 1}, *res)
	got := (*settled)[0]
	assert.Equal(t, config.ListingStateClosed, got.State)
	assert.Nil(t, got.ProceedsReceived)
	assert.Nil(t, got.FeePaid)
}

// A settle the store's guard drops (canceled, relisted or settled meanwhile)
// is reported as skipped, not settled.
func TestPollMarketplaceListings_GuardedWriteLostIsSkipped(t *testing.T) {
	f := newPollerFixture(t, listedRow("ri-1", "ril-1"))
	f.aws.statuses["ril-1"] = marketplaceListingStatus{ListingID: "ril-1", State: config.ListingStateClosed,
		SoldCount: 1, UnitPrice: 10, PriceKnown: true}
	f.captureSettle(false)

	res, err := f.h.PollMarketplaceListings(context.Background())

	require.NoError(t, err)
	assert.Equal(t, MarketplacePollResult{Found: 1, Skipped: 1}, *res)
}

func TestPollMarketplaceListings_AWSPendingNeedsNoWrite(t *testing.T) {
	store := &MockConfigStore{}
	f := newPollerFixtureWithStore(t, store, listedRow("ri-1", "ril-1"))
	f.aws.statuses["ril-1"] = marketplaceListingStatus{ListingID: "ril-1", State: "pending", CreatedAt: pollerCreateDate}

	res, err := f.h.PollMarketplaceListings(context.Background())

	require.NoError(t, err)
	assert.Equal(t, MarketplacePollResult{Found: 1, Skipped: 1}, *res)
	store.AssertNotCalled(t, "SettleMarketplaceListing")
	store.AssertNotCalled(t, "RecordMarketplaceListingActive")
}

// One listing failing (AWS or store) does not stop the others.
func TestPollMarketplaceListings_FailuresAreCountedAndTheRunGoesOn(t *testing.T) {
	noAccount := listedRow("ri-orphan", "ril-orphan")
	noAccount.CloudAccountID, noAccount.AccountID = nil, ""
	f := newPollerFixture(t, listedRow("ri-1", "ril-1"), noAccount, listedRow("ri-2", "ril-2"), listedRow("ri-3", "ril-3"))
	f.aws.errs["ril-1"] = errors.New("RequestLimitExceeded")
	f.aws.statuses["ril-2"] = marketplaceListingStatus{ListingID: "ril-2", State: config.ListingStateActive, CreatedAt: pollerCreateDate}
	f.store.On("RecordMarketplaceListingActive", mock.Anything, "ri-2", "ril-2", pollerCreateDate).Return(false, errors.New("db down"))
	f.aws.statuses["ril-3"] = marketplaceListingStatus{ListingID: "ril-3", State: config.ListingStateClosed,
		SoldCount: 1, UnitPrice: 10, PriceKnown: true}
	f.captureSettle(true)

	res, err := f.h.PollMarketplaceListings(context.Background())

	require.NoError(t, err)
	assert.Equal(t, MarketplacePollResult{Found: 4, Settled: 1, Errored: 3}, *res)
	assert.Equal(t, []string{"ril-1", "ril-2", "ril-3"}, f.aws.reads, "a row without credentials is never read from AWS")
}

func TestPollMarketplaceListings_ListFailureFailsTheRun(t *testing.T) {
	f := newPollerFixture(t)
	f.store.ExpectedCalls = nil
	f.store.On("ListMarketplaceListingsToPoll", mock.Anything).Return(nil, errors.New("db down"))

	res, err := f.h.PollMarketplaceListings(context.Background())

	require.Error(t, err)
	assert.Nil(t, res)
}

func TestPollMarketplaceListings_StopsWhenTheContextEnds(t *testing.T) {
	f := newPollerFixture(t, listedRow("ri-1", "ril-1"), listedRow("ri-2", "ril-2"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := f.h.PollMarketplaceListings(ctx)

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 2, res.Found)
	assert.Empty(t, f.aws.reads)
}

func TestMarketplaceSettlementAmounts(t *testing.T) {
	cases := []struct {
		name          string
		sold          int64
		price         float64
		proceeds, fee float64
	}{
		{"two at 1000", 2, 1000, 1760, 240},
		{"fee rounds half up to the cent", 1, 0.125, 0.11, 0.02},
		{"price rounded to the cent first", 3, 33.333, 87.99, 12},
		{"nothing sold", 0, 99, 0, 0},
		{"free", 4, 0, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			proceeds, fee := marketplaceSettlementAmounts(c.sold, c.price)
			assert.InDelta(t, c.proceeds, proceeds, 1e-9)
			assert.InDelta(t, c.fee, fee, 1e-9)
		})
	}
}

func usdTier(active bool, price float64) sdkec2types.PriceSchedule {
	return sdkec2types.PriceSchedule{Active: aws.Bool(active), Price: aws.Float64(price), Term: aws.Int64(6),
		CurrencyCode: sdkec2types.CurrencyCodeValuesUsd}
}

func TestActiveUSDListingPrice(t *testing.T) {
	nonUSD := usdTier(true, 5)
	nonUSD.CurrencyCode = "EUR"
	noPrice := usdTier(true, 5)
	noPrice.Price = nil
	cases := []struct {
		name      string
		schedules []sdkec2types.PriceSchedule
		price     float64
		known     bool
	}{
		{"the active tier", []sdkec2types.PriceSchedule{usdTier(false, 900), usdTier(true, 450.5), usdTier(false, 100)}, 450.5, true},
		{"none active", []sdkec2types.PriceSchedule{usdTier(false, 900)}, 0, false},
		{"two active", []sdkec2types.PriceSchedule{usdTier(true, 900), usdTier(true, 450)}, 0, false},
		{"not USD", []sdkec2types.PriceSchedule{nonUSD}, 0, false},
		{"no price", []sdkec2types.PriceSchedule{noPrice}, 0, false},
		{"negative", []sdkec2types.PriceSchedule{usdTier(true, -1)}, 0, false},
		{"empty", nil, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			price, known := activeUSDListingPrice(c.schedules)
			assert.Equal(t, c.known, known)
			assert.InDelta(t, c.price, price, 1e-9)
		})
	}
}

func TestAWSMarketplaceListingFinder_DescribeStatus(t *testing.T) {
	created := pollerCreateDate
	api := &fakeDescribeListings{out: []sdkec2types.ReservedInstancesListing{
		{ReservedInstancesListingId: aws.String("ril-other"), Status: sdkec2types.ListingStatusActive},
		{
			ReservedInstancesListingId: aws.String("ril-1"), Status: sdkec2types.ListingStatusCancelled, CreateDate: &created,
			InstanceCounts: []sdkec2types.InstanceCount{
				{State: sdkec2types.ListingStateSold, InstanceCount: aws.Int32(2)},
				{State: sdkec2types.ListingStateCancelled, InstanceCount: aws.Int32(1)},
				{State: sdkec2types.ListingStateAvailable, InstanceCount: aws.Int32(0)},
			},
			PriceSchedules: []sdkec2types.PriceSchedule{usdTier(false, 900), usdTier(true, 450)},
		},
	}}
	finder := awsMarketplaceListingFinder{api: api}

	got, err := finder.DescribeMarketplaceListingStatus(context.Background(), "ril-1")

	require.NoError(t, err)
	assert.Equal(t, "ril-1", aws.ToString(api.in.ReservedInstancesListingId))
	assert.Nil(t, api.in.ReservedInstancesId)
	assert.Equal(t, marketplaceListingStatus{ListingID: "ril-1", State: config.ListingStateCancelled, CreatedAt: created,
		SoldCount: 2, UnitPrice: 450, PriceKnown: true}, got)

	_, err = finder.DescribeMarketplaceListingStatus(context.Background(), "ril-missing")
	require.ErrorIs(t, err, errMarketplaceListingNotFound)

	api.err = errors.New("boom")
	_, err = finder.DescribeMarketplaceListingStatus(context.Background(), "ril-1")
	require.Error(t, err)
}

// The SDK status is normalized to the lowercase values the store compares.
func TestMarketplaceListingStatusFromSDK_NormalizesState(t *testing.T) {
	st := marketplaceListingStatusFromSDK(&sdkec2types.ReservedInstancesListing{Status: "Closed"})
	assert.Equal(t, config.ListingStateClosed, st.State)
	assert.True(t, st.CreatedAt.IsZero())
}

var _ describeListingsAPI = (*sdkec2.Client)(nil)
