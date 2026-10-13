package purchase

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	sdksp "github.com/aws/aws-sdk-go-v2/service/savingsplans"
	sptypes "github.com/aws/aws-sdk-go-v2/service/savingsplans/types"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// visFake is both the reconcile store and the SP API. inTx lets the API assert
// it is never reached while a reconcile transaction is open.
type visFake struct {
	mu           sync.Mutex
	inTx         bool
	callsInTx    int
	calls        int
	factoryHits  int
	factoryAccts []string
	apiErr       error
	plans        []sptypes.SavingsPlan
	fired        []config.LadderTrancheDB
	execs        map[string]*config.PurchaseExecution
	completed    []string
	lastStates   []sptypes.SavingsPlanState
}

func (f *visFake) ListFiredLadderTranches(context.Context, int) ([]config.LadderTrancheDB, error) {
	return f.fired, nil
}
func (f *visFake) WithTx(_ context.Context, fn func(pgx.Tx) error) error {
	f.inTx = true
	defer func() { f.inTx = false }()
	return fn(nil)
}
func (f *visFake) LockLadderTrancheChainTx(_ context.Context, _ pgx.Tx, id string) ([]config.LadderChainNode, error) {
	for _, tr := range f.fired {
		if tr.ID == id {
			return []config.LadderChainNode{{ExecutionID: *tr.ExecutionID, Status: f.execs[*tr.ExecutionID].Status, CreatedAt: time.Unix(0, 0)}}, nil
		}
	}
	return nil, config.ErrLadderTrancheNotFired
}
func (f *visFake) TransitionLadderTrancheTx(_ context.Context, _ pgx.Tx, id string, _, to ladder.TrancheStatus, _ string) (bool, error) {
	if to == ladder.TrancheStatusCompleted {
		f.completed = append(f.completed, id)
	}
	return true, nil
}
func (f *visFake) GetExecutionByID(_ context.Context, id string) (*config.PurchaseExecution, error) {
	return f.execs[id], nil
}
func (f *visFake) DescribeSavingsPlans(_ context.Context, in *sdksp.DescribeSavingsPlansInput, _ ...func(*sdksp.Options)) (*sdksp.DescribeSavingsPlansOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastStates = in.States
	if f.inTx {
		f.callsInTx++
	}
	if f.apiErr != nil {
		return nil, f.apiErr
	}
	return &sdksp.DescribeSavingsPlansOutput{SavingsPlans: f.plans}, nil
}
func (f *visFake) factory(_ context.Context, acct, _ string) (SPDescriber, error) {
	f.factoryHits++
	f.factoryAccts = append(f.factoryAccts, acct)
	return f, nil
}

func (f *visFake) addTranche(id, acct, spID string, layer ladder.LayerType) {
	execID := "exec-" + id
	f.fired = append(f.fired, config.LadderTrancheDB{ID: id, ExecutionID: &execID, LayerType: layer, Status: ladder.TrancheStatusFired})
	if f.execs == nil {
		f.execs = map[string]*config.PurchaseExecution{}
	}
	f.execs[execID] = &config.PurchaseExecution{
		ExecutionID: execID, Status: config.StatusCompleted, CloudAccountID: aws.String(acct),
		Recommendations: []config.RecommendationRecord{{Selected: true, PurchaseID: spID, Region: "us-east-1"}},
	}
}

func sp(id string, typ sptypes.SavingsPlanType, state sptypes.SavingsPlanState, region string) sptypes.SavingsPlan {
	p := sptypes.SavingsPlan{SavingsPlanId: aws.String(id), SavingsPlanType: typ, State: state}
	if region != "" {
		p.Region = aws.String(region)
	}
	return p
}

func (f *visFake) run(t *testing.T) LadderReconcileResult {
	t.Helper()
	res, err := RunLadderReconcile(context.Background(), LadderReconcileDeps{Store: f, SP: f.factory, Now: time.Now})
	require.NoError(t, err)
	return res
}

