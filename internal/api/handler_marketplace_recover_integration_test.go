//go:build integration

package api

// Real-Postgres tests for issue #525 with fake marketplace clients (no AWS
// call is made): a list request that crashes between the AWS create and the DB
// write is recovered through the cancel endpoint, and a cancel that lands
// while a list request is about to record its listing is not undone.

import (
	"context"
	"errors"
	"sync"
	"testing"

	ec2svc "github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/ec2"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lockedMarketplaceAWS serializes the fake AWS for the race test. The embedded
// finder is nil: only FindMarketplaceListingByToken, overridden below, is used,
// and embedding keeps the type a marketplaceListingFinder if that interface
// grows.
type lockedMarketplaceAWS struct {
	marketplaceListingFinder //nolint:unused // nil; fills in finder methods these tests never call
	mu                       sync.Mutex
	fake                     fakeMarketplaceAWS
}

func (a *lockedMarketplaceAWS) create(ctx context.Context, req ec2svc.MarketplaceListingRequest) (ec2svc.MarketplaceListingResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.fake.create(ctx, req)
}

func (a *lockedMarketplaceAWS) cancel(ctx context.Context, id string) (ec2svc.MarketplaceListingResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.fake.cancel(ctx, id)
}

func (a *lockedMarketplaceAWS) describe(ctx context.Context, id string) (ec2svc.MarketplaceListingResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.fake.describe(ctx, id)
}

func (a *lockedMarketplaceAWS) FindMarketplaceListingByToken(ctx context.Context, ri, token string) (ec2svc.MarketplaceListingResult, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.fake.FindMarketplaceListingByToken(ctx, ri, token)
}

func (a *lockedMarketplaceAWS) live() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.fake.live()
}

func (a *lockedMarketplaceAWS) setFailCancel(fail bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fake.failCancel = fail
}

// listingWriteHook wraps the real store and runs hook before every
// UpdatePurchaseHistoryListing; a non-nil error from hook replaces the write.
type listingWriteHook struct {
	config.StoreInterface
	mu   sync.Mutex
	hook func(listingID, state string) error
}

func (s *listingWriteHook) setHook(hook func(listingID, state string) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hook = hook
}

func (s *listingWriteHook) UpdatePurchaseHistoryListing(ctx context.Context, purchaseID, listingID, state string) error {
	s.mu.Lock()
	hook := s.hook
	s.mu.Unlock()
	if hook != nil {
		if err := hook(listingID, state); err != nil {
			return err
		}
	}
	return s.StoreInterface.UpdatePurchaseHistoryListing(ctx, purchaseID, listingID, state)
}

type recoverRig struct {
	h     *Handler
	store *config.PostgresStore
	hook  *listingWriteHook
	aws   *lockedMarketplaceAWS
	ec2   *stubMarketplaceEC2
	row   *config.PurchaseHistoryRecord
}

func newRecoverRig(t *testing.T) *recoverRig {
	t.Helper()
	ctx := t.Context()
	pg := testhelpers.RequireMigratedDatabase(ctx, t, "../database/postgres/migrations")
	store := config.NewPostgresStore(pg.DB)
	rig := &recoverRig{store: store, hook: &listingWriteHook{StoreInterface: store}, aws: &lockedMarketplaceAWS{}}
	rig.row = setupCompensationRow(t, store)
	rig.ec2 = &stubMarketplaceEC2{createFn: rig.aws.create, cancelFn: rig.aws.cancel, describeFn: rig.aws.describe}
	rig.h = &Handler{config: rig.hook, auth: newCompensationAuth(t),
		marketplaceEC2Factory:    func(aws.Config) marketplaceEC2Client { return rig.ec2 },
		marketplaceFinderFactory: func(aws.Config) marketplaceListingFinder { return rig.aws }}
	rig.h.awsCfgOnce.Do(func() { rig.h.awsCfg = hostAWSConfig() })
	return rig
}

