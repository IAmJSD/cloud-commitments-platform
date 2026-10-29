package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// pageStore serves PageRecommendations' store calls: the override keys of
// triples, the suppressions in sups, and a page built by page, which sees the
// query (and can call its DecideSuppressed) like the Postgres store does.
type pageStore struct {
	mockOverrideStore
	triples []config.RecommendationTriple
	sups    []config.PurchaseSuppression
	supsErr error
	page    func(q config.RecommendationPageQuery) (config.RecommendationPage, error)
	queries []config.RecommendationPageQuery
}

func (m *pageStore) ListActiveSuppressions(_ context.Context) ([]config.PurchaseSuppression, error) {
	return m.sups, m.supsErr
}

func (m *pageStore) ListStoredRecommendationTriples(_ context.Context, _ config.RecommendationListScope) ([]config.RecommendationTriple, error) {
	return m.triples, nil
}

func (m *pageStore) PageStoredRecommendations(_ context.Context, q config.RecommendationPageQuery) (config.RecommendationPage, error) {
	m.queries = append(m.queries, q)
	return m.page(q)
}

const pageAccount = "11111111-1111-4111-8111-111111111111"

func pageRec(id, service, resourceType string, count int) config.RecommendationRecord {
	account := pageAccount
	return config.RecommendationRecord{ID: id, Provider: "aws", Service: service, Region: "us-east-1",
		ResourceType: resourceType, Count: count, CloudAccountID: &account}
}

func pageSuppression(service, resourceType string, count int) config.PurchaseSuppression {
	return config.PurchaseSuppression{ExecutionID: "exec-" + resourceType, AccountID: pageAccount, Provider: "aws",
		Service: service, Region: "us-east-1", ResourceType: resourceType, SuppressedCount: count,
		ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now()}
}

func newPageScheduler(store *pageStore) *Scheduler {
	return &Scheduler{config: store, isLambda: true}
}

// TestPageRecommendations_SuppressionsDecidedInGo checks the suppression
// split: the store is asked to match every stored spelling of a suppressed
// service, rows on those tuples are decided by applySuppressionIndex and the
// override filter, and the page rows come back with their badge fields.
func TestPageRecommendations_SuppressionsDecidedInGo(t *testing.T) {
	store := &pageStore{
		triples: []config.RecommendationTriple{{AccountID: pageAccount, Provider: "aws", Service: "ec2", HasAccount: true}},
		sups:    []config.PurchaseSuppression{pageSuppression("ec2", "m5.large", 2), pageSuppression("compute", "c5.large", 5)},
	}
	store.globals = map[string]*config.ServiceConfig{"aws|ec2": {Provider: "aws", Service: "ec2", Enabled: true, MinCount: 2}}
	var decided []string
	store.page = func(q config.RecommendationPageQuery) (config.RecommendationPage, error) {
		require.NotNil(t, q.DecideSuppressed)
		var err error
		decided, err = q.DecideSuppressed([]config.StoredRecommendation{
			{RowID: "keep", Rec: pageRec("a", "compute", "m5.large", 6)}, // 6-2=4 >= min_count 2
			{RowID: "floor", Rec: pageRec("b", "ec2", "m5.large", 3)},    // 3-2=1 < min_count 2
			{RowID: "gone", Rec: pageRec("c", "ec2", "c5.large", 5)},     // fully suppressed
		})
		require.NoError(t, err)
		return config.RecommendationPage{TotalCount: 1, Rows: []config.RecommendationRecord{pageRec("a", "compute", "m5.large", 6)}}, nil
	}
	page, err := newPageScheduler(store).PageRecommendations(context.Background(), config.RecommendationListScope{}, "", 10)
	require.NoError(t, err)

	assert.Equal(t, []string{"keep"}, decided)
	require.Len(t, store.queries, 1)
	keys := store.queries[0].SuppressionKeys
	for _, service := range []string{"compute", "ec2"} {
		assert.Contains(t, keys, config.SuppressionTupleKey(pageAccount, "aws", service, "us-east-1", "m5.large", ""))
	}
	require.Len(t, store.queries[0].Rules, 1)
	assert.Equal(t, config.AccountConfigKey(pageAccount, "aws", "ec2"), store.queries[0].Rules[0].Key)
	assert.NotNil(t, store.queries[0].Rules[0].Config)
	require.Len(t, page.Rows, 1)
	assert.Equal(t, 4, page.Rows[0].Count, "the page row carries the suppressed count")
	assert.Equal(t, 2, page.Rows[0].SuppressedCount)
}

// TestPageRecommendations_FallsBackToScan covers each reason the SQL page
// hands the request back to the Go scan.
func TestPageRecommendations_FallsBackToScan(t *testing.T) {
	triple := config.RecommendationTriple{AccountID: pageAccount, Provider: "aws", Service: "rds", HasAccount: true}
	cases := []struct {
		name  string
		setup func(*pageStore)
	}{
		{"override resolution fails", func(s *pageStore) { s.getGlobalErr = errors.New("db down") }},
		{"empty but present account", func(s *pageStore) {
			s.triples = []config.RecommendationTriple{{Provider: "aws", Service: "rds", HasAccount: true}}
		}},
		{"two keys join to one string", func(s *pageStore) {
			s.triples = []config.RecommendationTriple{
				{AccountID: "a", Provider: "b|c", Service: "d", HasAccount: true},
				{AccountID: "a|b", Provider: "c", Service: "d", HasAccount: true},
			}
		}},
		{"store reports a fallback", func(s *pageStore) {
			s.page = func(config.RecommendationPageQuery) (config.RecommendationPage, error) {
				return config.RecommendationPage{Fallback: "unsure rows"}, nil
			}
		}},
		{"page row the override filter drops", func(s *pageStore) {
			s.globals = map[string]*config.ServiceConfig{"aws|rds": {Provider: "aws", Service: "rds", Enabled: false}}
		}},
		{"page row fully suppressed", func(s *pageStore) {
			s.sups = []config.PurchaseSuppression{pageSuppression("rds", "db.r5.large", 9)}
			s.page = func(q config.RecommendationPageQuery) (config.RecommendationPage, error) {
				return config.RecommendationPage{TotalCount: 1, Rows: []config.RecommendationRecord{pageRec("a", "rds", "db.r5.large", 1)}}, nil
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &pageStore{triples: []config.RecommendationTriple{triple}}
			store.page = func(config.RecommendationPageQuery) (config.RecommendationPage, error) {
				return config.RecommendationPage{TotalCount: 1, Rows: []config.RecommendationRecord{pageRec("a", "rds", "db.r5.large", 1)}}, nil
			}
			tc.setup(store)
			_, err := newPageScheduler(store).PageRecommendations(context.Background(), config.RecommendationListScope{}, "", 10)
			require.ErrorIs(t, err, ErrRecommendationsPageNeedsScan)
		})
	}
}

// TestPageRecommendations_SuppressionReadFailsClosed keeps issue #111: a
// suppression read failure fails the request instead of falling back.
func TestPageRecommendations_SuppressionReadFailsClosed(t *testing.T) {
	store := &pageStore{supsErr: errors.New("db down")}
	_, err := newPageScheduler(store).PageRecommendations(context.Background(), config.RecommendationListScope{}, "", 10)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrRecommendationsPageNeedsScan)
	assert.Empty(t, store.queries)
}
