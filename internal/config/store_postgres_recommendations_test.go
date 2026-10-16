//go:build integration
// +build integration

package config_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupRecommendationsStore(ctx context.Context, t *testing.T) (*config.PostgresStore, func()) {
	t.Helper()
	container, err := testhelpers.SetupPostgresContainer(ctx, t)
	require.NoError(t, err)
	err = migrations.RunMigrations(ctx, container.DB.Pool(), getMigrationsPath(), "", "")
	require.NoError(t, err)
	store := config.NewPostgresStore(container.DB)
	return store, func() { container.Cleanup(ctx) }
}

// seedRecommendationCloudAccount registers a cloud account so that
// recommendations rows referencing it satisfy the FK on
// recommendations.cloud_account_id (migration 000030). Production upserts
// only ever carry registered account IDs, so tests must create the account
// first.
func seedRecommendationCloudAccount(ctx context.Context, t *testing.T, store *config.PostgresStore, id, provider, externalID string) {
	t.Helper()
	require.NoError(t, store.CreateCloudAccount(ctx, &config.CloudAccount{
		ID:         id,
		Name:       "rec-test-" + externalID,
		Provider:   provider,
		ExternalID: externalID,
		Enabled:    true,
	}), "seeding cloud account %s failed", id)
}

func awsRec(id, service, region, resourceType string, savings float64) config.RecommendationRecord {
	return config.RecommendationRecord{
		ID:           id,
		Provider:     "aws",
		Service:      service,
		Region:       region,
		ResourceType: resourceType,
		Savings:      savings,
		UpfrontCost:  100.0,
		MonthlyCost:  float64Ptr(50.0),
		Count:        1,
		Term:         12,
		Payment:      "no-upfront",
	}
}

func TestPostgresStore_ReplaceRecommendations(t *testing.T) {
	ctx := context.Background()
	store, cleanup := setupRecommendationsStore(ctx, t)
	defer cleanup()

	now := time.Now().UTC().Truncate(time.Second)

	initial := []config.RecommendationRecord{
		awsRec("a1", "ec2", "us-east-1", "m5.large", 10),
		awsRec("a2", "rds", "us-east-1", "db.r5.large", 20),
	}
	require.NoError(t, store.ReplaceRecommendations(ctx, now, initial))

	got, err := store.ListStoredRecommendations(ctx, config.RecommendationFilter{})
	require.NoError(t, err)
	assert.Len(t, got, 2)

	// Replace with a new snapshot — original rows must be gone.
	replacement := []config.RecommendationRecord{
		awsRec("b1", "ec2", "eu-west-1", "m5.xlarge", 50),
	}
	require.NoError(t, store.ReplaceRecommendations(ctx, now.Add(time.Minute), replacement))

	got, err = store.ListStoredRecommendations(ctx, config.RecommendationFilter{})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "eu-west-1", got[0].Region)
}

func TestPostgresStore_UpsertRecommendations_PartialCollect(t *testing.T) {
	ctx := context.Background()
	store, cleanup := setupRecommendationsStore(ctx, t)
	defer cleanup()

	t0 := time.Now().UTC().Truncate(time.Second)

	// Seed full collection covering both aws and azure (both ambient — nil account).
	seed := []config.RecommendationRecord{
		awsRec("a1", "ec2", "us-east-1", "m5.large", 10),
		{ID: "z1", Provider: "azure", Service: "vm", Region: "eastus", ResourceType: "Standard_D2", Savings: 15},
	}
	require.NoError(t, store.UpsertRecommendations(ctx, t0, seed, []config.SuccessfulCollect{
		{Provider: "aws"},
		{Provider: "azure"},
	}))

	// Second collect succeeds only for aws — provide new aws rows, list only
	// aws in successfulCollects. Azure rows must be preserved since azure
	// didn't run.
	t1 := t0.Add(time.Hour)
	partial := []config.RecommendationRecord{
		awsRec("a1", "ec2", "us-east-1", "m5.large", 25), // same natural key as before → upsert
	}
	require.NoError(t, store.UpsertRecommendations(ctx, t1, partial, []config.SuccessfulCollect{
		{Provider: "aws"},
	}))

	got, err := store.ListStoredRecommendations(ctx, config.RecommendationFilter{})
	require.NoError(t, err)
	require.Len(t, got, 2, "azure row should survive partial-aws collect")

	var awsFound, azureFound bool
	for _, rec := range got {
		if rec.Provider == "aws" {
			awsFound = true
			assert.InDelta(t, 25.0, rec.Savings, 0.001, "aws row should reflect upserted savings")
		}
		if rec.Provider == "azure" {
			azureFound = true
		}
	}
	assert.True(t, awsFound)
	assert.True(t, azureFound)
}

func TestPostgresStore_UpsertRecommendations_EvictsStaleInSuccessfulProvider(t *testing.T) {
	ctx := context.Background()
	store, cleanup := setupRecommendationsStore(ctx, t)
	defer cleanup()

	t0 := time.Now().UTC().Truncate(time.Second)

	// Seed two aws rows (ambient — nil account).
	seed := []config.RecommendationRecord{
		awsRec("a1", "ec2", "us-east-1", "m5.large", 10),
		awsRec("a2", "ec2", "us-east-1", "m5.xlarge", 20),
	}
	require.NoError(t, store.UpsertRecommendations(ctx, t0, seed, []config.SuccessfulCollect{
		{Provider: "aws"},
	}))

	// Second collect: aws only returns one row (the m5.xlarge is gone).
	// With successfulCollects=[{aws,nil}], the unseen m5.large must be deleted.
	t1 := t0.Add(time.Hour)
	follow := []config.RecommendationRecord{
		awsRec("a2", "ec2", "us-east-1", "m5.xlarge", 25),
	}
	require.NoError(t, store.UpsertRecommendations(ctx, t1, follow, []config.SuccessfulCollect{
		{Provider: "aws"},
	}))

	got, err := store.ListStoredRecommendations(ctx, config.RecommendationFilter{})
	require.NoError(t, err)
	require.Len(t, got, 1, "unseen aws row should be evicted")
	assert.Equal(t, "m5.xlarge", got[0].ResourceType)
}

