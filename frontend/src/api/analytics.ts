/**
 * Savings-snapshot analytics API (issue #30).
 *
 * GET /api/analytics/trends returns the historical savings-snapshot series
 * collected by the analytics collector (savings_snapshots + the
 * monthly_savings_summary materialized view). It is distinct from the
 * purchase_history-backed /history/analytics the Purchases chart reads.
 * Response shape mirrors TrendsResponse in internal/api/handler_analytics.go.
 */

import { apiRequest } from './client';

/**
 * One (month, account, provider, service) bucket of the monthly series.
 * total_savings is the month's average monthly savings run-rate.
 * avg_coverage is omitted when no coverage figure was collected for the
 * bucket; it is absent data, not 0% coverage.
 */
export interface TrendsMonthlyRow {
  month: string;
  account_id: string;
  cloud_account_id?: string;
  provider: string;
  service: string;
  total_savings: number;
  avg_coverage?: number | null;
  snapshot_count: number;
}

/** Savings averaged over the window for one (provider, service) bucket. */
export interface TrendsProviderRow {
  provider: string;
  service: string;
  total_savings: number;
  avg_coverage?: number | null;
}

/** Savings averaged over the window for one (service, region) bucket. */
export interface TrendsServiceRow {
  service: string;
  region: string;
  total_savings: number;
  avg_coverage?: number | null;
}

export interface AnalyticsTrendsResponse {
  start: string;
  end: string;
  months: number;
  monthly: TrendsMonthlyRow[] | null;
  by_provider: TrendsProviderRow[] | null;
  by_service: TrendsServiceRow[] | null;
}

export interface AnalyticsTrendsFilter {
  /** Selected cloud_accounts UUIDs, sent as account_ids; empty or omitted means all accessible. */
  accountIDs?: string[];
  /** Narrows by_service server-side; omitted means every provider. */
  provider?: string;
  /** RFC 3339 window bounds. The backend caps the window at 366 days. */
  start?: string;
  end?: string;
}

/**
 * Fetch the savings-snapshot trends. Every filter is optional and only
 * forwarded when set, so an unfiltered call hits the bare endpoint.
 */
export async function getAnalyticsTrends(filter: AnalyticsTrendsFilter = {}): Promise<AnalyticsTrendsResponse> {
  const params = new URLSearchParams();
  if (filter.accountIDs && filter.accountIDs.length > 0) params.set('account_ids', filter.accountIDs.join(','));
  if (filter.provider) params.set('provider', filter.provider);
  if (filter.start) params.set('start', filter.start);
  if (filter.end) params.set('end', filter.end);
  const qs = params.toString();
  return apiRequest<AnalyticsTrendsResponse>(`/analytics/trends${qs ? `?${qs}` : ''}`);
}
