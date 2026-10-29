package api

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// Issue #1034: group writes bind allowed_accounts to account IDs at grant
// time. AccountScope matches entries by ID only, so a name the client sends
// is resolved here to the one account carrying it, and the ID is stored. A
// later rename or reuse of the name then cannot move the grant.

const bindingGroupID = "11111111-1111-1111-1111-111111111111"

var (
	bindProd  = config.CloudAccount{ID: "b1b1b1b1-0000-4000-8000-000000000001", Name: "prod", Provider: "aws", ExternalID: "123456789012"}
	bindDupA  = config.CloudAccount{ID: "b2b2b2b2-0000-4000-8000-000000000002", Name: "shared", Provider: "aws", ExternalID: "210987654321"}
	bindDupB  = config.CloudAccount{ID: "b3b3b3b3-0000-4000-8000-000000000003", Name: "shared", Provider: "azure", ExternalID: "sub-b"}
	bindOther = config.CloudAccount{ID: "b4b4b4b4-0000-4000-8000-000000000004", Name: "other", Provider: "gcp", ExternalID: "proj-other"}
	bindDupC  = config.CloudAccount{ID: "b5b5b5b5-0000-4000-8000-000000000005", Name: "shared", Provider: "gcp", ExternalID: "proj-c"}
)

// bindingHandler wires an admin session (restricted to scope when scope is
// non-nil) against a store holding accounts. listErr makes ListCloudAccounts
// fail. listCalls counts ListCloudAccounts calls.
func bindingHandler(t *testing.T, scope []string, accounts []config.CloudAccount, listErr error) (*Handler, *MockAuthService, *int) {
	t.Helper()
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	mockAuth.On("ValidateSession", mock.Anything, "admin-token").
		Return(&Session{UserID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"}, nil)
	if scope == nil {
		mockAuth.grantAdmin()
	} else {
		mockAuth.grantScoped(scope...)
	}
	listCalls := 0
	store := new(MockConfigStore)
	store.ListCloudAccountsFn = func(context.Context, config.CloudAccountFilter) ([]config.CloudAccount, error) {
		listCalls++
		if listErr != nil {
			return nil, listErr
		}
		return append([]config.CloudAccount(nil), accounts...), nil
	}
	return &Handler{auth: mockAuth, config: store}, mockAuth, &listCalls
}

func bindingRequest(body string) *events.LambdaFunctionURLRequest {
	return &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"Authorization": "Bearer admin-token"},
		Body:    body,
	}
}

// expectCreate records the allowed_accounts CreateGroupAPI receives.
func expectCreate(m *MockAuthService, got *[]string) {
	m.On("CreateGroupAPI", mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			*got = args.Get(2).(auth.APICreateGroupRequest).AllowedAccounts
		}).
		Return(&auth.APIGroup{ID: bindingGroupID}, nil).Once()
}

// expectUpdate records the allowed_accounts UpdateGroupAPI receives.
func expectUpdate(m *MockAuthService, got *[]string) {
	m.On("UpdateGroupAPI", mock.Anything, mock.Anything, bindingGroupID, mock.Anything).
		Run(func(args mock.Arguments) {
			*got = args.Get(3).(auth.APIUpdateGroupRequest).AllowedAccounts
		}).
		Return(&auth.APIGroup{ID: bindingGroupID}, nil).Once()
}

func TestCreateGroup_BindsAccountNamesToIDs(t *testing.T) {
	t.Parallel()
	all := []config.CloudAccount{bindProd, bindDupA, bindDupB, bindOther}
	for _, tc := range []struct {
		name string
		body string
		want []string
	}{
		{"a unique name is stored as its account's ID", `{"name":"g","allowed_accounts":["prod"]}`, []string{bindProd.ID}},
		{"an ID is kept", `{"name":"g","allowed_accounts":["` + bindDupA.ID + `"]}`, []string{bindDupA.ID}},
		{"an external ID is kept verbatim", `{"name":"g","allowed_accounts":["123456789012"]}`, []string{"123456789012"}},
		{"order and mixed forms are preserved", `{"name":"g","allowed_accounts":["` + bindOther.ID + `","prod"]}`, []string{bindOther.ID, bindProd.ID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, mockAuth, _ := bindingHandler(t, nil, all, nil)
			var got []string
			expectCreate(mockAuth, &got)

			_, err := h.createGroup(context.Background(), bindingRequest(tc.body))

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			mockAuth.AssertExpectations(t)
		})
	}
}

// An entry that does not identify exactly one account is refused, not stored
// as a name: stored, it would grant whichever account takes the name later.
func TestCreateGroup_RefusesUnbindableAccountEntries(t *testing.T) {
	t.Parallel()
	// Three accounts share "shared", so a third copy cannot make it unique again.
	all := []config.CloudAccount{bindProd, bindDupA, bindDupB, bindOther, bindDupC}
	for _, tc := range []struct {
		name  string
		scope []string
		entry string
	}{
		{"unknown name", nil, "deleted-account"},
		{"name shared by two accounts", nil, "shared"},
		{"empty entry", nil, ""},
		{"name of an account outside the actor's scope", []string{bindProd.ID}, "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, mockAuth, _ := bindingHandler(t, tc.scope, all, nil)

			_, err := h.createGroup(context.Background(),
				bindingRequest(`{"name":"g","allowed_accounts":["`+bindProd.ID+`","`+tc.entry+`"]}`))

			require.Error(t, err)
			ce, ok := IsClientError(err)
			require.True(t, ok, "want a 400 client error, got %T: %v", err, err)
			assert.Equal(t, 400, ce.code)
			assert.Contains(t, err.Error(), "does not identify exactly one cloud account you can access")
			mockAuth.AssertNumberOfCalls(t, "CreateGroupAPI", 0)
		})
	}
}

