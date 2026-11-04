//go:build integration

package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	azureprovider "github.com/LeanerCloud/cloud-commitments-go/providers/azure"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func heldPartial(ids ...string) *config.RecommendationsCollectionPartial {
	return &config.RecommendationsCollectionPartial{Providers: []config.PartialProviderSweep{{
		Provider: "azure", IncompleteAccounts: len(ids), HeldAccountIDs: ids,
		Reasons: map[string]int{config.PartialReasonPricingUnavailable: 1},
	}}}
}

func heldAzureAccounts(t *testing.T, store config.StoreInterface) []string {
	t.Helper()
	fresh, err := store.GetRecommendationsFreshness(context.Background())
	require.NoError(t, err)
	if fresh.LastCollectionPartial == nil {
		return nil
	}
	for _, sweep := range fresh.LastCollectionPartial.Providers {
		if sweep.Provider == "azure" && sweep.Reasons[config.PartialReasonPricingUnavailable] > 0 {
			return sweep.HeldAccountIDs
		}
	}
	return nil
}

func registerAzure(t *testing.T, store config.StoreInterface) string {
	t.Helper()
	acct := &config.CloudAccount{Name: "azure-" + uuid.NewString(), Provider: "azure", ExternalID: uuid.NewString(), Enabled: true}
	require.NoError(t, store.CreateCloudAccount(context.Background(), acct))
	return acct.ID
}

// seedAzureAccountRows stores an old Azure row for each account, the rows a
// hold protects.
func seedAzureAccountRows(t *testing.T, store config.StoreInterface, accounts ...string) {
	t.Helper()
	recs := make([]config.RecommendationRecord, 0, len(accounts))
	for _, id := range accounts {
		recs = append(recs, config.RecommendationRecord{ID: "row-" + id, CloudAccountID: &id, Provider: "azure", Service: "compute",
			Region: "westeurope", ResourceType: "Standard_D2s_v3", Savings: 40, Count: 1, Term: 12, Payment: "upfront"})
	}
	require.NoError(t, store.UpsertRecommendations(context.Background(), time.Now().Add(-time.Hour), recs, nil))
}