func TestPostgresStore_ListStoredRecommendations_FilterPushdown(t *testing.T) {
	ctx := context.Background()
	store, cleanup := setupRecommendationsStore(ctx, t)
	defer cleanup()

	now := time.Now().UTC().Truncate(time.Second)

	recs := []config.RecommendationRecord{
		awsRec("a1", "ec2", "us-east-1", "m5.large", 5),
		awsRec("a2", "rds", "us-east-1", "db.r5.large", 30),
		{ID: "z1", Provider: "azure", Service: "vm", Region: "eastus", ResourceType: "Standard_D2", Savings: 20},
	}
	require.NoError(t, store.ReplaceRecommendations(ctx, now, recs))

	// Filter by provider.
	got, err := store.ListStoredRecommendations(ctx, config.RecommendationFilter{Provider: "aws"})
	require.NoError(t, err)
	assert.Len(t, got, 2)

	// Filter by service.
	got, err = store.ListStoredRecommendations(ctx, config.RecommendationFilter{Service: "ec2"})
	require.NoError(t, err)
	assert.Len(t, got, 1)

	// Filter by min savings (dollar floor, pushed down to SQL).
	got, err = store.ListStoredRecommendations(ctx, config.RecommendationFilter{MinSavingsUSD: 25})
	require.NoError(t, err)
	assert.Len(t, got, 1)
	assert.Equal(t, "rds", got[0].Service)
}

func TestPostgresStore_Freshness_RoundTrip(t *testing.T) {
	ctx := context.Background()
	store, cleanup := setupRecommendationsStore(ctx, t)
	defer cleanup()

	// Fresh migration: LastCollectedAt is nil.
	fr, err := store.GetRecommendationsFreshness(ctx)
	require.NoError(t, err)
	assert.Nil(t, fr.LastCollectedAt)
	assert.Nil(t, fr.LastCollectionError)

	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, store.ReplaceRecommendations(ctx, now, nil))

	fr, err = store.GetRecommendationsFreshness(ctx)
	require.NoError(t, err)
	require.NotNil(t, fr.LastCollectedAt)
	assert.WithinDuration(t, now, *fr.LastCollectedAt, time.Second)
	assert.Nil(t, fr.LastCollectionError)

	// SetRecommendationsCollectionError leaves last_collected_at untouched.
	require.NoError(t, store.SetRecommendationsCollectionError(ctx, "aws failed"))
	fr, err = store.GetRecommendationsFreshness(ctx)
	require.NoError(t, err)
	require.NotNil(t, fr.LastCollectedAt)
	require.NotNil(t, fr.LastCollectionError)
	assert.Equal(t, "aws failed", *fr.LastCollectionError)
}

// TestPostgresStore_CollectionPartial_RecordedAndCleared pins issue #87: a
// partial sweep's summary round-trips through recommendations_state, survives
// an error write (both can be true of one run), and is cleared by the next
// Upsert or Replace -- i.e. by the next persisted collection, which re-sets
// it only if that collection was partial too.
func TestPostgresStore_CollectionPartial_RecordedAndCleared(t *testing.T) {
	ctx := context.Background()
	store, cleanup := setupRecommendationsStore(ctx, t)
	defer cleanup()

	fr, err := store.GetRecommendationsFreshness(ctx)
	require.NoError(t, err)
	assert.Nil(t, fr.LastCollectionPartial, "a fresh deployment reads as complete")

	partial := &config.RecommendationsCollectionPartial{
		Providers: []config.PartialProviderSweep{{
			Provider:           "azure",
			IncompleteAccounts: 1,
			AttemptedScopes:    10,
			FailedScopes:       9,
			Reasons:            map[string]int{config.PartialReasonPermissionDenied: 9},
		}},
	}
	recordPartial := func() {
		t.Helper()
		require.NoError(t, store.SetRecommendationsCollectionPartial(ctx, partial))
		got, getErr := store.GetRecommendationsFreshness(ctx)
		require.NoError(t, getErr)
		require.Equal(t, partial, got.LastCollectionPartial)
	}

	recordPartial()
	require.NoError(t, store.SetRecommendationsCollectionError(ctx, "gcp: failed"))
	fr, err = store.GetRecommendationsFreshness(ctx)
	require.NoError(t, err)
	assert.Equal(t, partial, fr.LastCollectionPartial, "recording an error must not drop the partial marker")

	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, store.UpsertRecommendations(ctx, now, nil, nil))
	fr, err = store.GetRecommendationsFreshness(ctx)
	require.NoError(t, err)
	assert.Nil(t, fr.LastCollectionPartial, "the next persisted collection must clear the marker")
	assert.Nil(t, fr.LastCollectionError)

	recordPartial()
	require.NoError(t, store.ReplaceRecommendations(ctx, now.Add(time.Minute), nil))
	fr, err = store.GetRecommendationsFreshness(ctx)
	require.NoError(t, err)
	assert.Nil(t, fr.LastCollectionPartial, "a full resync must clear the marker too")

	recordPartial()
	require.NoError(t, store.SetRecommendationsCollectionPartial(ctx, nil))
	fr, err = store.GetRecommendationsFreshness(ctx)
	require.NoError(t, err)
	assert.Nil(t, fr.LastCollectionPartial, "a nil partial clears the column")
}

