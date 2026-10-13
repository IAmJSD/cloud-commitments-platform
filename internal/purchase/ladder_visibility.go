package purchase

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	sdksp "github.com/aws/aws-sdk-go-v2/service/savingsplans"
	sptypes "github.com/aws/aws-sdk-go-v2/service/savingsplans/types"
)

// ladderVisibleSPStates is the set of Savings Plan states a commitment must be
// in to count as visible. It MUST equal what the pinned AWS ladder passes to
// DescribeSavingsPlans when it builds ExistingUSDPerHour for GetLayerStates
// (providers/aws/ladder/adapters.go spListerStates, unexported, so it cannot be
// imported). TestLadderVisibleStatesMatchGetLayerStates pins the equivalence
// against the real AWSLadder. If this set were wider than the ladder's, a
// tranche would complete while netting still cannot see it and the next run
// would over-buy.
var ladderVisibleSPStates = []sptypes.SavingsPlanState{
	sptypes.SavingsPlanStateActive,
	sptypes.SavingsPlanStatePaymentPending,
}

// ladderDescribeBatch is how many plan IDs go into one DescribeSavingsPlans call.
const ladderDescribeBatch = 100

// SPDescriber is the one Savings Plans API call the visibility prefetch needs.
// *savingsplans.Client satisfies it.
type SPDescriber interface {
	DescribeSavingsPlans(ctx context.Context, in *sdksp.DescribeSavingsPlansInput, optFns ...func(*sdksp.Options)) (*sdksp.DescribeSavingsPlansOutput, error)
}

// LadderSPDescriberFactory returns a describer authenticated for one cloud
// account and region.
type LadderSPDescriberFactory func(ctx context.Context, cloudAccountID, region string) (SPDescriber, error)

// LadderSPDescriber is the production factory: the same per-account credential
// resolution the purchase itself used.
func (m *Manager) LadderSPDescriber(ctx context.Context, cloudAccountID, region string) (SPDescriber, error) {
	account, err := m.config.GetCloudAccount(ctx, cloudAccountID)
	if err != nil {
		return nil, fmt.Errorf("load cloud account %s: %w", cloudAccountID, err)
	}
	if account == nil {
		return nil, fmt.Errorf("cloud account %s not found", cloudAccountID)
	}
	provCfg, err := m.resolveAccountProvider(ctx, *account)
	if err != nil {
		return nil, err
	}
	if provCfg.AWSCredentialsProvider == nil {
		return nil, fmt.Errorf("cloud account %s resolved without AWS credentials", cloudAccountID)
	}
	return sdksp.NewFromConfig(aws.Config{Region: region, Credentials: provCfg.AWSCredentialsProvider}), nil
}

type ladderVisibility struct {
	err     error
	visible bool
}

// prefetchedVisibility is a pure lookup over results fetched before the
// reconciler opens any transaction. Unknown executions fail closed.
type prefetchedVisibility map[string]ladderVisibility

func (p prefetchedVisibility) CommitmentVisible(_ context.Context, _ config.LadderTrancheDB, executionID string) (bool, error) {
	v, ok := p[executionID]
	if !ok {
		return false, fmt.Errorf("execution %s was not prefetched", executionID)
	}
	return v.visible, v.err
}

// expectedPlan is one commitment a completed execution should have created.
type expectedPlan struct {
	id, planType, region string
}

// spPlanTypeForLayer is the SavingsPlanType GetLayerStates sums into a layer.
func spPlanTypeForLayer(l ladder.LayerType) (sptypes.SavingsPlanType, error) {
	switch l {
	case ladder.LayerComputeSP:
		return sptypes.SavingsPlanTypeCompute, nil
	case ladder.LayerEC2InstanceSP:
		return sptypes.SavingsPlanTypeEc2Instance, nil
	}
	return "", fmt.Errorf("layer %q is not a savings plan layer", l)
}

// spCountsAsExisting mirrors the ladder's ExistingUSDPerHour predicate for one
// plan: state in ladderVisibleSPStates, plan type equal to the layer's, and an
// EC2Instance plan only in the ladder's region (Compute plans are global).
func spCountsAsExisting(sp sptypes.SavingsPlan, e expectedPlan) bool {
	return slices.Contains(ladderVisibleSPStates, sp.State) &&
		string(sp.SavingsPlanType) == e.planType &&
		(sp.SavingsPlanType != sptypes.SavingsPlanTypeEc2Instance || aws.ToString(sp.Region) == e.region)
}

type ladderFetchGroup struct {
	account, region string
	execs           map[string][]expectedPlan // execution ID -> its plans
}

// PrefetchLadderVisibility resolves, outside any transaction, whether the
// commitments of each fired tranche's completed execution are visible. It
// makes no provider call when nothing is fired or no execution has completed.
// Any lookup or API error is recorded against the affected executions and
// logged; nothing is ever reported visible on an error.
func PrefetchLadderVisibility(ctx context.Context, store interface {
	GetExecutionByID(ctx context.Context, id string) (*config.PurchaseExecution, error)
}, factory LadderSPDescriberFactory, fired []config.LadderTrancheDB) prefetchedVisibility {
	out := prefetchedVisibility{}
	groups := map[string]*ladderFetchGroup{}
	for i := range fired {
		tr := fired[i]
		if tr.ExecutionID == nil {
			continue
		}
		exec, err := chainTail(ctx, store, *tr.ExecutionID)
		if err == nil && exec != nil && exec.Status == config.StatusCompleted {
			err = addExpected(groups, exec, tr)
		}
		if err != nil {
			logging.Errorf("ladder tranche %s: visibility prefetch failed: %v", tr.ID, err)
			if exec != nil {
				out[exec.ExecutionID] = ladderVisibility{err: err}
			}
		}
	}
	for _, g := range groups {
		fetchGroup(ctx, factory, g, out)
	}
	return out
}

