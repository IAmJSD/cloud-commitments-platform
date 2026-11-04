package config

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #8: created_at and updated_at on a ServiceConfig are the
// service_configs row's own stamps. A purchase plan's services map is not a
// service_configs row, so its JSONB column must never store them.

// capturedJSONArg matches any []byte argument and keeps it for inspection.
type capturedJSONArg struct{ raw []byte }

func (c *capturedJSONArg) Match(v interface{}) bool {
	b, ok := v.([]byte)
	if ok {
		c.raw = b
	}
	return ok
}

func stampedService() ServiceConfig {
	forged := time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC)
	return ServiceConfig{
		Provider: "aws", Service: "ec2", Enabled: true, Term: 1, Payment: "no-upfront", Coverage: 80,
		CreatedAt: &forged, UpdatedAt: &forged,
	}
}

func assertNoStampKeys(t *testing.T, raw []byte) {
	t.Helper()
	var decoded map[string]map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.NotEmpty(t, decoded)
	for key, svc := range decoded {
		assert.NotContains(t, svc, "created_at", "plan service %s", key)
		assert.NotContains(t, svc, "updated_at", "plan service %s", key)
		assert.Equal(t, "no-upfront", svc["payment"], "the other fields are kept")
	}
}

func TestMarshalPlanServices_DropsStampsWithoutMutatingInput(t *testing.T) {
	services := map[string]ServiceConfig{"aws/ec2": stampedService()}

	raw, err := marshalPlanServices(services)
	require.NoError(t, err)
	assertNoStampKeys(t, raw)
	assert.NotNil(t, services["aws/ec2"].CreatedAt, "the caller's map is not modified")

	nilRaw, err := marshalPlanServices(nil)
	require.NoError(t, err)
	assert.Equal(t, "null", string(nilRaw), "a nil map still encodes as before")
}

func TestPGXMock_CreatePurchasePlan_StoresServicesWithoutStamps(t *testing.T) {
	mock := newMock(t)
	store := storeWith(mock)

	servicesArg := &capturedJSONArg{}
	args := anyArgsCfg(13)
	args[5] = servicesArg
	mock.ExpectQuery("INSERT INTO purchase_plans").WithArgs(args...).
		WillReturnRows(pgxmock.NewRows([]string{"updated_at"}).AddRow(time.Now()))

	plan := &PurchasePlan{ID: "plan-id", Name: "p", Services: map[string]ServiceConfig{"aws/ec2": stampedService()}}
	require.NoError(t, store.CreatePurchasePlan(context.Background(), plan))
	assertNoStampKeys(t, servicesArg.raw)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestPGXMock_UpdatePurchasePlan_StoresServicesWithoutStamps(t *testing.T) {
	mock := newMock(t)
	store := storeWith(mock)

	expectLockedUnmanagedPlan(mock)
	servicesArg := &capturedJSONArg{}
	args := anyArgsCfg(12)
	args[5] = servicesArg
	mock.ExpectQuery("UPDATE purchase_plans").WithArgs(args...).
		WillReturnRows(pgxmock.NewRows([]string{"updated_at"}).AddRow(time.Now()))
	// The services set changed, so the stale ramp baseline is cleared.
	mock.ExpectQuery("SET ramp_baseline = NULL").WithArgs("plan-id").
		WillReturnRows(pgxmock.NewRows([]string{"updated_at"}))
	mock.ExpectCommit()

	plan := &PurchasePlan{ID: "plan-id", Name: "My Plan", Services: map[string]ServiceConfig{"aws/ec2": stampedService()}}
	require.NoError(t, store.UpdatePurchasePlan(context.Background(), plan))
	assertNoStampKeys(t, servicesArg.raw)
	assert.NoError(t, mock.ExpectationsWereMet())
}