// The hold marker lasts as long as the held rows: it survives every cycle that
// does not re-price the account, and is dropped only by a cycle that does.
func TestAzureHoldCarriedAcrossCycles(t *testing.T) {
	ctx := context.Background()
	var acctA, acctB string
	newScheduler := func(t *testing.T) (*Scheduler, config.StoreInterface) {
		container := testhelpers.RequireMigratedDatabase(ctx, t, "../database/postgres/migrations")
		store := config.NewPostgresStore(container.DB)
		acctA, acctB = registerAzure(t, store), registerAzure(t, store)
		seedAzureAccountRows(t, store, acctA, acctB)
		s := &Scheduler{config: store}
		s.persistCollection(ctx, nil, nil, nil, heldPartial(acctA))
		require.Equal(t, []string{acctA}, heldAzureAccounts(t, store))
		return s, store
	}
	azureOK := func(id string) []config.SuccessfulCollect { return expandSuccessfulCollects("azure", []string{id}) }

	t.Run("outright Azure failure keeps the hold", func(t *testing.T) {
		s, store := newScheduler(t)
		s.persistCollection(ctx, nil, nil, map[string]string{"azure": "boom"}, nil)
		require.Equal(t, []string{acctA}, heldAzureAccounts(t, store))
	})

	t.Run("run without Azure keeps the hold", func(t *testing.T) {
		s, store := newScheduler(t)
		s.persistCollection(ctx, nil, expandSuccessfulCollects("aws", []string{""}), nil, nil)
		require.Equal(t, []string{acctA}, heldAzureAccounts(t, store))
	})

	t.Run("an outage on another account holds both", func(t *testing.T) {
		s, store := newScheduler(t)
		s.persistCollection(ctx, nil, nil, nil, heldPartial(acctB))
		require.ElementsMatch(t, []string{acctA, acctB}, heldAzureAccounts(t, store))
	})

	t.Run("a good cycle for another account keeps this hold", func(t *testing.T) {
		s, store := newScheduler(t)
		s.persistCollection(ctx, nil, azureOK(acctB), nil, nil)
		require.Equal(t, []string{acctA}, heldAzureAccounts(t, store))
	})

	t.Run("a good cycle for the account clears it", func(t *testing.T) {
		s, store := newScheduler(t)
		s.persistCollection(ctx, nil, azureOK(acctA), nil, nil)
		require.Empty(t, heldAzureAccounts(t, store))
	})

	t.Run("a good cycle re-prices one of two held accounts", func(t *testing.T) {
		s, store := newScheduler(t)
		s.persistCollection(ctx, nil, nil, nil, heldPartial(acctB))
		s.persistCollection(ctx, nil, azureOK(acctA), nil, nil)
		require.Equal(t, []string{acctB}, heldAzureAccounts(t, store))
	})

	t.Run("a deleted account's hold drops", func(t *testing.T) {
		s, store := newScheduler(t)
		require.NoError(t, store.DeleteCloudAccount(ctx, acctA))
		s.persistCollection(ctx, nil, azureOK(acctB), nil, nil)
		require.Empty(t, heldAzureAccounts(t, store))
	})

	t.Run("a disabled account with rows stays held", func(t *testing.T) {
		s, store := newScheduler(t)
		acct, err := store.GetCloudAccount(ctx, acctA)
		require.NoError(t, err)
		acct.Enabled = false
		require.NoError(t, store.UpdateCloudAccount(ctx, acct))
		s.persistCollection(ctx, nil, azureOK(acctB), nil, nil)
		require.Equal(t, []string{acctA}, heldAzureAccounts(t, store))
	})

	ambientHold := func(t *testing.T, withRows bool) (*Scheduler, config.StoreInterface) {
		container := testhelpers.RequireMigratedDatabase(ctx, t, "../database/postgres/migrations")
		store := config.NewPostgresStore(container.DB)
		if withRows {
			seedAdvisorRow(t, store)
		}
		s := &Scheduler{config: store}
		s.persistCollection(ctx, nil, nil, nil, heldPartial())
		fresh, err := store.GetRecommendationsFreshness(ctx)
		require.NoError(t, err)
		require.NotNil(t, fresh.LastCollectionPartial)
		return s, store
	}

	t.Run("an ambient hold stays while ambient rows exist, even once accounts are registered", func(t *testing.T) {
		s, store := ambientHold(t, true)
		s.persistCollection(ctx, nil, azureOK(registerAzure(t, store)), nil, nil)
		fresh, err := store.GetRecommendationsFreshness(ctx)
		require.NoError(t, err)
		require.NotNil(t, fresh.LastCollectionPartial)
	})

	t.Run("an ambient hold drops once no ambient rows remain", func(t *testing.T) {
		s, store := ambientHold(t, false)
		s.persistCollection(ctx, nil, azureOK(registerAzure(t, store)), nil, nil)
		fresh, err := store.GetRecommendationsFreshness(ctx)
		require.NoError(t, err)
		require.Nil(t, fresh.LastCollectionPartial)
	})

	t.Run("an unresolved ambient hold survives an outright failure", func(t *testing.T) {
		container := testhelpers.RequireMigratedDatabase(ctx, t, "../database/postgres/migrations")
		store := config.NewPostgresStore(container.DB)
		s := &Scheduler{config: store}
		seedAdvisorRow(t, store)
		collectAzureAmbientCycle(t, store, nil, &azureprovider.PartialResultError{Skipped: advisorOutageSkips})
		s.persistCollection(ctx, nil, nil, map[string]string{"azure": "boom"}, nil)
		fresh, err := store.GetRecommendationsFreshness(ctx)
		require.NoError(t, err)
		require.NotNil(t, fresh.LastCollectionPartial, "no registered account to name, but the hold must remain")
		collectAzureAmbientCycle(t, store, []common.Recommendation{partialSweepRec()}, nil)
		fresh, err = store.GetRecommendationsFreshness(ctx)
		require.NoError(t, err)
		require.Nil(t, fresh.LastCollectionPartial)
	})
}

// The rows and the hold marker are written in one transaction: when writing the
// new marker fails, the upsert rolls back too, so the old rows and the old hold
// both stay. Before, the upsert committed and reset the marker first, and a
// failing second write left the held rows purchasable.
func TestPersistCollection_FailedMarkerWriteRollsBackUpsert(t *testing.T) {
	ctx := context.Background()
	container := testhelpers.RequireMigratedDatabase(ctx, t, "../database/postgres/migrations")
	store := config.NewPostgresStore(container.DB)
	s := &Scheduler{config: store}
	acctA := registerAzure(t, store)
	seedAdvisorRow(t, store)
	s.persistCollection(ctx, nil, nil, nil, heldPartial(acctA))
	require.Equal(t, []string{acctA}, heldAzureAccounts(t, store))

	_, err := container.DB.Exec(ctx, `
		CREATE FUNCTION fail_poisoned_marker() RETURNS trigger AS $$
		BEGIN
			IF NEW.last_collection_partial::text LIKE '%poison%' THEN
				RAISE EXCEPTION 'injected marker write failure';
			END IF;
			RETURN NEW;
		END $$ LANGUAGE plpgsql;
		CREATE TRIGGER fail_poisoned_marker BEFORE UPDATE ON recommendations_state
			FOR EACH ROW EXECUTE FUNCTION fail_poisoned_marker();`)
	require.NoError(t, err)

	newRow := config.RecommendationRecord{ID: "new-row", Provider: "azure", Service: "compute", Region: "eastus", ResourceType: "Standard_D4s_v3",
		Savings: 10, Count: 1, Term: 12, Payment: "upfront"}
	s.persistCollection(ctx, []config.RecommendationRecord{newRow}, nil, nil, heldPartial("poison"))

	require.Equal(t, []string{"prev-advisor-row"}, storedAzureIDs(t, store), "the upsert must roll back with the marker")
	require.Equal(t, []string{acctA}, heldAzureAccounts(t, store), "the old hold must survive a failed marker write")
}
