package auth

// allActions and allResources enumerate the predefined Action* and
// Resource* constants (issue #166). Go cannot enumerate a const block, so
// these arrays are the enumerable form of it. They are not trusted to stay
// complete by hand: TestPermissionCatalogMatchesConstDecls parses this
// package's source and fails when an Action* or Resource* constant is
// declared without being listed here, or listed here without being
// declared. Add new verbs or resources to the const blocks in types.go AND
// here; the test names the one you missed.
var allActions = [...]string{
	ActionView,
	ActionCreate,
	ActionUpdate,
	ActionDelete,
	ActionExecute,
	ActionApprove,
	ActionAdmin,
	ActionCancelOwn,
	ActionCancelAny,
	ActionRetryOwn,
	ActionRetryAny,
	ActionApproveOwn,
	ActionApproveAny,
	ActionExecuteOwn,
	ActionExecuteAny,
	ActionUpdateAny,
	ActionRevokeOwn,
	ActionRevokeAny,
	ActionSellOwn,
	ActionSellAny,
}

var allResources = [...]string{
	ResourceRecommendations,
	ResourcePlans,
	ResourcePurchases,
	ResourceHistory,
	ResourceConfig,
	ResourceAccounts,
	ResourceUsers,
	ResourceGroups,
	ResourceAPIKeys,
	ResourceRIExchange,
	ResourceAll,
}

// AllActions returns every predefined permission action, in declaration
// order. It includes the admin verb. The caller owns the returned slice.
func AllActions() []string {
	return append([]string(nil), allActions[:]...)
}

// AllResources returns every predefined permission resource, in
// declaration order. It includes the ResourceAll wildcard ("*"), which a
// stored permission may name. The caller owns the returned slice.
func AllResources() []string {
	return append([]string(nil), allResources[:]...)
}
