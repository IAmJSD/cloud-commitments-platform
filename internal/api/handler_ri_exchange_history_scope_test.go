package api

import (
	"context"
	"errors"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// exchangeHistoryScopeCase is one scoped GET /api/ri-exchange/history call:
// the allow-list, the exact STS id set the handler must push into the store
// query, and the record ids the response must carry.
type exchangeHistoryScopeCase struct {
	name    string
	scope   []string
	wantIDs []string
	want    []string
}

// runExchangeHistoryScopeCases calls the history handler once per case with
// the given registered accounts. It checks the account-id set pushed into the
// store query (the SQL predicate) against the case. The store then returns
// every stored row whatever the ids, so the handler's own per-row scope
// re-check decides what the response carries.
func runExchangeHistoryScopeCases(t *testing.T, accounts []config.CloudAccount, stored []config.RIExchangeRecord, cases []exchangeHistoryScopeCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			mockAuth := new(MockAuthService)
			mockAuth.On("ValidateSession", ctx, "tok").Return(&Session{UserID: "uid"}, nil)
			mockAuth.grantScoped(tc.scope...)
			store := new(MockConfigStore)
			store.ListCloudAccountsFn = func(_ context.Context, _ config.CloudAccountFilter) ([]config.CloudAccount, error) {
				return accounts, nil
			}
			var gotIDs []string
			store.On("GetRIExchangeHistoryForAccounts", ctx, mock.Anything, 500, mock.Anything).
				Run(func(args mock.Arguments) { gotIDs = args.Get(3).([]string) }).
				Return(stored, nil).Once()
			h := &Handler{auth: mockAuth, config: store}
			req := &events.LambdaFunctionURLRequest{Headers: map[string]string{"Authorization": "Bearer tok"}}

			result, err := h.getRIExchangeHistory(ctx, req)
			require.NoError(t, err)
			assert.Equal(t, tc.wantIDs, gotIDs, "account ids pushed into the store query")
			records := result.(*RIExchangeHistoryResponse).Records
			got := make([]string, 0, len(records))
			for _, r := range records {
				got = append(got, r.ID)
			}
			assert.Equal(t, tc.want, got)
			store.AssertExpectations(t)
		})
	}
}

// Issue #558: an exchange record whose STS account id no registered AWS
// account carries is covered only by that literal id in the allow-list. A
// scope naming a registered account must not reach it.
func TestHandler_getRIExchangeHistory_UnregisteredHostAccount(t *testing.T) {
	t.Parallel()
	accounts := []config.CloudAccount{
		{ID: "u-a", Name: "a", Provider: "aws", ExternalID: exchangeAcctA},
	}
	stored := []config.RIExchangeRecord{
		{ID: "a-1", AccountID: exchangeAcctA},
		{ID: "x-1", AccountID: exchangeAcctOther},
	}
	runExchangeHistoryScopeCases(t, accounts, stored, []exchangeHistoryScopeCase{
		{
			name:    "literal sts id of the unregistered host sees its exchanges",
			scope:   []string{exchangeAcctOther},
			wantIDs: []string{exchangeAcctOther},
			want:    []string{"x-1"},
		},
		{
			name:    "a registered account's uuid does not reach the unregistered host",
			scope:   []string{"u-a"},
			wantIDs: []string{exchangeAcctA, "u-a"},
			want:    []string{"a-1"},
		},
		{
			name:    "uuid and literal sts id together see both",
			scope:   []string{"u-a", exchangeAcctOther},
			wantIDs: []string{exchangeAcctA, exchangeAcctOther, "u-a"},
			want:    []string{"a-1", "x-1"},
		},
	})
}

// Issue #558: when two registered AWS accounts carry the same STS id, the id
// resolves to neither, so neither account's uuid reaches its exchanges. Both
// uuids are tested so that resolving to the first or to the last duplicate
// each fails a case. Only the literal STS id still covers them.
func TestHandler_getRIExchangeHistory_AmbiguousHostAccount(t *testing.T) {
	t.Parallel()
	accounts := []config.CloudAccount{
		{ID: "u-a1", Name: "a1", Provider: "aws", ExternalID: exchangeAcctA},
		{ID: "u-a2", Name: "a2", Provider: "aws", ExternalID: exchangeAcctA},
		{ID: "u-b", Name: "b", Provider: "aws", ExternalID: exchangeAcctB},
	}
	stored := []config.RIExchangeRecord{
		{ID: "a-1", AccountID: exchangeAcctA},
		{ID: "b-1", AccountID: exchangeAcctB},
	}
	runExchangeHistoryScopeCases(t, accounts, stored, []exchangeHistoryScopeCase{
		{
			name:    "uuid of the first duplicate reaches nothing",
			scope:   []string{"u-a1"},
			wantIDs: []string{"u-a1"},
			want:    []string{},
		},
		{
			name:    "uuid of the second duplicate reaches nothing",
			scope:   []string{"u-a2"},
			wantIDs: []string{"u-a2"},
			want:    []string{},
		},
		{
			name:    "literal sts id still sees the shared id's exchanges",
			scope:   []string{exchangeAcctA},
			wantIDs: []string{exchangeAcctA},
			want:    []string{"a-1"},
		},
		{
			name:    "an unambiguous account beside the duplicates still resolves",
			scope:   []string{"u-b"},
			wantIDs: []string{exchangeAcctB, "u-b"},
			want:    []string{"b-1"},
		},
	})
}

// Issue #558: when the cloud account list cannot be loaded, a scoped history
// request fails rather than matching the allow-list by literal ids only or
// falling back to the unscoped query. Both store queries are set up to answer,
// so a handler that carried on would return records instead of the error.
func TestHandler_getRIExchangeHistory_AccountListErrorFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := new(MockConfigStore)
	rows := []config.RIExchangeRecord{{ID: "b-1", AccountID: exchangeAcctB}}
	store.On("GetRIExchangeHistory", ctx, mock.Anything, 500).Return(rows, nil).Maybe()
	store.On("GetRIExchangeHistoryForAccounts", ctx, mock.Anything, 500, mock.Anything).Return(rows, nil).Maybe()
	h, req := scopedExchangeHistoryHandler(ctx, store)
	store.ListCloudAccountsFn = func(_ context.Context, _ config.CloudAccountFilter) ([]config.CloudAccount, error) {
		return nil, errors.New("list failed")
	}

	result, err := h.getRIExchangeHistory(ctx, req)
	require.ErrorContains(t, err, "failed to list cloud accounts for exchange history scope: list failed")
	assert.Nil(t, result)
	store.AssertNotCalled(t, "GetRIExchangeHistory", mock.Anything, mock.Anything, mock.Anything)
	store.AssertNotCalled(t, "GetRIExchangeHistoryForAccounts", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}