func TestRunLadderReconcile_NoFiredMakesNoProviderCall(t *testing.T) {
	f := &visFake{}
	res := f.run(t)
	assert.Zero(t, res.Checked)
	assert.Zero(t, f.calls)
	assert.Zero(t, f.factoryHits)
}

func TestRunLadderReconcile_VisibleCompletesWithProviderCallsOutsideTx(t *testing.T) {
	f := &visFake{}
	f.addTranche("t1", "acct-a", "sp-1", ladder.LayerComputeSP)
	f.plans = []sptypes.SavingsPlan{sp("sp-1", sptypes.SavingsPlanTypeCompute, sptypes.SavingsPlanStateActive, "")}
	res := f.run(t)
	assert.Equal(t, []string{"t1"}, f.completed)
	assert.Equal(t, 1, res.Completed)
	assert.Equal(t, 1, f.calls)
	assert.Zero(t, f.callsInTx, "provider must not be called inside the locked transaction")
}

func TestRunLadderReconcile_StaysFiredWhenNotVisible(t *testing.T) {
	cases := map[string]sptypes.SavingsPlan{
		"queued":          sp("sp-1", sptypes.SavingsPlanTypeCompute, sptypes.SavingsPlanStateQueued, ""),
		"pending-return":  sp("sp-1", sptypes.SavingsPlanTypeCompute, sptypes.SavingsPlanStatePendingReturn, ""),
		"wrong plan type": sp("sp-1", sptypes.SavingsPlanTypeEc2Instance, sptypes.SavingsPlanStateActive, "us-east-1"),
		"absent":          sp("other", sptypes.SavingsPlanTypeCompute, sptypes.SavingsPlanStateActive, ""),
	}
	for name, plan := range cases {
		t.Run(name, func(t *testing.T) {
			f := &visFake{plans: []sptypes.SavingsPlan{plan}}
			f.addTranche("t1", "acct-a", "sp-1", ladder.LayerComputeSP)
			res := f.run(t)
			assert.Empty(t, f.completed)
			assert.Equal(t, 1, res.Unresolved)
		})
	}
	t.Run("ec2 instance sp in another region", func(t *testing.T) {
		f := &visFake{plans: []sptypes.SavingsPlan{sp("sp-1", sptypes.SavingsPlanTypeEc2Instance, sptypes.SavingsPlanStateActive, "eu-west-1")}}
		f.addTranche("t1", "acct-a", "sp-1", ladder.LayerEC2InstanceSP)
		f.run(t)
		assert.Empty(t, f.completed)
	})
}

func TestRunLadderReconcile_APIErrorFailsClosed(t *testing.T) {
	f := &visFake{apiErr: errors.New("throttled")}
	f.addTranche("t1", "acct-a", "sp-1", ladder.LayerComputeSP)
	res := f.run(t)
	assert.Empty(t, f.completed)
	assert.Equal(t, 1, res.Unresolved)
}

func TestRunLadderReconcile_BatchesPerAccountAndRegion(t *testing.T) {
	f := &visFake{plans: []sptypes.SavingsPlan{
		sp("sp-1", sptypes.SavingsPlanTypeCompute, sptypes.SavingsPlanStateActive, ""),
		sp("sp-2", sptypes.SavingsPlanTypeCompute, sptypes.SavingsPlanStatePaymentPending, ""),
		sp("sp-3", sptypes.SavingsPlanTypeCompute, sptypes.SavingsPlanStateActive, ""),
	}}
	f.addTranche("t1", "acct-a", "sp-1", ladder.LayerComputeSP)
	f.addTranche("t2", "acct-a", "sp-2", ladder.LayerComputeSP)
	f.addTranche("t3", "acct-b", "sp-3", ladder.LayerComputeSP)
	f.run(t)
	assert.Equal(t, 2, f.calls, "one call per account, not per tranche")
	assert.ElementsMatch(t, []string{"t1", "t2", "t3"}, f.completed)
}