func (r *recoverRig) attempt(t *testing.T) config.MarketplaceListingAttempt {
	t.Helper()
	a, err := r.store.GetMarketplaceListingAttempt(t.Context(), r.row.PurchaseID)
	require.NoError(t, err)
	require.NotNil(t, a)
	return *a
}

// A list request creates the listing on AWS, then every write after it fails
// (the DB write, the rollback cancel, the record of the uncanceled listing),
// as when the process dies right there. The row is left pending with the
// attempt's token. The cancel endpoint recovers it: it finds the listing by
// the token, cancels it on AWS and records it, and the RI lists again.
func TestMarketplaceCancelRecoversCrashedListAttempt(t *testing.T) {
	rig := newRecoverRig(t)
	ctx := t.Context()
	rig.hook.setHook(func(string, string) error { return errors.New("process died") })
	rig.aws.setFailCancel(true)

	_, err := rig.h.marketplaceList(ctx, marketplaceReq(), rig.row.PurchaseID)
	require.Error(t, err)
	crashed := rig.attempt(t)
	require.Equal(t, config.ListingStatePending, crashed.State)
	require.NotEmpty(t, crashed.ClientToken, "the token was persisted before the AWS call")
	require.Equal(t, 1, rig.aws.live())

	rig.hook.setHook(nil)
	rig.aws.setFailCancel(false)
	resp, err := rig.h.marketplaceCancel(ctx, marketplaceReq(), rig.row.PurchaseID)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"listing_id": "ril-1", "listing_state": config.ListingStateCancelled}, resp)
	assert.Equal(t, 0, rig.aws.live())
	assert.Equal(t, config.MarketplaceListingAttempt{ListingID: "ril-1", State: config.ListingStateCancelled}, rig.attempt(t))

	_, err = rig.h.marketplaceList(ctx, marketplaceReq(), rig.row.PurchaseID)
	require.NoError(t, err)
	assert.Equal(t, config.MarketplaceListingAttempt{ListingID: "ril-2", State: config.ListingStateActive}, rig.attempt(t))
	assert.Equal(t, 1, rig.aws.live())
}

// A cancel arrives while a list request has created its listing on AWS but
// not yet recorded it. The cancel finds the listing by the persisted token,
// cancels it and records it; the list request's record then reaches Postgres,
// which refuses to mark the canceled listing active again. The list request
// reports the conflict and does not run its rollback.
func TestMarketplaceCancelDuringListIsNotUndone(t *testing.T) {
	rig := newRecoverRig(t)
	ctx := t.Context()
	reached := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once
	rig.hook.setHook(func(_, state string) error {
		if state == config.ListingStateActive {
			once.Do(func() {
				close(reached)
				<-proceed
			})
		}
		return nil
	})

	listDone := make(chan error, 1)
	go func() {
		_, listErr := rig.h.marketplaceList(ctx, marketplaceReq(), rig.row.PurchaseID)
		listDone <- listErr
	}()
	testhelpers.Await(t, reached)
	require.Equal(t, config.ListingStatePending, rig.attempt(t).State)

	resp, err := rig.h.marketplaceCancel(ctx, marketplaceReq(), rig.row.PurchaseID)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"listing_id": "ril-1", "listing_state": config.ListingStateCancelled}, resp)
	close(proceed)

	listErr := testhelpers.Await(t, listDone)
	ce, ok := IsClientError(listErr)
	require.True(t, ok, "%v", listErr)
	assert.Equal(t, 409, ce.code)
	assert.Contains(t, ce.message, "canceled by another request")
	assert.Equal(t, config.MarketplaceListingAttempt{ListingID: "ril-1", State: config.ListingStateCancelled}, rig.attempt(t),
		"the canceled listing must not be recorded as active")
	assert.Equal(t, 0, rig.aws.live())
	assert.Equal(t, 1, rig.ec2.cancelCallCount, "only the cancel endpoint's cancel; no rollback cancel")
}