func chainTail(ctx context.Context, store interface {
	GetExecutionByID(ctx context.Context, id string) (*config.PurchaseExecution, error)
}, id string) (*config.PurchaseExecution, error) {
	var exec *config.PurchaseExecution
	for range config.LadderChainMaxDepth {
		var err error
		if exec, err = store.GetExecutionByID(ctx, id); err != nil {
			return nil, err
		}
		if exec == nil {
			return nil, fmt.Errorf("execution %s not found", id)
		}
		if exec.RetryExecutionID == nil {
			return exec, nil
		}
		id = *exec.RetryExecutionID
	}
	return exec, fmt.Errorf("retry chain longer than %d", config.LadderChainMaxDepth)
}

func addExpected(groups map[string]*ladderFetchGroup, exec *config.PurchaseExecution, tr config.LadderTrancheDB) error {
	planType, err := spPlanTypeForLayer(tr.LayerType)
	if err != nil {
		return err
	}
	account, err := SingleCloudAccountIDFromRecs(exec.Recommendations)
	if err != nil {
		return err
	}
	if exec.CloudAccountID != nil {
		account = exec.CloudAccountID
	}
	var plans []expectedPlan
	region := ""
	for i := range exec.Recommendations {
		r := &exec.Recommendations[i]
		if !r.Selected {
			continue
		}
		if r.PurchaseID == "" {
			return fmt.Errorf("completed execution %s has a selected recommendation without a commitment ID", exec.ExecutionID)
		}
		plans = append(plans, expectedPlan{id: r.PurchaseID, planType: string(planType), region: r.Region})
		region = r.Region
	}
	if account == nil || len(plans) == 0 {
		return fmt.Errorf("completed execution %s has no account or commitment to look up", exec.ExecutionID)
	}
	key := *account + "/" + region
	g := groups[key]
	if g == nil {
		g = &ladderFetchGroup{account: *account, region: region, execs: map[string][]expectedPlan{}}
		groups[key] = g
	}
	g.execs[exec.ExecutionID] = plans
	return nil
}

// fetchGroup issues one batched per-ID describe per (account, region) and
// marks an execution visible only when every one of its plans is returned and
// satisfies spCountsAsExisting.
func fetchGroup(ctx context.Context, factory LadderSPDescriberFactory, g *ladderFetchGroup, out prefetchedVisibility) {
	fail := func(err error) {
		logging.Errorf("ladder visibility lookup for account %s region %s failed: %v", g.account, g.region, err)
		for id := range g.execs {
			out[id] = ladderVisibility{err: err}
		}
	}
	var ids []string
	for _, plans := range g.execs {
		for _, p := range plans {
			if !slices.Contains(ids, p.id) {
				ids = append(ids, p.id)
			}
		}
	}
	desc, err := factory(ctx, g.account, g.region)
	if err != nil {
		fail(err)
		return
	}
	found := map[string]sptypes.SavingsPlan{}
	for chunk := range slices.Chunk(ids, ladderDescribeBatch) {
		if err := describeIDs(ctx, desc, chunk, found); err != nil {
			fail(err)
			return
		}
	}
	for id, plans := range g.execs {
		out[id] = ladderVisibility{visible: allCountAsExisting(plans, found)}
	}
}

func allCountAsExisting(plans []expectedPlan, found map[string]sptypes.SavingsPlan) bool {
	for _, p := range plans {
		sp, ok := found[p.id]
		if !ok || !spCountsAsExisting(sp, p) {
			return false
		}
	}
	return true
}

func describeIDs(ctx context.Context, desc SPDescriber, ids []string, found map[string]sptypes.SavingsPlan) error {
	var token *string
	seen := map[string]bool{}
	for {
		res, err := desc.DescribeSavingsPlans(ctx, &sdksp.DescribeSavingsPlansInput{
			SavingsPlanIds: ids, States: ladderVisibleSPStates, NextToken: token,
		})
		if err != nil {
			return fmt.Errorf("DescribeSavingsPlans: %w", err)
		}
		for i := range res.SavingsPlans {
			found[aws.ToString(res.SavingsPlans[i].SavingsPlanId)] = res.SavingsPlans[i]
		}
		tok := aws.ToString(res.NextToken)
		if tok == "" {
			return nil
		}
		if seen[tok] {
			return errors.New("DescribeSavingsPlans returned a repeated pagination token")
		}
		seen[tok] = true
		token = res.NextToken
	}
}

// LadderReconcileDeps is what one scheduled reconcile run needs.
type LadderReconcileDeps struct {
	Store ladderReconcileDB
	SP    LadderSPDescriberFactory
	Now   func() time.Time
}

type ladderReconcileDB interface {
	LadderReconcileStore
	GetExecutionByID(ctx context.Context, id string) (*config.PurchaseExecution, error)
}

// RunLadderReconcile is the entry point for the future scheduled reconcile
// task: prefetch visibility, then ReconcileLadderTranches against the map.
// Nothing schedules it yet. With no fired tranche it touches no provider API.
func RunLadderReconcile(ctx context.Context, d LadderReconcileDeps) (LadderReconcileResult, error) {
	fired, err := d.Store.ListFiredLadderTranches(ctx, ladderReconcileBatch)
	if err != nil {
		return LadderReconcileResult{}, err
	}
	if len(fired) == 0 {
		return LadderReconcileResult{}, nil
	}
	vis := PrefetchLadderVisibility(ctx, d.Store, d.SP, fired)
	return ReconcileLadderTranches(ctx, d.Store, vis, d.Now())
}
