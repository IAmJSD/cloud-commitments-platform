package api

import (
	"context"
	"fmt"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/require"
)

type amendmentTestStore struct {
	*MockConfigStore
	accountID string
	called    bool
	actor     string
	err       error
}

func (s *amendmentTestStore) LadderTrancheScope(context.Context, string) (string, string, error) {
	return s.accountID, "aws", nil
}

func (s *amendmentTestStore) AmendLadderTranche(_ context.Context, id, accountID, provider, actor string, amendment config.LadderAmendment) (*config.LadderAmendmentResult, error) {
	s.called = true
	s.actor = actor
	return &config.LadderAmendmentResult{ID: id}, s.err
}

func TestLadderAmendmentScopeAndActor(t *testing.T) {
	for _, accountID := range []string{scopedInAccount, scopedOutAccount} {
		h, original := ladderScopedHandler(t)
		store := &amendmentTestStore{MockConfigStore: original, accountID: accountID}
		h.config = store
		result, err := h.amendLadderTranche(context.Background(), ladderScopedReq(`{"expected_revision":0,"scheduled_date":"2030-01-01T00:00:00Z","amount_usd_hr":"1.123456"}`), scopedInAccount)
		if accountID == scopedOutAccount {
			require.ErrorIs(t, err, errNotFound)
			require.False(t, store.called)
			require.Nil(t, result)
		} else {
			require.NoError(t, err)
			require.True(t, store.called)
			require.Equal(t, scopedUserID, store.actor)
		}
	}
}

func TestLadderAmendmentStrictPayload(t *testing.T) {
	for _, body := range []string{
		`{"scheduled_date":"2030-01-01T00:00:00Z","amount_usd_hr":"1"}`,
		`{"expected_revision":0,"scheduled_date":"2030-01-01T00:00:00Z","amount_usd_hr":"1","actor":"spoof"}`,
		`{"expected_revision":0,"scheduled_date":"2030-01-01T00:00:00Z","amount_usd_hr":"1.0000001"}`,
		`{"expected_revision":0,"scheduled_date":"2020-01-01T00:00:00Z","amount_usd_hr":"1"}`,
		`{} {}`,
	} {
		h, original := ladderScopedHandler(t)
		store := &amendmentTestStore{MockConfigStore: original, accountID: scopedInAccount}
		h.config = store
		_, err := h.amendLadderTranche(context.Background(), ladderScopedReq(body), scopedInAccount)
		clientErr, ok := IsClientError(err)
		require.True(t, ok, body)
		require.Equal(t, 400, clientErr.code, body)
		require.False(t, store.called, body)
	}
}

func TestLadderAmendmentStatusErrors(t *testing.T) {
	for _, example := range []struct {
		err  error
		code int
	}{
		{config.ErrLadderAmendConflict, 409}, {config.ErrLadderAmendInvalid, 400},
		{fmt.Errorf("amend ladder tranche: %w", config.ErrLadderLockTimeout), 503},
	} {
		h, original := ladderScopedHandler(t)
		h.config = &amendmentTestStore{MockConfigStore: original, accountID: scopedInAccount, err: example.err}
		_, err := h.amendLadderTranche(context.Background(), ladderScopedReq(`{"expected_revision":0,"scheduled_date":"2030-01-01T00:00:00Z","amount_usd_hr":"1"}`), scopedInAccount)
		clientErr, ok := IsClientError(err)
		require.True(t, ok)
		require.Equal(t, example.code, clientErr.code)
		if example.code == 503 {
			require.Equal(t, "5", clientErr.headers["Retry-After"])
		}
	}
}