// TestPostgresStore_UpsertRecommendations_StoresAllTermVariants pins
// the broadened-natural-key behavior from migration 000032: when
// Azure returns multiple `(term, payment)` variants for the same
// (account, provider, service, region, resource_type) SKU, all of
// them must round-trip through the cache as distinct rows. Pre-fix
// they collided on `ON CONFLICT DO UPDATE command cannot affect
// row a second time` (SQLSTATE 21000); the worked-around dedupe
// helper would silently drop all but the highest-savings variant.
//
// Schema dependency: requires migration 000032 applied (term and
// payment_option columns + the 7-column unique index). The
// setupRecommendationsStore helper runs the full migration chain.
func TestPostgresStore_UpsertRecommendations_StoresAllTermVariants(t *testing.T) {
	ctx := context.Background()
	store, cleanup := setupRecommendationsStore(ctx, t)
	defer cleanup()

	now := time.Now().UTC().Truncate(time.Second)

	// Three Azure recs that share (account, provider, service, region,
	// resource_type) but differ in (term, payment) — same SKU, three
	// variants the user might choose between.
	azureSKU := "Standard_D2s_v3"
	variants := []config.RecommendationRecord{
		{ID: "v1", Provider: "azure", Service: "vm", Region: "eastus", ResourceType: azureSKU, Term: 1, Payment: "upfront", Savings: 100, UpfrontCost: 1000, Count: 1},
		{ID: "v2", Provider: "azure", Service: "vm", Region: "eastus", ResourceType: azureSKU, Term: 3, Payment: "upfront", Savings: 500, UpfrontCost: 4000, Count: 1},
		{ID: "v3", Provider: "azure", Service: "vm", Region: "eastus", ResourceType: azureSKU, Term: 3, Payment: "no-upfront", Savings: 350, UpfrontCost: 0, Count: 1},
	}

	require.NoError(t, store.UpsertRecommendations(ctx, now, variants, []config.SuccessfulCollect{
		{Provider: "azure"},
	}))

	got, err := store.ListStoredRecommendations(ctx, config.RecommendationFilter{Provider: "azure"})
	require.NoError(t, err)
	require.Len(t, got, 3, "all 3 (term, payment) variants must round-trip — pre-fix this would have collapsed to 1")

	// Distinct (Term, Payment) tuples — none missing.
	seen := map[string]bool{}
	for _, r := range got {
		key := fmt.Sprintf("%d/%s", r.Term, r.Payment)
		seen[key] = true
	}
	assert.True(t, seen["1/upfront"], "1yr upfront variant must be present")
	assert.True(t, seen["3/upfront"], "3yr upfront variant must be present")
	assert.True(t, seen["3/no-upfront"], "3yr no-upfront variant must be present")
}

// TestPostgresStore_UpsertRecommendations_AccountScopedEviction pins the
// per-account eviction contract: when only one of two accounts under the
// same provider succeeds, the other account's previous-cycle rows survive.
// This is the core bug-fix this commit lands — pre-fix, eviction was
// scoped to (provider) only and would wipe the failed account's rows.
func TestPostgresStore_UpsertRecommendations_AccountScopedEviction(t *testing.T) {
	ctx := context.Background()
	store, cleanup := setupRecommendationsStore(ctx, t)
	defer cleanup()

	// Two registered Azure accounts; valid UUIDs because the
	// account_key generated column is UUID-typed. The accounts must exist
	// in cloud_accounts: recommendations.cloud_account_id carries an FK
	// (migration 000030).
	acct1 := "11111111-1111-1111-1111-111111111111"
	acct2 := "22222222-2222-2222-2222-222222222222"
	// seedRecommendationCloudAccount already creates the cloud_accounts rows
	// that recommendations.cloud_account_id FK (migration 000030) requires.
	seedRecommendationCloudAccount(ctx, t, store, acct1, "azure", "sub-1111")
	seedRecommendationCloudAccount(ctx, t, store, acct2, "azure", "sub-2222")

	t0 := time.Now().UTC().Truncate(time.Second)

	seed := []config.RecommendationRecord{
		{ID: "az1-a", Provider: "azure", Service: "vm", Region: "eastus", ResourceType: "Standard_D2", Savings: 10, CloudAccountID: &acct1},
		{ID: "az1-b", Provider: "azure", Service: "vm", Region: "eastus", ResourceType: "Standard_D4", Savings: 20, CloudAccountID: &acct1},
		{ID: "az2-a", Provider: "azure", Service: "vm", Region: "eastus", ResourceType: "Standard_D2", Savings: 30, CloudAccountID: &acct2},
		{ID: "az2-b", Provider: "azure", Service: "vm", Region: "eastus", ResourceType: "Standard_D4", Savings: 40, CloudAccountID: &acct2},
	}
	require.NoError(t, store.UpsertRecommendations(ctx, t0, seed, []config.SuccessfulCollect{
		{Provider: "azure", CloudAccountID: &acct1},
		{Provider: "azure", CloudAccountID: &acct2},
	}))

	// Partial collect at t1: only acct-1 succeeded with one new row.
	t1 := t0.Add(time.Hour)
	partial := []config.RecommendationRecord{
		{ID: "az1-c", Provider: "azure", Service: "vm", Region: "eastus", ResourceType: "Standard_D8", Savings: 50, CloudAccountID: &acct1},
	}
	require.NoError(t, store.UpsertRecommendations(ctx, t1, partial, []config.SuccessfulCollect{
		{Provider: "azure", CloudAccountID: &acct1},
	}))

	// Assert: acct-1's stale rows (D2 + D4 from t0) are evicted; acct-1
	// keeps the new D8 row; acct-2's two rows survive.
	got, err := store.ListStoredRecommendations(ctx, config.RecommendationFilter{Provider: "azure"})
	require.NoError(t, err)

	byAccountAndType := map[string]bool{}
	for _, rec := range got {
		var acctTag string
		if rec.CloudAccountID != nil {
			acctTag = *rec.CloudAccountID
		}
		byAccountAndType[acctTag+"|"+rec.ResourceType] = true
	}
	assert.True(t, byAccountAndType[acct1+"|Standard_D8"], "acct-1 new row should be present")
	assert.False(t, byAccountAndType[acct1+"|Standard_D2"], "acct-1 stale D2 should be evicted")
	assert.False(t, byAccountAndType[acct1+"|Standard_D4"], "acct-1 stale D4 should be evicted")
	assert.True(t, byAccountAndType[acct2+"|Standard_D2"], "acct-2 row must be preserved (not in successfulCollects this run)")
	assert.True(t, byAccountAndType[acct2+"|Standard_D4"], "acct-2 row must be preserved (not in successfulCollects this run)")
}

