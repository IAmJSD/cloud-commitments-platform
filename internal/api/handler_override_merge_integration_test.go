//go:build integration

package api

// Real-Postgres tests for the account service override PUT merge (issue
// #1775): a single-field PUT keeps every other stored field, and two
// concurrent single-field PUTs both persist. A mock cannot hold the row and
// advisory locks the race depends on.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// overrideMergeRaceRounds is how many concurrent PUT pairs the race test fires.
const overrideMergeRaceRounds = 40

func fullOverrideFor(accountID string) *config.AccountServiceOverride {
	o := fullStoredOverride()
	o.ID, o.AccountID, o.CreatedAt = "", accountID, o.CreatedAt.UTC()
	return o
}

// A single-field PUT over a fully populated row, through the real store,
// changes that field only.
func TestOverrideMerge_SingleKeyPutKeepsOtherFieldsInStore(t *testing.T) {
	ctx := context.Background()
	f := newOverrideAuditFixture(ctx, t)
	seed := fullOverrideFor(f.acct.ID)
	require.NoError(t, f.store.SaveAccountServiceOverride(ctx, seed))

	f.put(ctx, t, "rds", `{"term":3}`)

	got, err := f.store.GetAccountServiceOverride(ctx, f.acct.ID, "aws", "rds")
	require.NoError(t, err)
	require.NotNil(t, got)
	want := fullOverrideFor(f.acct.ID)
	want.Term = ovPtr(3)
	assert.Equal(t, want.Enabled, got.Enabled)
	assert.Equal(t, want.Term, got.Term)
	assert.Equal(t, want.Payment, got.Payment)
	assert.Equal(t, want.Coverage, got.Coverage)
	assert.Equal(t, want.RampSchedule, got.RampSchedule)
	assert.Equal(t, want.IncludeEngines, got.IncludeEngines)
	assert.Equal(t, want.ExcludeEngines, got.ExcludeEngines)
	assert.Equal(t, want.IncludeRegions, got.IncludeRegions)
	assert.Equal(t, want.ExcludeRegions, got.ExcludeRegions)
	assert.Equal(t, want.IncludeTypes, got.IncludeTypes)
	assert.Equal(t, want.ExcludeTypes, got.ExcludeTypes)
}

// The reviewer's repro for #1775 (PR #1788): reset the row to term 1,
// all-upfront and exclude_regions [eu-west-1], then PUT {"exclude_regions":
// ["r-N"]} and {"term":3} at once. Both return 200; unless the read-merge-write
// is one locked transaction, one change is lost every round (an exclusion
// silently reverted widens what can be bought).
func TestOverrideMerge_ConcurrentSingleKeyPutsKeepBothKeys(t *testing.T) {
	ctx := context.Background()
	f := newOverrideAuditFixture(ctx, t)

	lost := 0
	for round := 1; round <= overrideMergeRaceRounds; round++ {
		require.NoError(t, f.store.SaveAccountServiceOverride(ctx, &config.AccountServiceOverride{
			AccountID: f.acct.ID, Provider: "aws", Service: "rds",
			Term: ovPtr(1), Payment: ovPtr("all-upfront"), ExcludeRegions: []string{"eu-west-1"},
		}))
		region := fmt.Sprintf("r-%d", round)

		bodies := []string{fmt.Sprintf(`{"exclude_regions":[%q]}`, region), `{"term":3}`}
		start := make(chan struct{})
		errs := make([]error, len(bodies))
		var wg sync.WaitGroup
		for i, body := range bodies {
			wg.Add(1)
			go func(i int, body string) {
				defer wg.Done()
				<-start
				_, errs[i] = f.h.saveAccountServiceOverride(ctx, adminRequest(body), f.acct.ID+"/service-overrides/aws/rds")
			}(i, body)
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			require.NoError(t, err, "PUT %d (%s)", i, bodies[i])
		}

		got, err := f.store.GetAccountServiceOverride(ctx, f.acct.ID, "aws", "rds")
		require.NoError(t, err)
		if got.Term == nil || *got.Term != 3 || len(got.ExcludeRegions) != 1 || got.ExcludeRegions[0] != region {
			lost++
			t.Logf("round %d: term=%v (want 3) exclude_regions=%v (want [%s])", round, got.Term, got.ExcludeRegions, region)
		}
		require.Equal(t, ovPtr("all-upfront"), got.Payment, "round %d: untouched field must survive", round)
	}
	require.Zero(t, lost, "%d of %d rounds lost one of two concurrent PUTs", lost, overrideMergeRaceRounds)
}

// Two spellings of one account id must take the same lock: with the raw path id
// in the lock key, a first-time override raced by a lower- and an upper-case PUT
// merged both onto an empty row and the second write NULLed the first's field.
func TestOverrideMerge_ConcurrentFirstPutsAcrossIDSpellingsKeepBothKeys(t *testing.T) {
	ctx := context.Background()
	f := newOverrideAuditFixture(ctx, t)

	lost := 0
	for round := 1; round <= overrideMergeRaceRounds; round++ {
		_, err := f.pg.DB.Pool().Exec(ctx, `DELETE FROM account_service_overrides WHERE account_id = $1`, f.acct.ID)
		require.NoError(t, err)
		region := fmt.Sprintf("r-%d", round)
		paths := []string{f.acct.ID, strings.ToUpper(f.acct.ID)}
		bodies := []string{fmt.Sprintf(`{"exclude_regions":[%q]}`, region), `{"term":3}`}

		start := make(chan struct{})
		errs := make([]error, len(bodies))
		var wg sync.WaitGroup
		for i := range bodies {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, errs[i] = f.h.saveAccountServiceOverride(ctx, adminRequest(bodies[i]), paths[i]+"/service-overrides/aws/rds")
			}(i)
		}
		close(start)
		wg.Wait()
		for i, e := range errs {
			require.NoError(t, e, "PUT %d (%s)", i, bodies[i])
		}

		got, err := f.store.GetAccountServiceOverride(ctx, f.acct.ID, "aws", "rds")
		require.NoError(t, err)
		if got.Term == nil || *got.Term != 3 || len(got.ExcludeRegions) != 1 || got.ExcludeRegions[0] != region {
			lost++
			t.Logf("round %d: term=%v exclude_regions=%v (want 3, [%s])", round, got.Term, got.ExcludeRegions, region)
		}
	}
	require.Zero(t, lost, "%d of %d rounds lost one of two concurrent first PUTs", lost, overrideMergeRaceRounds)
}
