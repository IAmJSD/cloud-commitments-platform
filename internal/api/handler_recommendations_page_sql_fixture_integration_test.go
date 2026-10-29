//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/scheduler"
)

// pageSQLFixture is a recommendations cache built to exercise every filter
// the SQL page read re-implements (issue #981): registered and missing
// accounts, three providers, legacy and canonical service slugs,
// Savings Plans, empty regions, engines, every shape of the savings-pct
// inputs, fractional dollar amounts (so a summation order change would show),
// IDs whose byte order differs from linguistic order, suppressions with and
// without variants (including a legacy "shared" tenancy and unreadable
// Details), service configs and account overrides.
type pageSQLFixture struct {
	store    *config.PostgresStore
	sched    *scheduler.Scheduler
	h        *Handler
	accounts []config.CloudAccount // alpha, beta, gamma (registered)
	unreg    string                // an account ID no cloud_accounts row has (scope-only: rows carry registered accounts by FK)
	recs     []config.RecommendationRecord
}

func newPageSQLFixture(ctx context.Context, t *testing.T) *pageSQLFixture {
	t.Helper()
	pg := testhelpers.RequireMigratedDatabase(ctx, t, "../database/postgres/migrations")
	f := &pageSQLFixture{store: config.NewPostgresStore(pg.DB), unreg: uuid.NewString()}
	for i, name := range []string{"alpha", "beta", "gamma"} {
		a := config.CloudAccount{ID: uuid.NewString(), Name: name, Provider: "aws", ExternalID: fmt.Sprintf("11111111111%d", i), Enabled: true}
		require.NoError(t, f.store.CreateCloudAccount(ctx, &a))
		f.accounts = append(f.accounts, a)
	}
	f.recs = f.buildRecs()
	require.NoError(t, f.store.UpsertRecommendations(ctx, time.Now().UTC(), f.recs, nil))
	f.seedSuppressions(ctx, t, pg)
	f.seedOverrides(ctx, t)
	f.sched = scheduler.NewScheduler(scheduler.SchedulerConfig{ConfigStore: f.store, IsLambda: true})
	f.h = &Handler{config: f.store, scheduler: f.sched}
	return f
}

func pfloat(v float64) *float64 { return &v }

// pick returns one of options, chosen by rng.
func pick[T any](rng *rand.Rand, options ...T) T { return options[rng.Intn(len(options))] }

func (f *pageSQLFixture) buildRecs() []config.RecommendationRecord {
	rng := rand.New(rand.NewSource(981))
	accounts := []*string{nil, &f.accounts[0].ID, &f.accounts[1].ID, &f.accounts[2].ID}
	type svc struct{ provider, service string }
	services := []svc{
		{"aws", "compute"}, {"aws", "ec2"}, {"aws", "rds"}, {"aws", "relational-db"},
		{"aws", "savings-plans-compute"}, {"aws", "savingsplans-compute"}, {"azure", "compute"}, {"gcp", "compute"},
	}
	prefixes := []string{"aws", "AWS", "aws-x", "aws x", "élan", "elan", "_z", "Zeta"}
	seen := map[string]bool{}
	var recs []config.RecommendationRecord
	for i := 0; len(recs) < 900; i++ {
		s := pick(rng, services...)
		rec := config.RecommendationRecord{
			ID:             fmt.Sprintf("%s|%05d", pick(rng, prefixes...), rng.Intn(100000)),
			Provider:       s.provider,
			Service:        s.service,
			Region:         pick(rng, "us-east-1", "us-west-2", "eu-west-1", ""),
			ResourceType:   pick(rng, "m5.large", "c5.xlarge", "db.r6g.large"),
			Engine:         pick(rng, "", "", "mysql", "postgres"),
			Count:          1 + rng.Intn(12),
			Term:           pick(rng, 0, 1, 1, 3),
			Payment:        pick(rng, "no-upfront", "all-upfront", "partial-upfront"),
			Savings:        float64(rng.Intn(500000)) / 7,
			UpfrontCost:    pick(rng, 0, float64(rng.Intn(9000000))/13),
			CloudAccountID: pick(rng, accounts...),
		}
		f.setPctInputs(rng, &rec)
		if s.service == "compute" || s.service == "ec2" {
			rec.Details = json.RawMessage(pick(rng,
				`{"platform":"Linux/UNIX","tenancy":"default"}`, `{"platform":"Linux/UNIX","tenancy":"shared"}`,
				`{"platform":"Windows","tenancy":"dedicated"}`, `"unreadable"`, `{"availability_zone":"us-east-1a"}`, ``))
			rec.AvailabilityZone = pick(rng, "", "", "us-east-1b")
		}
		key := naturalKeyOf(&rec)
		if seen[key] || seen["id:"+rec.ID] {
			continue
		}
		seen[key], seen["id:"+rec.ID] = true, true
		recs = append(recs, rec)
	}
	return append(recs, pctBoundaryRecs()...)
}

