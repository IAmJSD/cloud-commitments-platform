package config

import (
	"context"
	"errors"
	"fmt"
)

// recommendationScanKeyExpr is the application recommendation ID in the
// byte-wise order the keyset scan walks (issue #981). COLLATE "C" compares
// bytes, the same order as Go's string comparison, so paging through SQL
// returns rows in the order the API used to produce by sorting in Go.
// COALESCE maps a missing or JSON-null id to "", which is what decoding the
// payload yields. It must stay identical to the expression indexed by
// migration 000142 (recommendations_scan_key_idx), or the planner falls back
// to a full scan and sort for every chunk.
const recommendationScanKeyExpr = `(COALESCE(payload->>'id', '') COLLATE "C")`

// ListStoredRecommendationsChunk reads at most limit rows matching filter,
// ordered by recommendation ID (byte-wise) and then by the table's UUID
// primary key, starting strictly after the position after (nil means from
// the beginning). The primary key gives rows that share an ID a total order,
// so resuming from Last never skips or repeats a row, even at a run of
// duplicate IDs.
//
// The SQL-pushed conditions are the same as ListStoredRecommendations; the
// MinSavingsPct floor is still applied in-process, so Recs can be shorter
// than the rows read. Last is the position of the last row read (before that
// floor), and Done reports that fewer than limit rows were read, meaning
// nothing matches after Last. Each call is a single statement, so a chunk is
// one consistent snapshot; consecutive chunks are not.
func (s *PostgresStore) ListStoredRecommendationsChunk(ctx context.Context, filter RecommendationFilter, after *RecommendationScanKey, limit int) (RecommendationChunk, error) {
	if limit < 1 {
		return RecommendationChunk{}, errors.New("recommendations chunk limit must be positive")
	}
	if err := s.checkAzurePricingMigration(ctx, filter.RequireAzurePricingMigration); err != nil {
		return RecommendationChunk{}, err
	}
	query, args := buildRecommendationChunkQuery(filter, after, limit)
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return RecommendationChunk{}, fmt.Errorf("failed to query recommendations chunk: %w", err)
	}
	defer rows.Close()

	var chunk RecommendationChunk
	read := 0
	for rows.Next() {
		var key RecommendationScanKey
		var payload []byte
		if err := rows.Scan(&key.RowID, &key.ID, &payload); err != nil {
			return RecommendationChunk{}, fmt.Errorf("failed to scan recommendations chunk row: %w", err)
		}
		read++
		chunk.Last = &key
		rec, keep, err := decodeStoredRecommendation(payload, filter)
		if err != nil {
			return RecommendationChunk{}, err
		}
		if keep {
			chunk.Recs = append(chunk.Recs, rec)
		}
	}
	if err := rows.Err(); err != nil {
		return RecommendationChunk{}, fmt.Errorf("recommendations chunk rows iteration: %w", err)
	}
	chunk.Done = read < limit
	return chunk, nil
}

// buildRecommendationChunkQuery appends the keyset bound, ORDER BY and LIMIT
// to the ListStoredRecommendations filter. The row comparison against
// (ID, primary key) is what recommendations_scan_key_idx can serve directly.
func buildRecommendationChunkQuery(filter RecommendationFilter, after *RecommendationScanKey, limit int) (query string, args []any) {
	where, args := buildRecommendationFilter(filter)
	if after != nil {
		cond := fmt.Sprintf("(%s, id) > ($%d, $%d::uuid)", recommendationScanKeyExpr, len(args)+1, len(args)+2)
		if where == "" {
			where = " WHERE " + cond
		} else {
			where += " AND " + cond
		}
		args = append(args, after.ID, after.RowID)
	}
	args = append(args, limit)
	// The output columns are aliased and the primary key in ORDER BY is
	// table-qualified: a bare "id" there would resolve to the id::text output
	// column, a different sort key the index cannot serve.
	query = fmt.Sprintf(`SELECT recommendations.id::text AS row_id, %s AS rec_id, payload FROM recommendations%s ORDER BY %s, recommendations.id LIMIT $%d`,
		recommendationScanKeyExpr, where, recommendationScanKeyExpr, len(args))
	return query, args
}