// A scoped actor can bind a name only to an account inside their own scope.
func TestCreateGroup_ScopedActorBindsInScopeName(t *testing.T) {
	t.Parallel()
	h, mockAuth, _ := bindingHandler(t, []string{bindProd.ID}, []config.CloudAccount{bindProd, bindOther}, nil)
	var got []string
	expectCreate(mockAuth, &got)

	_, err := h.createGroup(context.Background(), bindingRequest(`{"name":"g","allowed_accounts":["prod"]}`))

	require.NoError(t, err)
	assert.Equal(t, []string{bindProd.ID}, got)
}

// The account list is needed to bind; without it the write fails closed.
func TestCreateGroup_AccountListFailureRefusesWrite(t *testing.T) {
	t.Parallel()
	h, mockAuth, _ := bindingHandler(t, nil, nil, errors.New("db down"))

	_, err := h.createGroup(context.Background(), bindingRequest(`{"name":"g","allowed_accounts":["prod"]}`))

	require.Error(t, err)
	_, isClient := IsClientError(err)
	assert.False(t, isClient, "a store failure is a server error")
	mockAuth.AssertNumberOfCalls(t, "CreateGroupAPI", 0)
}

func TestCreateGroup_NoAccountStoreRefusesWrite(t *testing.T) {
	t.Parallel()
	h, mockAuth, _ := bindingHandler(t, nil, nil, nil)
	h.config = nil

	_, err := h.createGroup(context.Background(), bindingRequest(`{"name":"g","allowed_accounts":["prod"]}`))

	require.Error(t, err)
	mockAuth.AssertNumberOfCalls(t, "CreateGroupAPI", 0)
}

// Nothing to bind: omitted, empty and wildcard-only lists pass through
// untouched (the auth service's ceiling judges them) without loading accounts.
func TestCreateGroup_NothingToBindPassesThrough(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		body string
		want []string
	}{
		{"omitted", `{"name":"g"}`, nil},
		{"empty", `{"name":"g","allowed_accounts":[]}`, []string{}},
		{"wildcard", `{"name":"g","allowed_accounts":["*"]}`, []string{"*"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, mockAuth, listCalls := bindingHandler(t, nil, nil, errors.New("must not be called"))
			var got []string
			expectCreate(mockAuth, &got)

			_, err := h.createGroup(context.Background(), bindingRequest(tc.body))

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Zero(t, *listCalls)
		})
	}
}

func TestUpdateGroup_BindsAccountNamesToIDs(t *testing.T) {
	t.Parallel()
	h, mockAuth, _ := bindingHandler(t, nil, []config.CloudAccount{bindProd, bindOther}, nil)
	var got []string
	expectUpdate(mockAuth, &got)

	_, err := h.updateGroup(context.Background(), bindingRequest(`{"allowed_accounts":["prod","`+bindOther.ID+`"]}`), bindingGroupID)

	require.NoError(t, err)
	assert.Equal(t, []string{bindProd.ID, bindOther.ID}, got)
}

func TestUpdateGroup_RefusesUnbindableAccountEntry(t *testing.T) {
	t.Parallel()
	h, mockAuth, _ := bindingHandler(t, nil, []config.CloudAccount{bindDupA, bindDupB}, nil)

	_, err := h.updateGroup(context.Background(), bindingRequest(`{"allowed_accounts":["shared"]}`), bindingGroupID)

	ce, ok := IsClientError(err)
	require.True(t, ok, "want a 400 client error, got %T: %v", err, err)
	assert.Equal(t, 400, ce.code)
	mockAuth.AssertNumberOfCalls(t, "UpdateGroupAPI", 0)
}

// "not sent" stays nil, so the update leaves the group's scope alone.
func TestUpdateGroup_OmittedAccountsStayNil(t *testing.T) {
	t.Parallel()
	h, mockAuth, listCalls := bindingHandler(t, nil, nil, errors.New("must not be called"))
	got := []string{"sentinel"}
	expectUpdate(mockAuth, &got)

	_, err := h.updateGroup(context.Background(), bindingRequest(`{"description":"d"}`), bindingGroupID)

	require.NoError(t, err)
	assert.Nil(t, got)
	assert.Zero(t, *listCalls)
}

// End to end for the issue: a group created by naming "prod" keeps reaching
// that account after it is renamed, and does not reach the account that
// later takes the name "prod".
func TestGroupGrantByName_SurvivesRenameAndNameReuse(t *testing.T) {
	t.Parallel()
	h, mockAuth, _ := bindingHandler(t, nil, []config.CloudAccount{bindProd, bindOther}, nil)
	var stored []string
	expectCreate(mockAuth, &stored)
	_, err := h.createGroup(context.Background(), bindingRequest(`{"name":"g","allowed_accounts":["prod"]}`))
	require.NoError(t, err)

	renamed := bindProd
	renamed.Name = "prod-legacy"
	newcomer := bindOther
	newcomer.Name = "prod"
	scoped := sharedNameHandler(t, stored, []config.CloudAccount{renamed, newcomer}, nil)
	scopedAuth := scoped.auth.(*MockAuthService)
	t.Cleanup(func() { scopedAuth.AssertExpectations(t) })

	assert.Equal(t, 200, sharedNameGET(t, scoped, "/api/accounts/"+renamed.ID).StatusCode)
	assert.Equal(t, 404, sharedNameGET(t, scoped, "/api/accounts/"+newcomer.ID).StatusCode,
		"the account that took the name must not inherit the grant")
}