func TestRunLadderReconcile_StaleUnresolvedIsCounted(t *testing.T) {
	f := &visFake{apiErr: errors.New("boom")}
	f.addTranche("t1", "acct-a", "sp-1", ladder.LayerComputeSP)
	res := f.run(t)
	assert.Equal(t, 1, res.Stale, "chain head is epoch, far older than LadderFiredStaleAfter")
}

// TestLadderVisiblePredicateMatchesUpstream pins the predicate to the pinned
// module's, without any network access. The module exports no symbol for it,
// so the table is copied from providers/aws/ladder at the go.mod pin:
//   - states: adapters.go:39-42 spListerStates (active, payment-pending only;
//     queued is excluded on purpose, adapters.go:30-37)
//   - EC2Instance plans only in the ladder region, Compute global:
//     adapters.go:123-133 appendRegionScopedSPs
//   - plans are summed into the layer of their own plan type:
//     layer_states.go:244-251 sumSPHourlyCost
//
// TODO: export the set from cloud-commitments-go and import it here.
func TestLadderVisiblePredicateMatchesUpstream(t *testing.T) {
	const region = "us-east-1"
	cases := []struct {
		name  string
		state sptypes.SavingsPlanState
		typ   sptypes.SavingsPlanType
		plan  sptypes.SavingsPlanType
		reg   string
		want  bool
	}{
		{"compute active", sptypes.SavingsPlanStateActive, sptypes.SavingsPlanTypeCompute, sptypes.SavingsPlanTypeCompute, "", true},
		{"compute payment-pending", sptypes.SavingsPlanStatePaymentPending, sptypes.SavingsPlanTypeCompute, sptypes.SavingsPlanTypeCompute, "", true},
		{"compute queued", sptypes.SavingsPlanStateQueued, sptypes.SavingsPlanTypeCompute, sptypes.SavingsPlanTypeCompute, "", false},
		{"compute pending-return", sptypes.SavingsPlanStatePendingReturn, sptypes.SavingsPlanTypeCompute, sptypes.SavingsPlanTypeCompute, "", false},
		{"compute retired", sptypes.SavingsPlanStateRetired, sptypes.SavingsPlanTypeCompute, sptypes.SavingsPlanTypeCompute, "", false},
		{"ec2 same region", sptypes.SavingsPlanStateActive, sptypes.SavingsPlanTypeEc2Instance, sptypes.SavingsPlanTypeEc2Instance, region, true},
		{"ec2 other region", sptypes.SavingsPlanStateActive, sptypes.SavingsPlanTypeEc2Instance, sptypes.SavingsPlanTypeEc2Instance, "eu-west-1", false},
		{"type differs from layer", sptypes.SavingsPlanStateActive, sptypes.SavingsPlanTypeCompute, sptypes.SavingsPlanTypeEc2Instance, region, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := spCountsAsExisting(sp("x", c.typ, c.state, c.reg), expectedPlan{id: "x", planType: string(c.plan), region: region})
			assert.Equal(t, c.want, got)
		})
	}
	assert.ElementsMatch(t, []sptypes.SavingsPlanState{sptypes.SavingsPlanStateActive, sptypes.SavingsPlanStatePaymentPending}, ladderVisibleSPStates)
}

func TestRunLadderReconcile_PartialVisibilityStaysFired(t *testing.T) {
	f := &visFake{plans: []sptypes.SavingsPlan{sp("sp-1", sptypes.SavingsPlanTypeCompute, sptypes.SavingsPlanStateActive, "")}}
	f.addTranche("t1", "acct-a", "sp-1", ladder.LayerComputeSP)
	f.execs["exec-t1"].Recommendations = append(f.execs["exec-t1"].Recommendations,
		config.RecommendationRecord{Selected: true, PurchaseID: "sp-2", Region: "us-east-1"})
	f.run(t)
	assert.Empty(t, f.completed)
	assert.Equal(t, []string{"acct-a"}, f.factoryAccts)
}
