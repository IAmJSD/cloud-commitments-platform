-- Add last_collection_partial to recommendations_state.
--
-- Issue #87: a multi-subscription sweep that loses some subscriptions keeps
-- the rows of the ones that answered (tolerateIncompleteSweep), but the
-- incompleteness only reached the logs, so the dashboard rendered 1 of 10
-- subscriptions exactly like full coverage. This column records an
-- aggregate, sanitized summary of the most recent collection's incomplete
-- sweeps (per-provider counts and reason categories, never raw provider
-- errors or subscription IDs). NULL means the last persisted collection was
-- complete; UpsertRecommendations/ReplaceRecommendations reset it to NULL
-- and the scheduler re-sets it when a sweep was partial.
ALTER TABLE recommendations_state
    ADD COLUMN IF NOT EXISTS last_collection_partial JSONB;
