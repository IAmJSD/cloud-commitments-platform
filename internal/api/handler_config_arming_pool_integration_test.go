//go:build integration

package api

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/require"
)

// armingSavePoolSize is the Fargate default pool (db_max_connections = 3).
const armingSavePoolSize = 3

// TestGlobalConfigArmingSavesDoNotExhaustPool is the pool deadlock of issue
// #1517. UpdateGlobalConfigAtomic holds one pooled connection for its locked
// transaction. When a save arms unattended spend, the grant check runs inside
// that transaction; if it reads the auth store through the same pool, then as
// many concurrent saves as the pool has connections each hold one (the lock
// holder, plus the rest waiting on the advisory lock) and the holder waits for
// a second that never frees. Every save then hangs until its deadline.
//
// The auth service, its store and the config store all share one pool of the
// Fargate default size, as internal/server/app.go wires them. Arming and
// disarming saves alternate so that many lock holders need the grant, not just
// the first. Before the fix each subtest fails at the deadline; after it the
// whole batch takes well under a second.
func TestGlobalConfigArmingSavesDoNotExhaustPool(t *testing.T) {
	for _, tc := range []struct {
		name        string
		arm, disarm string
		put         func(h *Handler) func(context.Context, *events.LambdaFunctionURLRequest) error
		armed       func(*config.GlobalConfig) bool
	}{
		{
			name:   "config PUT arms auto-purchase",
			arm:    `{"auto_purchase_enabled":true}`,
			disarm: `{"auto_purchase_enabled":false}`,
			put:    putGlobalConfig,
			armed:  func(c *config.GlobalConfig) bool { return c.AutoPurchaseEnabled },
		},
		{
			name:   "config PUT arms ladder execution",
			arm:    `{"laddering_enabled":true,"ladder_execution_enabled":true}`,
			disarm: `{"ladder_execution_enabled":false}`,
			put:    putGlobalConfig,
			armed:  func(c *config.GlobalConfig) bool { return c.LadderExecutionEnabled },
		},
		{
			name:   "config PUT arms RI exchange auto mode",
			arm:    `{"ri_exchange_enabled":true,"ri_exchange_mode":"auto"}`,
			disarm: `{"ri_exchange_mode":"manual"}`,
			put:    putGlobalConfig,
			armed:  func(c *config.GlobalConfig) bool { return riExchangeArmStateOf(c).armed() },
		},
		{
			name:   "RI exchange config PUT arms auto mode",
			arm:    `{"auto_exchange_enabled":true,"mode":"auto","utilization_threshold":50,"max_payment_per_exchange_usd":100,"max_payment_daily_usd":500,"lookback_days":30}`,
			disarm: `{"auto_exchange_enabled":true,"mode":"manual","utilization_threshold":50,"max_payment_per_exchange_usd":100,"max_payment_daily_usd":500,"lookback_days":30}`,
			put:    putRIExchangeConfig,
			armed:  func(c *config.GlobalConfig) bool { return riExchangeArmStateOf(c).armed() },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, store, token := newSmallPoolArmingHandler(t)
			put := tc.put(h)
			ctx := t.Context()

			// Prove the fixture actually reaches the grant: the arming save
			// is authorized through the real auth store, and the session
			// holds the execute verbs.
			require.NoError(t, put(ctx, bearerRequest(token, tc.arm)))
			require.NoError(t, put(ctx, bearerRequest(token, tc.disarm)))

			const saves = armingSavePoolSize * 4
			reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			start := make(chan struct{})
			errs := make([]error, saves)
			var wg sync.WaitGroup
			for i := range saves {
				body := tc.arm
				if i%2 == 1 {
					body = tc.disarm
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					errs[i] = put(reqCtx, bearerRequest(token, body))
				}()
			}
			began := time.Now()
			close(start)
			wg.Wait()
			t.Logf("%d concurrent arming/disarming saves on a pool of %d took %v", saves, armingSavePoolSize, time.Since(began))

			for i, err := range errs {
				require.NoError(t, err, "save %d", i)
			}
			// A final arming save lands, so the batch did not leave the
			// switch in a state the gate never saw.
			require.NoError(t, put(ctx, bearerRequest(token, tc.arm)))
			got, err := store.GetGlobalConfig(ctx)
			require.NoError(t, err)
			require.True(t, tc.armed(got))
		})
	}
}

