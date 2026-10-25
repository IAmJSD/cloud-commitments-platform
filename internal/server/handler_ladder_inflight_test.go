package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgcommon "github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	pkgladder "github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/testutil"
)

// errLadderInFlightFetch stands in for the store's locked in-flight read
// failing (PostgresStore.PlanAndSaveLadderRun -> lockedLadderCommit ->
// "read ladder netting rows: ...").
var errLadderInFlightFetch = errors.New("read ladder netting rows: connection reset by peer")

// inFlightTestAccount is both the config's cloud account and the caller account,
// so the single-account gate passes and the run reaches the persist step.
const inFlightTestAccount = "123456789012"

// submitCountingCapability wraps the fake capability and counts write-side
// calls instead of failing the test outright, so "nothing submitted" is a
// counted assertion rather than the absence of a t.Fatal.
type submitCountingCapability struct {
	*fakeLadderCapability
	purchases, reshapes int
}

func (c *submitCountingCapability) PurchaseLayer(_ context.Context, _ pkgladder.LayerType, _ pkgcommon.Recommendation, _ pkgcommon.PurchaseOptions) (pkgcommon.PurchaseResult, error) {
	c.purchases++
	return pkgcommon.PurchaseResult{}, errors.New("PurchaseLayer must not be called on an in-flight fetch error")
}

func (c *submitCountingCapability) ReshapeBuffer(_ context.Context, _ pkgladder.Scope, _ pkgladder.BufferReshapeConfig) (pkgladder.ReshapeSummary, error) {
	c.reshapes++
	return pkgladder.ReshapeSummary{}, errors.New("ReshapeBuffer must not be called on an in-flight fetch error")
}

// submitRecordingStore wraps ladderTestStore and records every purchase-side
// write a ladder run could make, plus the direct (non-transactional) ladder
// run/tranche saves, so a handler that swallowed the in-flight error and saved
// or submitted anyway is caught.
type submitRecordingStore struct {
	*ladderTestStore
	executionsSaved, historySaved int
}

func (s *submitRecordingStore) SavePurchaseExecution(_ context.Context, _ *config.PurchaseExecution) error {
	s.executionsSaved++
	return nil
}

func (s *submitRecordingStore) SavePurchaseHistory(_ context.Context, _ *config.PurchaseHistoryRecord) error {
	s.historySaved++
	return nil
}

// assertNothingPersistedOrSubmitted checks that an in-flight fetch error left
// no ladder run, no tranche (which the fire task would later submit), no
// purchase record and no provider write behind.
func assertNothingPersistedOrSubmitted(t *testing.T, store *submitRecordingStore, fakeCap *submitCountingCapability) {
	t.Helper()
	assert.Nil(t, store.savedRun, "no ladder_runs row may be saved when the in-flight fetch fails")
	assert.Empty(t, store.savedRuns, "no ladder_runs row may be saved when the in-flight fetch fails")
	assert.Nil(t, store.savedTranches, "no ladder_tranches rows may be saved when the in-flight fetch fails")
	assert.Zero(t, store.executionsSaved, "no purchase execution may be saved")
	assert.Zero(t, store.historySaved, "no purchase history may be saved")
	assert.Zero(t, fakeCap.purchases, "PurchaseLayer must not be called")
	assert.Zero(t, fakeCap.reshapes, "ReshapeBuffer must not be called")
}

// newInFlightErrFixture returns a store whose in-flight read fails and a
// capability that would otherwise plan a positive purchase (low-water 10,
// target 8, nothing existing), so a swallowed error would visibly save a run
// with tranches.
func newInFlightErrFixture(t *testing.T) (*submitRecordingStore, *submitCountingCapability) {
	t.Helper()
	store := &submitRecordingStore{ladderTestStore: &ladderTestStore{
		// Built inline rather than via validTestCloudAccount: a fourth caller
		// passing the same account trips unparam on that helper.
		cloudAcct:   &config.CloudAccount{ID: "cloud-acct-uuid", Provider: "aws", ExternalID: inFlightTestAccount, Enabled: true},
		inFlightErr: errLadderInFlightFetch,
	}}
	fakeCap := &submitCountingCapability{fakeLadderCapability: &fakeLadderCapability{t: t, baseline: testBaseline()}}
	return store, fakeCap
}

// TestExecuteLadderRun_InFlightFetchError_FailsLoudAndPersistsNothing covers
// the handler's branch for an in-flight-commitment fetch error (#1180): the
// run must return the wrapped error, not treat in-flight as empty and plan the
// full gap, and must save and submit nothing.
func TestExecuteLadderRun_InFlightFetchError_FailsLoudAndPersistsNothing(t *testing.T) {
	ctx := testutil.TestContext(t)
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	dbCfg := validTestDBConfig("cfg-inflight-err")
	store, fakeCap := newInFlightErrFixture(t)
	app := &Application{Config: store}

	err := app.executeLadderRun(ctx, &dbCfg, fakeCap, inFlightTestAccount, pkgladder.Term1Year, pkgladder.PaymentNoUpfront, now)

	require.Error(t, err, "an in-flight fetch error must fail the run, not report success")
	require.ErrorIs(t, err, errLadderInFlightFetch, "the store's error must be wrapped, not replaced")
	assert.Equal(t, "persist ladder run: "+errLadderInFlightFetch.Error(), err.Error())
	assertNothingPersistedOrSubmitted(t, store, fakeCap)
}

// TestRunLadderConfigs_InFlightFetchError_CountsErrored drives the same error
// through the per-config loop: the config must be counted Errored (the task's
// failure signal), never Planned.
func TestRunLadderConfigs_InFlightFetchError_CountsErrored(t *testing.T) {
	ctx := testutil.TestContext(t)
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	dbCfg := validTestDBConfig("cfg-inflight-err-loop")
	store, fakeCap := newInFlightErrFixture(t)
	app := &Application{
		Config: store,
		LadderCapabilityFactory: func(_ context.Context, _, _ string) (pkgladder.LadderCapability, error) {
			return fakeCap, nil
		},
	}

	result := app.runLadderConfigs(ctx, []config.LadderConfigDB{dbCfg}, inFlightTestAccount, "us-east-1", pkgladder.Term1Year, pkgladder.PaymentNoUpfront, now, false)

	assert.Equal(t, LadderRunResult{Errored: 1}, *result, "an in-flight fetch error must count the config Errored and nothing else")
	assertNothingPersistedOrSubmitted(t, store, fakeCap)
}