// pctBoundaryRecs sit exactly on the fixture's pct floors: a provider
// percentage equal to the floor (kept, as Go's check is "pct < floor"), and
// 57/100*100, which float64 rounds to 56.99999999999999 (dropped at a floor
// of 57 by Go, kept by exact decimal arithmetic), through both the on-demand
// and the reconstructed baseline.
func pctBoundaryRecs() []config.RecommendationRecord {
	mk := func(id, provider, service string, count int) config.RecommendationRecord {
		return config.RecommendationRecord{ID: id, Provider: provider, Service: service, Region: "us-east-1",
			ResourceType: "boundary-" + id, Engine: "postgres", Count: count, Term: 1, Payment: "no-upfront"}
	}
	at20 := mk("bound|20", "aws", "rds", 5)
	at20.SavingsPercentage, at20.Savings = pfloat(20), 300
	at35 := mk("bound|35", "aws", "rds", 5)
	at35.SavingsPercentage, at35.Savings = pfloat(35), 200
	at57 := mk("bound|57", "aws", "rds", 5)
	at57.SavingsPercentage = pfloat(57)
	od57 := mk("bound|od57", "aws", "rds", 5)
	od57.Savings, od57.OnDemandCost = 57, pfloat(100)
	mc57 := mk("bound|mc57", "gcp", "compute", 7)
	mc57.Savings, mc57.MonthlyCost = 57, pfloat(43)
	return []config.RecommendationRecord{at20, at35, at57, od57, mc57}
}

// naturalKeyOf mirrors the cache's natural key, so the fixture never upserts
// two rows onto one key: the account (nil and missing collapse), the
// identity columns, and availability zone, platform and tenancy read
// leniently from Details as the generated columns do.
func naturalKeyOf(rec *config.RecommendationRecord) string {
	account := ""
	if rec.CloudAccountID != nil {
		account = *rec.CloudAccountID
	}
	var d struct {
		Platform string `json:"platform"`
		Tenancy  string `json:"tenancy"`
		AZ       string `json:"availability_zone"`
	}
	_ = json.Unmarshal(rec.Details, &d) // a non-object reads as no platform or tenancy
	az := rec.AvailabilityZone
	if az == "" {
		az = d.AZ
	}
	return fmt.Sprintf("%s|%s|%s|%s|%s|%s|%d|%s|%s|%s|%s", account, rec.Provider, rec.Service, rec.Region,
		rec.ResourceType, rec.Engine, rec.Term, rec.Payment, az, d.Platform, d.Tenancy)
}

// setPctInputs covers each branch of the savings-pct computation: a provider
// percentage, an on-demand baseline (positive or zero), a reconstructed
// baseline from monthly cost, and AWS rows without a baseline.
func (f *pageSQLFixture) setPctInputs(rng *rand.Rand, rec *config.RecommendationRecord) {
	switch rng.Intn(5) {
	case 0:
		rec.SavingsPercentage = pfloat(float64(rng.Intn(9000)) / 100)
	case 1:
		rec.OnDemandCost = pfloat(rec.Savings * float64(1+rng.Intn(9)) / 3)
	case 2:
		rec.OnDemandCost = pfloat(0)
		rec.MonthlyCost = pfloat(float64(rng.Intn(400000)) / 9)
	case 3:
		rec.MonthlyCost = pfloat(float64(rng.Intn(400000)) / 9)
	}
}