// TestGlobalConfigArmingSaveGrantStaysStrict checks, on the real auth store,
// that a session holding update:config but neither execute verb is still
// refused every arming save while an ordinary save goes through.
func TestGlobalConfigArmingSaveGrantStaysStrict(t *testing.T) {
	ctx := t.Context()
	pg := testhelpers.RequireMigratedDatabase(ctx, t, getMigrationsPath())
	store, authStore := config.NewPostgresStore(pg.DB), auth.NewPostgresStore(pg.DB)
	csrfKey := []byte(strings.Repeat("g", 32))
	h := NewHandler(HandlerConfig{ConfigStore: store, AuthService: &marketplaceAuthFixture{service: auth.NewService(auth.ServiceConfig{Store: authStore, CSRFKey: csrfKey})}})
	_, token, _ := marketplaceSession(t, authStore, []auth.Permission{
		{Action: auth.ActionView, Resource: auth.ResourceConfig},
		{Action: auth.ActionUpdate, Resource: auth.ResourceConfig},
	}, []string{"*"}, csrfKey)

	require.NoError(t, h.updateConfigErr(ctx, bearerRequest(token, `{"laddering_enabled":true}`)))
	for _, tc := range []struct {
		put  func(context.Context, *events.LambdaFunctionURLRequest) error
		body string
	}{
		{h.updateConfigErr, `{"auto_purchase_enabled":true}`},
		{h.updateConfigErr, `{"ladder_execution_enabled":true}`},
		{h.updateConfigErr, `{"ri_exchange_enabled":true,"ri_exchange_mode":"auto"}`},
		{h.updateRIExchangeConfigErr, `{"auto_exchange_enabled":true,"mode":"auto","utilization_threshold":50,"lookback_days":30}`},
	} {
		err := tc.put(ctx, bearerRequest(token, tc.body))
		ce, ok := IsClientError(err)
		require.True(t, ok, "%s: want a 403, got %v", tc.body, err)
		require.Equal(t, 403, ce.code, tc.body)
	}
	got, err := store.GetGlobalConfig(ctx)
	require.NoError(t, err)
	require.True(t, got.LadderingEnabled)
	require.False(t, got.AutoPurchaseEnabled)
	require.False(t, got.LadderExecutionEnabled)
	require.False(t, riExchangeArmStateOf(got).armed())
}

// newSmallPoolArmingHandler wires a handler whose config store and real auth
// service share one pool of armingSavePoolSize connections, and returns a
// session token whose user may arm every money-spending switch.
func newSmallPoolArmingHandler(t *testing.T) (*Handler, *config.PostgresStore, string) {
	t.Helper()
	ctx := t.Context()
	// The limit under test is this client pool's size, so the shared server
	// is fine.
	pg := testhelpers.RequireMigratedDatabase(ctx, t, getMigrationsPath())
	poolCfg := *pg.Config
	poolCfg.MaxConnections, poolCfg.MinConnections = armingSavePoolSize, 0
	db, err := database.NewConnection(ctx, &poolCfg, nil)
	require.NoError(t, err)
	t.Cleanup(db.Close)

	store, authStore := config.NewPostgresStore(db), auth.NewPostgresStore(db)
	csrfKey := []byte(strings.Repeat("p", 32))
	h := NewHandler(HandlerConfig{ConfigStore: store, AuthService: &marketplaceAuthFixture{service: auth.NewService(auth.ServiceConfig{Store: authStore, CSRFKey: csrfKey})}})
	_, token, _ := marketplaceSession(t, authStore, []auth.Permission{
		{Action: auth.ActionView, Resource: auth.ResourceConfig},
		{Action: auth.ActionUpdate, Resource: auth.ResourceConfig},
		{Action: auth.ActionExecute, Resource: auth.ResourcePurchases},
		{Action: auth.ActionExecute, Resource: auth.ResourceRIExchange},
	}, []string{"*"}, csrfKey)
	return h, store, token
}

func putGlobalConfig(h *Handler) func(context.Context, *events.LambdaFunctionURLRequest) error {
	return h.updateConfigErr
}

func putRIExchangeConfig(h *Handler) func(context.Context, *events.LambdaFunctionURLRequest) error {
	return h.updateRIExchangeConfigErr
}

func (h *Handler) updateConfigErr(ctx context.Context, req *events.LambdaFunctionURLRequest) error {
	_, err := h.updateConfig(ctx, req)
	return err
}

func (h *Handler) updateRIExchangeConfigErr(ctx context.Context, req *events.LambdaFunctionURLRequest) error {
	_, err := h.updateRIExchangeConfig(ctx, req)
	return err
}

func bearerRequest(token, body string) *events.LambdaFunctionURLRequest {
	return &events.LambdaFunctionURLRequest{Headers: map[string]string{"Authorization": "Bearer " + token}, Body: body}
}
