package api

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/commitmentopts"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

const overrideMergeAccount = "11111111-1111-1111-1111-111111111111"

func ovPtr[T any](v T) *T { return &v }

// fullStoredOverride is a stored override with every overlay field set.
func fullStoredOverride() *config.AccountServiceOverride {
	return &config.AccountServiceOverride{
		ID: "ov-1", AccountID: overrideMergeAccount, Provider: "aws", Service: "rds",
		Enabled: ovPtr(true), Term: ovPtr(1), Payment: ovPtr("all-upfront"),
		Coverage: ovPtr(80.0), RampSchedule: ovPtr("weekly-25pct"),
		IncludeEngines: []string{"postgres"}, ExcludeEngines: []string{"mysql"},
		IncludeRegions: []string{"us-east-1"}, ExcludeRegions: []string{"eu-west-1"},
		IncludeTypes: []string{"db.m5.large"}, ExcludeTypes: []string{"db.t3.micro"},
		CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

// putOverride runs the PUT handler against a store holding existing and
// returns the row handed to SaveAccountServiceOverrideAuditedTx (nil if none).
func putOverride(t *testing.T, existing *config.AccountServiceOverride, body string, opts CommitmentOptsInterface) (*config.AccountServiceOverride, error) {
	t.Helper()
	ctx := context.Background()
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	setupAdminAuth(ctx, mockAuth)
	store := new(MockConfigStore)
	store.On("GetAccountServiceOverride", ctx, overrideMergeAccount, "aws", "rds").Return(existing, nil)
	var saved *config.AccountServiceOverride
	store.On("SaveAccountServiceOverrideAuditedTx", ctx, mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { saved = args.Get(2).(*config.AccountServiceOverride) }).Return(nil)
	h := &Handler{auth: mockAuth, config: store}
	if opts != nil {
		h.commitmentOpts = opts
	}
	_, err := h.saveAccountServiceOverride(ctx, adminRequest(body), overrideMergeAccount+"/service-overrides/aws/rds")
	return saved, err
}

// overrideMergeCases maps each overlay key to a body that changes it and the
// edit that produces the expected row from the stored one.
var overrideMergeCases = map[string]struct {
	body  string
	apply func(*config.AccountServiceOverride)
}{
	"enabled":         {`{"enabled":false}`, func(o *config.AccountServiceOverride) { o.Enabled = ovPtr(false) }},
	"term":            {`{"term":3}`, func(o *config.AccountServiceOverride) { o.Term = ovPtr(3) }},
	"payment":         {`{"payment":"no-upfront"}`, func(o *config.AccountServiceOverride) { o.Payment = ovPtr("no-upfront") }},
	"coverage":        {`{"coverage":0}`, func(o *config.AccountServiceOverride) { o.Coverage = ovPtr(0.0) }},
	"ramp_schedule":   {`{"ramp_schedule":"immediate"}`, func(o *config.AccountServiceOverride) { o.RampSchedule = ovPtr("immediate") }},
	"include_engines": {`{"include_engines":["aurora"]}`, func(o *config.AccountServiceOverride) { o.IncludeEngines = []string{"aurora"} }},
	"exclude_engines": {`{"exclude_engines":["oracle"]}`, func(o *config.AccountServiceOverride) { o.ExcludeEngines = []string{"oracle"} }},
	"include_regions": {`{"include_regions":["us-west-2"]}`, func(o *config.AccountServiceOverride) { o.IncludeRegions = []string{"us-west-2"} }},
	"exclude_regions": {`{"exclude_regions":["ap-south-1"]}`, func(o *config.AccountServiceOverride) { o.ExcludeRegions = []string{"ap-south-1"} }},
	"include_types":   {`{"include_types":["db.r5.large"]}`, func(o *config.AccountServiceOverride) { o.IncludeTypes = []string{"db.r5.large"} }},
	"exclude_types":   {`{"exclude_types":["db.t3.small"]}`, func(o *config.AccountServiceOverride) { o.ExcludeTypes = []string{"db.t3.small"} }},
}

// A single-key PUT (what the Settings UI sends) changes that key and keeps
// every other stored field (#1775).
func TestSaveAccountServiceOverride_SingleKeyKeepsOtherFields(t *testing.T) {
	for key, tc := range overrideMergeCases {
		t.Run(key, func(t *testing.T) {
			want := fullStoredOverride()
			tc.apply(want)
			got, err := putOverride(t, fullStoredOverride(), tc.body, nil)
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.WithinDuration(t, time.Now(), got.UpdatedAt, time.Minute)
			got.UpdatedAt = time.Time{}
			assert.Equal(t, want, got)
		})
	}
}

// An explicit JSON null clears only that key back to inherit.
func TestSaveAccountServiceOverride_ExplicitNullClearsOnlyThatKey(t *testing.T) {
	for key := range overrideMergeCases {
		t.Run(key, func(t *testing.T) {
			want := fullStoredOverride()
			clearOverrideKey(want, key)
			got, err := putOverride(t, fullStoredOverride(), `{"`+key+`":null}`, nil)
			require.NoError(t, err)
			got.UpdatedAt = time.Time{}
			assert.Equal(t, want, got)
		})
	}
}

func clearOverrideKey(o *config.AccountServiceOverride, key string) {
	switch key {
	case "enabled":
		o.Enabled = nil
	case "term":
		o.Term = nil
	case "payment":
		o.Payment = nil
	case "coverage":
		o.Coverage = nil
	case "ramp_schedule":
		o.RampSchedule = nil
	case "include_engines":
		o.IncludeEngines = nil
	case "exclude_engines":
		o.ExcludeEngines = nil
	case "include_regions":
		o.IncludeRegions = nil
	case "exclude_regions":
		o.ExcludeRegions = nil
	case "include_types":
		o.IncludeTypes = nil
	case "exclude_types":
		o.ExcludeTypes = nil
	}
}

// Key matching follows encoding/json: a differently-cased key still counts as sent.
func TestSaveAccountServiceOverride_KeyMatchIsCaseInsensitive(t *testing.T) {
	want := fullStoredOverride()
	want.Term = ovPtr(3)
	got, err := putOverride(t, fullStoredOverride(), `{"Term":3}`, nil)
	require.NoError(t, err)
	got.UpdatedAt = time.Time{}
	assert.Equal(t, want, got)
}

// [] sets an empty list, which is not the same as null clearing the key.
func TestSaveAccountServiceOverride_EmptyListSetsEmptyNotNil(t *testing.T) {
	want := fullStoredOverride()
	want.IncludeRegions = []string{}
	got, err := putOverride(t, fullStoredOverride(), `{"include_regions":[]}`, nil)
	require.NoError(t, err)
	got.UpdatedAt = time.Time{}
	assert.Equal(t, want, got)
}

// With no stored row the result carries only the sent fields.
func TestSaveAccountServiceOverride_NoExistingRowKeepsOnlySentFields(t *testing.T) {
	got, err := putOverride(t, nil, `{"term":1,"include_regions":["us-east-1"]}`, nil)
	require.NoError(t, err)
	assert.NotEmpty(t, got.ID)
	assert.Equal(t, ovPtr(1), got.Term)
	assert.Equal(t, []string{"us-east-1"}, got.IncludeRegions)
	assert.Nil(t, got.Enabled)
	assert.Nil(t, got.Payment)
	assert.Nil(t, got.Coverage)
	assert.Nil(t, got.RampSchedule)
	assert.Nil(t, got.IncludeEngines)
	assert.Nil(t, got.ExcludeTypes)
}

// The (term, payment) check sees the merged pair, not just the sent key.
func TestSaveAccountServiceOverride_ComboCheckedOnMergedRow(t *testing.T) {
	existing := &config.AccountServiceOverride{
		ID: "ov-1", AccountID: overrideMergeAccount, Provider: "aws", Service: "rds",
		Payment: ovPtr("no-upfront"),
	}
	// The options offer only 1yr no-upfront: the merged 3yr no-upfront is
	// refused, though the sent term alone (no payment) would pass.
	opts := &stubCommitmentOpts{
		getFn: func(context.Context) (commitmentopts.Options, error) {
			return commitmentopts.Options{"aws": {"rds": {{Provider: "aws", Service: "rds", TermYears: 1, Payment: "no-upfront"}}}}, nil
		},
	}
	got, err := putOverride(t, existing, `{"term":3}`, opts)
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok)
	assert.Equal(t, 400, ce.code)
	assert.Contains(t, ce.message, "3yr no-upfront")
	assert.Nil(t, got, "a refused combo must not be saved")
}

// Every json field of the request is merged: a new field must be added to the
// overlay allowlist (and to overrideMergeCases) or this fails.
func TestAccountServiceOverrideRequest_EveryFieldIsMerged(t *testing.T) {
	var tags []string
	rt := reflect.TypeOf(AccountServiceOverrideRequest{})
	for i := 0; i < rt.NumField(); i++ {
		tags = append(tags, strings.Split(rt.Field(i).Tag.Get("json"), ",")[0])
	}
	caseKeys := make([]string, 0, len(overrideMergeCases))
	for k := range overrideMergeCases {
		caseKeys = append(caseKeys, k)
	}
	assert.ElementsMatch(t, tags, serviceOverrideOverlayKeys)
	assert.ElementsMatch(t, tags, caseKeys)

	// The same set holds on the stored side: no request key is unknown to the row.
	var row map[string]json.RawMessage
	b, err := json.Marshal(fullStoredOverride())
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &row))
	for _, k := range tags {
		assert.Contains(t, row, k)
	}
}

// The store locks on the id it is given, so a mixed-case path id must reach it
// as the stored account's canonical id.
func TestSaveAccountServiceOverride_UsesCanonicalAccountID(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	setupAdminAuth(ctx, mockAuth)
	store := new(MockConfigStore)
	const canonical = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	store.GetCloudAccountFn = func(_ context.Context, _ string) (*config.CloudAccount, error) {
		return &config.CloudAccount{ID: canonical, Provider: "aws"}, nil
	}
	var gotID string
	store.On("UpdateAccountServiceOverrideAtomic", ctx, mock.Anything, "aws", "rds", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { gotID = args.String(1) }).
		Return(&config.AccountServiceOverride{}, nil)
	h := &Handler{auth: mockAuth, config: store}

	_, err := h.saveAccountServiceOverride(ctx, adminRequest(`{"term":1}`), strings.ToUpper(canonical)+"/service-overrides/aws/rds")
	require.NoError(t, err)
	assert.Equal(t, canonical, gotID)
}