// TestPostgresStore_UpsertRecommendations_AmbientAndRegisteredCoexist pins
// that ambient (cloud_account_id NULL → account_key=uuid.Nil) and
// registered identities are evicted independently within the same
// provider. A partial collect that only registers the registered account
// must not touch the ambient row.
func TestPostgresStore_UpsertRecommendations_AmbientAndRegisteredCoexist(t *testing.T) {
	ctx := context.Background()
	store, cleanup := setupRecommendationsStore(ctx, t)
	defer cleanup()

	// The registered account must exist in cloud_accounts:
	// recommendations.cloud_account_id carries an FK (migration 000030).
	// seedRecommendationCloudAccount already creates the cloud_accounts row
	// that recommendations.cloud_account_id FK (migration 000030) requires.
	registeredAcctID := "33333333-3333-3333-3333-333333333333"
	seedRecommendationCloudAccount(ctx, t, store, registeredAcctID, "aws", "333333333333")

	t0 := time.Now().UTC().Truncate(time.Second)

	// Seed one ambient row (CloudAccountID nil) + one registered row.
	seed := []config.RecommendationRecord{
		{ID: "amb1", Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "m5.large", Savings: 10, CloudAccountID: nil},
		{ID: "reg1", Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "m5.xlarge", Savings: 20, CloudAccountID: &registeredAcctID},
	}
	require.NoError(t, store.UpsertRecommendations(ctx, t0, seed, []config.SuccessfulCollect{
		{Provider: "aws"}, // ambient (nil)
		{Provider: "aws", CloudAccountID: &registeredAcctID},
	}))

	// Partial collect at t1: only the registered account succeeded.
	t1 := t0.Add(time.Hour)
	partial := []config.RecommendationRecord{
		{ID: "reg1-v2", Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "m5.xlarge", Savings: 25, CloudAccountID: &registeredAcctID},
	}
	require.NoError(t, store.UpsertRecommendations(ctx, t1, partial, []config.SuccessfulCollect{
		{Provider: "aws", CloudAccountID: &registeredAcctID},
	}))

	got, err := store.ListStoredRecommendations(ctx, config.RecommendationFilter{Provider: "aws"})
	require.NoError(t, err)
	require.Len(t, got, 2, "ambient row must survive; registered row must be upserted")

	var ambientFound, registeredFound bool
	for _, rec := range got {
		if rec.CloudAccountID == nil {
			ambientFound = true
			assert.InDelta(t, 10.0, rec.Savings, 0.001, "ambient row should keep its t0 savings")
		} else {
			registeredFound = true
			assert.Equal(t, registeredAcctID, *rec.CloudAccountID)
			assert.InDelta(t, 25.0, rec.Savings, 0.001, "registered row should reflect upserted savings")
		}
	}
	assert.True(t, ambientFound, "ambient row must remain in DB after partial registered-only collect")
	assert.True(t, registeredFound, "registered row must be upserted")
}

