package scheduler

import (
	"context"
	"errors"
	"fmt"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// ScanRecommendations is ListRecommendations for a caller that pages through
// the cache without holding all of it (issue #981). It walks the cached
// recommendations in chunks of at most chunkSize rows, in byte-wise ID order
// (the order of Go's string comparison), applies the same suppressions and
// account overrides as ListRecommendations to each chunk, and calls fn with
// each chunk's surviving rows in that order. fn may keep what it needs; the
// scan itself holds one chunk at a time. An error from fn stops the scan and
// is returned.
//
// It has the same cold-start and stale-while-revalidate side effects as
// ListRecommendations, and the same failure directions: a suppression read
// failure fails the scan before any chunk reaches fn (issue #111), while an
// override read failure is logged and the affected rows pass through.
//
// Each chunk is one consistent read; the walk as a whole is not one snapshot.
// A row upserted in place keeps its position, so it is visited once. A row
// inserted behind the scan position during the walk is missed by this walk,
// and a row evicted ahead of it is not visited.
func (s *Scheduler) ScanRecommendations(ctx context.Context, filter config.RecommendationFilter, chunkSize int, fn func([]config.RecommendationRecord) error) error {
	if chunkSize < 1 {
		return errors.New("recommendations scan chunk size must be positive")
	}
	logging.Info("Scanning recommendations from cache...")

	freshness, err := s.startCacheRead(ctx)
	if err != nil {
		return err
	}

	suppressions, err := s.loadSuppressionIndex(ctx)
	if err != nil {
		return err
	}
	if err := s.scanChunks(ctx, filter, chunkSize, suppressions, fn); err != nil {
		return err
	}

	s.finishCacheRead(ctx, freshness)
	return nil
}

// scanChunks is ScanRecommendations' walk: it reads the cache chunk by chunk,
// applies the suppression index and account overrides to each chunk, and
// hands the non-empty survivors to fn.
func (s *Scheduler) scanChunks(ctx context.Context, filter config.RecommendationFilter, chunkSize int, suppressions map[suppressionKey][]config.PurchaseSuppression, fn func([]config.RecommendationRecord) error) error {
	overrides := newOverrideScanFilter(s.config)
	var after *config.RecommendationScanKey
	for {
		chunk, err := s.config.ListStoredRecommendationsChunk(ctx, filter, after, chunkSize)
		if err != nil {
			return err
		}
		recs := chunk.Recs
		if len(suppressions) > 0 {
			recs = applySuppressionIndex(recs, suppressions)
		}
		recs = overrides.apply(ctx, recs)
		if len(recs) > 0 {
			if err := fn(recs); err != nil {
				return err
			}
		}
		if chunk.Done || chunk.Last == nil {
			return nil
		}
		after = chunk.Last
	}
}

// loadSuppressionIndex reads the active suppressions once for a whole scan and
// groups them by 6-tuple, as applySuppressions does. Which rows of a group
// apply to a rec depends only on that rec's variant (issue #816), so applying
// the index chunk by chunk suppresses exactly what ListRecommendations does.
// A read failure is returned, so the scan fails closed like
// ListRecommendations does (issue #111).
func (s *Scheduler) loadSuppressionIndex(ctx context.Context) (map[suppressionKey][]config.PurchaseSuppression, error) {
	sups, err := s.config.ListActiveSuppressions(ctx)
	if err != nil {
		return nil, fmt.Errorf("apply suppressions: %w", err)
	}
	if len(sups) == 0 {
		return nil, nil
	}
	return aggregateSuppressions(sups), nil
}

// overrideScanFilter applies account overrides chunk by chunk, resolving each
// (account, provider, service) triple once per scan instead of once per chunk.
type overrideScanFilter struct {
	store    config.AccountConfigReader
	resolved map[string]*config.ServiceConfig
	seen     map[string]struct{}
}

func newOverrideScanFilter(store config.AccountConfigReader) *overrideScanFilter {
	return &overrideScanFilter{
		store:    store,
		resolved: make(map[string]*config.ServiceConfig),
		seen:     make(map[string]struct{}),
	}
}

// apply resolves the triples recs introduces and filters recs with every
// configuration resolved so far. On a resolution error the chunk passes
// through unfiltered (logged), and its new triples stay unresolved so a later
// chunk retries them.
func (o *overrideScanFilter) apply(ctx context.Context, recs []config.RecommendationRecord) []config.RecommendationRecord {
	if len(recs) == 0 {
		return recs
	}
	var unseen []config.RecommendationRecord
	var unseenKeys []string
	for i := range recs {
		key := recAccountConfigKey(&recs[i])
		if _, ok := o.seen[key]; ok {
			continue
		}
		o.seen[key] = struct{}{}
		unseen = append(unseen, recs[i])
		unseenKeys = append(unseenKeys, key)
	}
	if len(unseen) > 0 {
		resolved, err := config.ResolveAccountConfigsForRecs(ctx, o.store, unseen)
		if err != nil {
			for _, key := range unseenKeys {
				delete(o.seen, key)
			}
			logging.Errorf("failed to apply account overrides; returning un-filtered recs: %v", err)
			return recs
		}
		for key, cfg := range resolved {
			o.resolved[key] = cfg
		}
	}
	if len(o.resolved) == 0 {
		return recs
	}
	return filterRecsByResolvedConfigs(recs, o.resolved)
}

// recAccountConfigKey is the AccountConfigKey filterRecsByResolvedConfigs
// looks a rec up by.
func recAccountConfigKey(rec *config.RecommendationRecord) string {
	accountID := ""
	if rec.CloudAccountID != nil {
		accountID = *rec.CloudAccountID
	}
	return config.AccountConfigKey(accountID, rec.Provider, rec.Service)
}