// seedSuppressions adds active suppressions on the 6-tuples of some recs:
// matching and non-matching variants, legacy rows without a variant, the
// legacy "ec2" slug against "compute" recs, counts below and above the rec's
// count, and an expired row that must not apply.
func (f *pageSQLFixture) seedSuppressions(ctx context.Context, t *testing.T, pg *testhelpers.PostgresContainer) {
	t.Helper()
	rng := rand.New(rand.NewSource(1981))
	for n := 0; n < 60; n++ {
		rec := f.recs[rng.Intn(len(f.recs))]
		account := ""
		if rec.CloudAccountID != nil {
			account = *rec.CloudAccountID
		}
		service := rec.Service
		if rec.Provider == "aws" && service == "compute" && rng.Intn(2) == 0 {
			service = "ec2"
		}
		var az, platform, tenancy any
		if v, err := rec.SuppressionVariant(); err == nil && rng.Intn(3) > 0 {
			az, platform, tenancy = v.AvailabilityZone, v.Platform, v.Tenancy
			if rng.Intn(4) == 0 {
				tenancy = "shared"
			}
			if rng.Intn(5) == 0 {
				platform = "other"
			}
		}
		expires := "NOW() + INTERVAL '1 day'"
		if n%13 == 0 {
			expires = "NOW() - INTERVAL '1 day'"
		}
		_, err := pg.DB.Pool().Exec(ctx, `
			INSERT INTO purchase_suppressions (execution_id, account_id, provider, service, region, resource_type, engine,
			                                   suppressed_count, expires_at, availability_zone, platform, tenancy)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, `+expires+`, $9, $10, $11)`,
			uuid.NewString(), account, rec.Provider, service, rec.Region, rec.ResourceType, rec.Engine,
			1+rng.Intn(rec.Count+2), az, platform, tenancy)
		require.NoError(t, err)
	}
}

// seedOverrides adds global service configurations and per-account
// overrides on every dimension the override filter has.
func (f *pageSQLFixture) seedOverrides(ctx context.Context, t *testing.T) {
	t.Helper()
	for _, c := range []config.ServiceConfig{
		{Provider: "aws", Service: "rds", Enabled: true, ExcludeEngines: []string{"mysql"}, MinCount: 3},
		{Provider: "aws", Service: "compute", Enabled: true, IncludeRegions: []string{"us-east-1", "us-west-2"}, ExcludeTypes: []string{"c5.xlarge"}},
		{Provider: "aws", Service: "savings-plans-compute", Enabled: true, MinCount: 50},
		{Provider: "azure", Service: "compute", Enabled: false},
		{Provider: "gcp", Service: "compute", Enabled: true, IncludeEngines: []string{"postgres"}, MinCount: 6},
	} {
		c := c
		require.NoError(t, f.store.SaveServiceConfig(ctx, &c))
	}
	yes, no := true, false
	for _, o := range []config.AccountServiceOverride{
		{AccountID: f.accounts[0].ID, Provider: "aws", Service: "rds", Enabled: &no},
		{AccountID: f.accounts[1].ID, Provider: "aws", Service: "compute", IncludeRegions: []string{"eu-west-1"}},
		{AccountID: f.accounts[2].ID, Provider: "azure", Service: "compute", Enabled: &yes, ExcludeRegions: []string{"us-west-2"}},
		{AccountID: f.accounts[0].ID, Provider: "aws", Service: "ec2", IncludeTypes: []string{"m5.large"}},
	} {
		o := o
		require.NoError(t, f.store.SaveAccountServiceOverride(ctx, &o))
	}
}

// scopes are the account scopes the comparison runs under: unrestricted,
// one registered account, a registered and an unregistered account, an
// account the caller can name only by external ID, and nothing at all.
func (f *pageSQLFixture) scopes() map[string]auth.AccountScope {
	registered := make([]auth.RegisteredAccount, len(f.accounts))
	for i, a := range f.accounts {
		registered[i] = auth.RegisteredAccount{ID: a.ID, Name: a.Name, ExternalID: a.ExternalID}
	}
	scoped := func(ids ...string) auth.AccountScope {
		return auth.ScopeForAccounts(ids).WithRegisteredAccounts(registered)
	}
	return map[string]auth.AccountScope{
		"unrestricted":   auth.UnrestrictedScope(),
		"alpha":          scoped(f.accounts[0].ID),
		"beta+unreg":     scoped(f.accounts[1].ID, f.unreg),
		"external-id":    scoped(f.accounts[2].ExternalID, f.accounts[2].Name),
		"nothing":        auth.ScopeForAccounts(nil),
		"alpha-unscoped": auth.ScopeForAccounts([]string{f.accounts[0].ID}),
	}
}

// filters are the query-string filters the comparison runs with.
func (f *pageSQLFixture) filters() map[string]map[string]string {
	return map[string]map[string]string{
		"none":          {},
		"aws":           {"provider": "aws"},
		"compute":       {"service": "compute"},
		"region":        {"region": "us-east-1"},
		"accounts":      {"account_ids": f.accounts[0].ID + "," + f.accounts[2].ID},
		"usd":           {"min_savings_usd": "20000"},
		"pct":           {"min_savings_pct": "20"},
		"pct-aws-usd":   {"min_savings_pct": "35", "provider": "aws", "min_savings_usd": "100"},
		"pct-high":      {"min_savings_pct": "95"},
		"pct-57":        {"min_savings_pct": "57"},
		"azure-account": {"provider": "azure", "account_ids": f.accounts[2].ID},
	}
}