// TestPostgresStore_ClearCollectionStarted_CompareAndClear pins the issue
// #261 compare-and-clear guard at the store level: a late clear from an
// abandoned run (run A) must not wipe the marker a later run (run B) now
// owns. Pre-fix, ClearCollectionStarted took no token and cleared
// unconditionally, so run A's deferred clear (arriving after run B had
// already started) would wipe run B's in-flight marker.
//
// Both runs here go through MarkCollectionStarted, which only the
// POST /api/recommendations/refresh handler calls, so run B models a second
// user-triggered refresh taking over after run A's 5-minute window lapses.
// The cron variant of the same race (a tokenless scheduler run clearing a
// user run's marker) cannot be expressed at this level because cron never
// marks; it is pinned in the scheduler by
// TestScheduler_CollectRecommendations_EmptyTokenSkipsClear.
func TestPostgresStore_ClearCollectionStarted_CompareAndClear(t *testing.T) {
	ctx := context.Background()
	container, err := testhelpers.SetupPostgresContainer(ctx, t)
	require.NoError(t, err)
	defer container.Cleanup(ctx)
	pool := container.DB.Pool()
	require.NoError(t, migrations.RunMigrations(ctx, pool, getMigrationsPath(), "", ""))
	store := config.NewPostgresStore(container.DB)

	// Run A wins the race and is stamped with a fresh owner token.
	tokenA, okA, err := store.MarkCollectionStarted(ctx)
	require.NoError(t, err)
	require.True(t, okA)
	_, uuidErr := uuid.Parse(tokenA)
	require.NoError(t, uuidErr, "MarkCollectionStarted must return a UUID token")

	// A concurrent Mark while A's marker is fresh is blocked (409 path).
	tokenNone, okBlocked, err := store.MarkCollectionStarted(ctx)
	require.NoError(t, err)
	assert.False(t, okBlocked)
	assert.Empty(t, tokenNone)

	// Force run A's marker stale (simulates its Lambda crashing or running
	// long past the 5-minute window), so the next refresh (run B) is allowed
	// to take the marker over.
	_, err = pool.Exec(ctx, `
		UPDATE recommendations_state
		   SET last_collection_started_at = NOW() - INTERVAL '6 minutes'
		 WHERE id = 1
	`)
	require.NoError(t, err)

	tokenB, okB, err := store.MarkCollectionStarted(ctx)
	require.NoError(t, err)
	require.True(t, okB)
	require.NotEqual(t, tokenA, tokenB, "run B must win its own distinct token")

	// The regression assertion: run A finally finishes (late, after its own
	// stale window) and calls Clear with its own token. Because
	// last_collection_owner_id now points at run B, this must be a silent
	// no-op: run B's in-flight marker must survive.
	require.NoError(t, store.ClearCollectionStarted(ctx, tokenA))
	freshness, err := store.GetRecommendationsFreshness(ctx)
	require.NoError(t, err)
	assert.NotNil(t, freshness.LastCollectionStartedAt,
		"run B's marker must survive run A's stale-token clear")

	// Run B's own clear matches the current owner and actually clears.
	require.NoError(t, store.ClearCollectionStarted(ctx, tokenB))
	freshness, err = store.GetRecommendationsFreshness(ctx)
	require.NoError(t, err)
	assert.Nil(t, freshness.LastCollectionStartedAt)

	// Empty token is a boundary error: only a caller that actually won
	// MarkCollectionStarted should ever call Clear.
	err = store.ClearCollectionStarted(ctx, "")
	require.Error(t, err)
}

// TestPostgresStore_SetRecommendationsCollectionError_DoesNotClearOwnerMarker
// pins a second issue #261 side door found in adversarial review:
// SetRecommendationsCollectionError is called mid-run from persistCollection
// on EVERY CollectRecommendations invocation that hits a provider error,
// including tokenless cron/cold-start/background runs. Pre-fix, it cleared
// last_collection_started_at unconditionally (no owner check), so a
// tokenless run's routine provider error would wipe a concurrent owner run's
// in-flight marker, reopening the exact race the compare-and-clear guard
// exists to close.
func TestPostgresStore_SetRecommendationsCollectionError_DoesNotClearOwnerMarker(t *testing.T) {
	ctx := context.Background()
	container, err := testhelpers.SetupPostgresContainer(ctx, t)
	require.NoError(t, err)
	defer container.Cleanup(ctx)
	pool := container.DB.Pool()
	require.NoError(t, migrations.RunMigrations(ctx, pool, getMigrationsPath(), "", ""))
	store := config.NewPostgresStore(container.DB)

	// Owner run A wins the race and is stamped with a fresh owner token.
	tokenA, okA, err := store.MarkCollectionStarted(ctx)
	require.NoError(t, err)
	require.True(t, okA)

	// A tokenless caller (cron/cold-start/background run hitting a routine
	// provider error) records the error. It must NOT touch run A's marker.
	require.NoError(t, store.SetRecommendationsCollectionError(ctx, "aws: transient throttling"))

	freshness, err := store.GetRecommendationsFreshness(ctx)
	require.NoError(t, err)
	require.NotNil(t, freshness.LastCollectionError)
	assert.Equal(t, "aws: transient throttling", *freshness.LastCollectionError)
	assert.NotNil(t, freshness.LastCollectionStartedAt,
		"run A's marker must survive a tokenless SetRecommendationsCollectionError call")

	// Run A's own token-guarded clear still works afterward.
	require.NoError(t, store.ClearCollectionStarted(ctx, tokenA))
	freshness, err = store.GetRecommendationsFreshness(ctx)
	require.NoError(t, err)
	assert.Nil(t, freshness.LastCollectionStartedAt)
}

func float64Ptr(f float64) *float64 { return &f }

func TestPostgresStore_UpsertRecommendations_ReplacesAzureWrongUnits(t *testing.T) {
	ctx := context.Background()
	store, cleanup := setupRecommendationsStore(ctx, t)
	defer cleanup()
	rec := config.RecommendationRecord{ID: "old-units", Provider: "azure", Service: "compute", Region: "westeurope", ResourceType: "Standard_D2s_v3", Count: 1, Term: 3, Payment: "all-upfront", UpfrontCost: 70, MonthlyCost: float64Ptr(10), Savings: 50}
	collects := []config.SuccessfulCollect{{Provider: "azure"}}
	now := time.Now().UTC()
	require.NoError(t, store.UpsertRecommendations(ctx, now, []config.RecommendationRecord{rec}, collects))
	rec.ID = "corrected"
	rec.UpfrontCost = 3600
	rec.MonthlyCost = nil
	require.NoError(t, store.UpsertRecommendations(ctx, now.Add(time.Minute), []config.RecommendationRecord{rec}, collects))
	got, err := store.ListStoredRecommendations(ctx, config.RecommendationFilter{Provider: "azure"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "corrected", got[0].ID)
	assert.Equal(t, 3600.0, got[0].UpfrontCost)
	assert.Nil(t, got[0].MonthlyCost)
}

func zonalAWSRec(az string, savings float64) config.RecommendationRecord {
	rec := awsRec("z-"+az, "ec2", "us-east-1", "m5.large", savings)
	rec.AvailabilityZone = az
	rec.Details = json.RawMessage(`{"instance_type":"m5.large","platform":"Linux/UNIX","tenancy":"default","scope":"Availability Zone","availability_zone":"` + az + `"}`)
	return rec
}

// Issue #792, PR2b: the upsert arbiter is the 9-column index that includes the
// generated availability_zone column. Same-AZ re-upserts update in place, a
// regional row (AZ ”) behaves as before, and a row written by pre-PR2b code
// (no top-level availability_zone in the payload) is found and updated.
// Multi-AZ rows on one 8-tuple are covered by
// TestPostgresStore_UpsertRecommendations_MultiAZBatch (PR2c).
func TestPostgresStore_UpsertRecommendations_AZArbiter(t *testing.T) {
	ctx := context.Background()
	container, err := testhelpers.SetupPostgresContainer(ctx, t)
	require.NoError(t, err)
	defer container.Cleanup(ctx)
	pool := container.DB.Pool()
	require.NoError(t, migrations.RunMigrations(ctx, pool, getMigrationsPath(), "", ""))
	store := config.NewPostgresStore(container.DB)
	t0 := time.Now().UTC().Truncate(time.Second)
	collect := []config.SuccessfulCollect{{Provider: "aws"}}

	t.Run("same-AZ re-upsert updates in place", func(t *testing.T) {
		require.NoError(t, store.UpsertRecommendations(ctx, t0, []config.RecommendationRecord{zonalAWSRec("us-east-1a", 10)}, collect))
		require.NoError(t, store.UpsertRecommendations(ctx, t0.Add(time.Hour), []config.RecommendationRecord{zonalAWSRec("us-east-1a", 25)}, collect))
		got, err := store.ListStoredRecommendations(ctx, config.RecommendationFilter{})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.InDelta(t, 25.0, got[0].Savings, 0.001)
		assert.Equal(t, "us-east-1a", got[0].AvailabilityZone)
		var az string
		require.NoError(t, pool.QueryRow(ctx, `SELECT availability_zone FROM recommendations`).Scan(&az))
		assert.Equal(t, "us-east-1a", az, "the generated column is derived from the payload")
	})

	t.Run("regional row unchanged", func(t *testing.T) {
		require.NoError(t, store.ReplaceRecommendations(ctx, t0, nil))
		require.NoError(t, store.UpsertRecommendations(ctx, t0, []config.RecommendationRecord{awsRec("r1", "ec2", "us-east-1", "c5.large", 10)}, collect))
		require.NoError(t, store.UpsertRecommendations(ctx, t0.Add(time.Hour), []config.RecommendationRecord{awsRec("r1", "ec2", "us-east-1", "c5.large", 30)}, collect))
		got, err := store.ListStoredRecommendations(ctx, config.RecommendationFilter{})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.InDelta(t, 30.0, got[0].Savings, 0.001)
		assert.Empty(t, got[0].AvailabilityZone)
	})

	t.Run("row written by pre-PR2b code is found and updated", func(t *testing.T) {
		require.NoError(t, store.ReplaceRecommendations(ctx, t0, nil))
		// What the old code stored: AZ, platform and tenancy only inside details (the
		// collector always sets all three on a zonal EC2 rec), 8-column semantics.
		_, err := pool.Exec(ctx, `
			INSERT INTO recommendations
			    (collected_at, provider, service, region, resource_type, engine, payload,
			     upfront_cost, monthly_savings, term, payment_option)
			VALUES ($1, 'aws', 'ec2', 'us-east-1', 'm5.large', '',
			        '{"details":{"platform":"Linux/UNIX","tenancy":"default","availability_zone":"us-east-1a"}}'::jsonb, 100, 5, 12, 'no-upfront')`, t0)
		require.NoError(t, err)
		require.NoError(t, store.UpsertRecommendations(ctx, t0.Add(time.Hour), []config.RecommendationRecord{zonalAWSRec("us-east-1a", 40)}, collect))
		var n int
		require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM recommendations`).Scan(&n))
		assert.Equal(t, 1, n, "the legacy row must be the conflict target, not duplicated")
		got, err := store.ListStoredRecommendations(ctx, config.RecommendationFilter{})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.InDelta(t, 40.0, got[0].Savings, 0.001)
	})
}

// Issue #806, PR-C: the upsert arbiter is the 11-column index that adds the
// generated platform and tenancy columns. A re-upsert on the same platform and
// tenancy updates in place, regional non-EC2 rows (platform and tenancy ”) are
// unchanged, and a row written by pre-PR-C code is found and updated. A batch
// of rows differing only by platform or tenancy is covered with the 9-column
// index drop (migration 000119).
func TestPostgresStore_UpsertRecommendations_PlatformTenancyArbiter(t *testing.T) {
	ctx := context.Background()
	container, err := testhelpers.SetupPostgresContainer(ctx, t)
	require.NoError(t, err)
	defer container.Cleanup(ctx)
	pool := container.DB.Pool()
	require.NoError(t, migrations.RunMigrations(ctx, pool, getMigrationsPath(), "", ""))
	store := config.NewPostgresStore(container.DB)
	t0 := time.Now().UTC().Truncate(time.Second)
	collect := []config.SuccessfulCollect{{Provider: "aws"}}

	ec2 := func(platform, tenancy string, savings float64) config.RecommendationRecord {
		rec := awsRec("p-"+platform+tenancy, "ec2", "us-east-1", "m5.large", savings)
		rec.Details = json.RawMessage(`{"instance_type":"m5.large","platform":"` + platform + `","tenancy":"` + tenancy + `"}`)
		return rec
	}
	columns := func(t *testing.T) (platform, tenancy string, n int) {
		t.Helper()
		require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM recommendations`).Scan(&n))
		require.NoError(t, pool.QueryRow(ctx, `SELECT platform, tenancy FROM recommendations LIMIT 1`).Scan(&platform, &tenancy))
		return platform, tenancy, n
	}

	t.Run("same platform and tenancy re-upsert updates in place", func(t *testing.T) {
		require.NoError(t, store.UpsertRecommendations(ctx, t0, []config.RecommendationRecord{ec2("Windows", "dedicated", 10)}, collect))
		require.NoError(t, store.UpsertRecommendations(ctx, t0.Add(time.Hour), []config.RecommendationRecord{ec2("Windows", "dedicated", 25)}, collect))
		got, err := store.ListStoredRecommendations(ctx, config.RecommendationFilter{})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.InDelta(t, 25.0, got[0].Savings, 0.001)
		platform, tenancy, n := columns(t)
		assert.Equal(t, 1, n)
		assert.Equal(t, "Windows", platform, "the generated column is derived from the payload")
		assert.Equal(t, "dedicated", tenancy)
	})

	t.Run("regional non-EC2 row has empty platform and tenancy and updates in place", func(t *testing.T) {
		require.NoError(t, store.ReplaceRecommendations(ctx, t0, nil))
		rds := awsRec("r1", "rds", "us-east-1", "db.m5.large", 10)
		require.NoError(t, store.UpsertRecommendations(ctx, t0, []config.RecommendationRecord{rds}, collect))
		rds.Savings = 30
		require.NoError(t, store.UpsertRecommendations(ctx, t0.Add(time.Hour), []config.RecommendationRecord{rds}, collect))
		got, err := store.ListStoredRecommendations(ctx, config.RecommendationFilter{})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.InDelta(t, 30.0, got[0].Savings, 0.001)
		platform, tenancy, n := columns(t)
		assert.Equal(t, 1, n)
		assert.Empty(t, platform)
		assert.Empty(t, tenancy)
	})

	t.Run("row written by pre-PR-C code is found and updated", func(t *testing.T) {
		require.NoError(t, store.ReplaceRecommendations(ctx, t0, nil))
		// What the old code stored: platform and tenancy only inside details.
		_, err := pool.Exec(ctx, `
			INSERT INTO recommendations
			    (collected_at, provider, service, region, resource_type, engine, payload,
			     upfront_cost, monthly_savings, term, payment_option)
			VALUES ($1, 'aws', 'ec2', 'us-east-1', 'm5.large', '',
			        '{"details":{"platform":"Linux/UNIX","tenancy":"default"}}'::jsonb, 100, 5, 12, 'no-upfront')`, t0)
		require.NoError(t, err)
		require.NoError(t, store.UpsertRecommendations(ctx, t0.Add(time.Hour), []config.RecommendationRecord{ec2("Linux/UNIX", "default", 40)}, collect))
		got, err := store.ListStoredRecommendations(ctx, config.RecommendationFilter{})
		require.NoError(t, err)
		require.Len(t, got, 1, "the legacy row must be the conflict target, not duplicated")
		assert.InDelta(t, 40.0, got[0].Savings, 0.001)
	})
}

// Issue #792, PR2c: once migration 000110 drops the 8-column index, two zonal
// EC2 recommendations that differ only by availability zone upsert in ONE batch
// (the shape persistCollection writes) without "ON CONFLICT DO UPDATE command
// cannot affect row a second time", and both rows persist.
func TestPostgresStore_UpsertRecommendations_MultiAZBatch(t *testing.T) {
	ctx := context.Background()
	container, err := testhelpers.SetupPostgresContainer(ctx, t)
	require.NoError(t, err)
	defer container.Cleanup(ctx)
	pool := container.DB.Pool()
	require.NoError(t, migrations.RunMigrations(ctx, pool, getMigrationsPath(), "", ""))
	store := config.NewPostgresStore(container.DB)
	t0 := time.Now().UTC().Truncate(time.Second)
	collect := []config.SuccessfulCollect{{Provider: "aws"}}

	var oldIdx bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'recommendations_natural_key_idx')`).Scan(&oldIdx))
	require.False(t, oldIdx, "8-column index must be gone after the full migration chain")
	var nineColIdx bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'recommendations_natural_key_az_idx')`).Scan(&nineColIdx))
	require.False(t, nineColIdx, "9-column index must be gone after the full migration chain (000119)")

	savingsByAZ := func(t *testing.T) map[string]float64 {
		t.Helper()
		got, err := store.ListStoredRecommendations(ctx, config.RecommendationFilter{})
		require.NoError(t, err)
		out := map[string]float64{}
		for _, r := range got {
			out[r.AvailabilityZone] = r.Savings
		}
		require.Len(t, out, len(got), "no two stored rows may share an AZ here")
		return out
	}

	t.Run("two zonal rows in one batch both persist", func(t *testing.T) {
		batch := []config.RecommendationRecord{zonalAWSRec("us-east-1a", 10), zonalAWSRec("us-east-1b", 20)}
		require.NoError(t, store.UpsertRecommendations(ctx, t0, batch, collect))
		assert.Equal(t, map[string]float64{"us-east-1a": 10, "us-east-1b": 20}, savingsByAZ(t))
	})

	t.Run("same-AZ re-upsert updates in place and leaves the sibling alone", func(t *testing.T) {
		require.NoError(t, store.UpsertRecommendations(ctx, t0.Add(time.Hour),
			[]config.RecommendationRecord{zonalAWSRec("us-east-1a", 11), zonalAWSRec("us-east-1b", 20)}, collect))
		assert.Equal(t, map[string]float64{"us-east-1a": 11, "us-east-1b": 20}, savingsByAZ(t))
	})

	t.Run("regional row alongside zonal rows is unaffected", func(t *testing.T) {
		regional := awsRec("r1", "ec2", "us-east-1", "m5.large", 30)
		batch := []config.RecommendationRecord{zonalAWSRec("us-east-1a", 12), zonalAWSRec("us-east-1b", 21), regional}
		require.NoError(t, store.UpsertRecommendations(ctx, t0.Add(2*time.Hour), batch, collect))
		assert.Equal(t, map[string]float64{"us-east-1a": 12, "us-east-1b": 21, "": 30}, savingsByAZ(t))
	})

	t.Run("ReplaceRecommendations stores the same AZ pair", func(t *testing.T) {
		batch := []config.RecommendationRecord{zonalAWSRec("us-east-1a", 1), zonalAWSRec("us-east-1b", 2)}
		require.NoError(t, store.ReplaceRecommendations(ctx, t0.Add(3*time.Hour), batch))
		assert.Equal(t, map[string]float64{"us-east-1a": 1, "us-east-1b": 2}, savingsByAZ(t))
	})
}

// Issue #806, PR-E: once migration 000119 drops the 9-column index, EC2 recs
// that differ only by platform or tenancy upsert in ONE batch (the shape
// persistCollection writes) without a unique violation, and both rows persist.
func TestPostgresStore_UpsertRecommendations_PlatformTenancyBatch(t *testing.T) {
	ctx := context.Background()
	container, err := testhelpers.SetupPostgresContainer(ctx, t)
	require.NoError(t, err)
	defer container.Cleanup(ctx)
	pool := container.DB.Pool()
	require.NoError(t, migrations.RunMigrations(ctx, pool, getMigrationsPath(), "", ""))
	store := config.NewPostgresStore(container.DB)
	t0 := time.Now().UTC().Truncate(time.Second)
	collect := []config.SuccessfulCollect{{Provider: "aws"}}

	ec2 := func(platform, tenancy string, savings float64) config.RecommendationRecord {
		rec := awsRec("p-"+platform+tenancy, "ec2", "us-east-1", "m5.large", savings)
		rec.Details = json.RawMessage(`{"instance_type":"m5.large","platform":"` + platform + `","tenancy":"` + tenancy + `"}`)
		return rec
	}
	savingsByKey := func(t *testing.T) map[string]float64 {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT platform || '|' || tenancy, monthly_savings FROM recommendations`)
		require.NoError(t, err)
		defer rows.Close()
		out := map[string]float64{}
		for rows.Next() {
			var k string
			var v float64
			require.NoError(t, rows.Scan(&k, &v))
			out[k] = v
		}
		require.NoError(t, rows.Err())
		return out
	}
	reset := func(t *testing.T) {
		t.Helper()
		require.NoError(t, store.ReplaceRecommendations(ctx, t0, nil))
	}

	t.Run("two rows differing only by platform in one batch both persist", func(t *testing.T) {
		reset(t)
		require.NoError(t, store.UpsertRecommendations(ctx, t0,
			[]config.RecommendationRecord{ec2("Linux/UNIX", "default", 10), ec2("Windows", "default", 20)}, collect))
		assert.Equal(t, map[string]float64{"Linux/UNIX|default": 10, "Windows|default": 20}, savingsByKey(t))
	})

	t.Run("two rows differing only by tenancy in one batch both persist", func(t *testing.T) {
		reset(t)
		require.NoError(t, store.UpsertRecommendations(ctx, t0,
			[]config.RecommendationRecord{ec2("Linux/UNIX", "default", 10), ec2("Linux/UNIX", "dedicated", 20)}, collect))
		assert.Equal(t, map[string]float64{"Linux/UNIX|default": 10, "Linux/UNIX|dedicated": 20}, savingsByKey(t))
	})

	t.Run("same-key re-upsert updates in place and leaves the sibling alone", func(t *testing.T) {
		require.NoError(t, store.UpsertRecommendations(ctx, t0.Add(time.Hour),
			[]config.RecommendationRecord{ec2("Linux/UNIX", "default", 11), ec2("Linux/UNIX", "dedicated", 20)}, collect))
		assert.Equal(t, map[string]float64{"Linux/UNIX|default": 11, "Linux/UNIX|dedicated": 20}, savingsByKey(t))
	})

	t.Run("regional non-EC2 row alongside EC2 platform rows is unaffected", func(t *testing.T) {
		reset(t)
		rds := awsRec("r1", "rds", "us-east-1", "db.m5.large", 30)
		batch := []config.RecommendationRecord{ec2("Linux/UNIX", "default", 10), ec2("Windows", "default", 20), rds}
		require.NoError(t, store.UpsertRecommendations(ctx, t0, batch, collect))
		assert.Equal(t, map[string]float64{"Linux/UNIX|default": 10, "Windows|default": 20, "|": 30}, savingsByKey(t))
	})

	t.Run("cross-run Linux then Windows both persist", func(t *testing.T) {
		// The transitional case that failed with SQLSTATE 23505 while the
		// 9-column index was live: one collection stored Linux, a later one
		// adds Windows on the same 9-column key (a successful collection sweeps
		// rows it does not re-send, so the later batch carries both).
		reset(t)
		require.NoError(t, store.UpsertRecommendations(ctx, t0, []config.RecommendationRecord{ec2("Linux/UNIX", "default", 10)}, collect))
		require.NoError(t, store.UpsertRecommendations(ctx, t0.Add(time.Hour), []config.RecommendationRecord{ec2("Linux/UNIX", "default", 10), ec2("Windows", "default", 20)}, collect))
		assert.Equal(t, map[string]float64{"Linux/UNIX|default": 10, "Windows|default": 20}, savingsByKey(t))
	})
}
