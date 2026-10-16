/**
 * Commitment Laddering API functions (issue #1333 phase 3).
 *
 * The feature is flag-gated default-off: the global kill-switch
 * (global_config.laddering_enabled) must be true AND the per-account
 * LadderConfig.enabled must be true before any laddering engine run fires.
 */

import { apiRequest } from './client';
import { ladderTotal, ladderTotalOd } from '../ladder-timeline-filters';

/**
 * A single ramp step within a ladder ramp schedule.
 * AfterDays is the delay from run start; Fraction is the share of the
 * total target allocated by this tranche (fractions must sum to 1.0).
 */
export interface LadderRampStep {
  after_days: number;
  fraction: number;
}

/**
 * Per-account, per-provider ladder configuration.
 *
 * Mode controls whether runs require human approval before executing:
 *   email_approval - sends an approval email; purchases fire only after approval
 *   auto_approve   - purchases fire immediately (no human gate)
 *
 * Cadence controls how often the engine runs:
 *   daily  - once per day
 *   weekly - once per week
 *
 * All numeric money fields use number|null rather than 0 so absent/unconfigured
 * values are distinguishable from a deliberately configured $0.
 */
export interface LadderConfig {
  id?: string;
  cloud_account_id: string;
  provider: string;
  enabled: boolean;
  mode: 'email_approval' | 'auto_approve';
  cadence: 'daily' | 'weekly';
  target_coverage: number;
  buffer_fraction: number;
  baseline_percentile: number;
  lookback_days: number;
  buffer_utilization_threshold: number;
  /** null = no cap on hourly commitment delta per run */
  max_hourly_commit_per_run: number | null;
  max_actions_per_run: number;
  ramp_schedule: { steps: LadderRampStep[] };
  created_at?: string;
  updated_at?: string;
}

/**
 * List all per-account ladder configurations.
 * Requires view:config permission.
 */
export async function getLadderConfigs(): Promise<LadderConfig[]> {
  const resp = await apiRequest<{ configs: LadderConfig[] }>('/ladder/configs');
  return resp.configs ?? [];
}

/**
 * Upsert (insert or update) a per-account ladder configuration.
 * The upsert key is (cloud_account_id, provider).
 * Requires update:config permission.
 */
export async function upsertLadderConfig(cfg: LadderConfig): Promise<LadderConfig> {
  return apiRequest<LadderConfig>('/ladder/configs', {
    method: 'PUT',
    body: JSON.stringify(cfg),
  });
}

export interface LadderCursor {
  created_at: string;
  id: string;
}

export interface LadderEvent {
  id: string;
  config_id: string;
  run_id: string;
  layer_type: string;
  term: string;
  payment_option: string;
  status: string;
  run_status: string;
  execution_id?: string;
  amount_usd_hr: string;
  /** Optional so a frontend newer than the API (deploy skew) falls back to the native amount. */
  amount_basis?: 'commitment' | 'on_demand_equivalent';
  commitment_rate?: string;
  od_usd_hr?: string;
  scheduled_date: string;
  created_at: string;
  revision: number;
}

export interface LadderRun {
  id: string;
  config_id: string;
  status: string;
  started_at: string;
  created_at: string;
  baseline_usd_hr: string | null;
  target_usd_hr: string | null;
  existing_usd_hr: string | null;
  gap_usd_hr: string | null;
  total_hourly_commit: string;
  actions: { action: string; layer: string; rationale: string; data_sources?: string[] }[];
}

interface TimelinePage {
  events?: LadderEvent[];
  runs?: LadderRun[];
  total_count: number;
  total_usd_hr?: string;
  total_od_usd_hr?: string;
  next_cursor?: LadderCursor;
}

async function completeTimeline<T extends LadderEvent | LadderRun>(
  endpoint: 'tranches' | 'runs', accountID: string, provider: string,
): Promise<T[]> {
  const params = new URLSearchParams({ account_id: accountID, provider });
  const items: T[] = [];
  const seen = new Set<string>();
  let count: number | undefined;
  let total: string | undefined;
  let totalOd: string | undefined;
  while (true) {
    const page = await apiRequest<TimelinePage>(`/ladder/${endpoint}?${params}`);
    if (!page || !Number.isSafeInteger(page.total_count) || page.total_count < 0) {
      throw new Error('Invalid ladder timeline response. Reload to try again.');
    }
    if (count !== undefined && count !== page.total_count) {
      throw new Error('The ladder timeline changed while loading. Reload to try again.');
    }
    count = page.total_count;
    if (endpoint === 'tranches') {
      if (typeof page.total_usd_hr !== 'string' || (total !== undefined && total !== page.total_usd_hr)) {
        throw new Error('The ladder timeline changed while loading. Reload to try again.');
      }
      total = page.total_usd_hr;
      if (page.total_od_usd_hr !== undefined) {
        if (typeof page.total_od_usd_hr !== 'string' || (totalOd !== undefined && totalOd !== page.total_od_usd_hr)) {
          throw new Error('The ladder timeline changed while loading. Reload to try again.');
        }
        totalOd = page.total_od_usd_hr;
      }
    }
    const rows = endpoint === 'tranches' ? page.events : page.runs;
    if (!Array.isArray(rows)) throw new Error('Ladder timeline rows are unavailable.');
    for (const row of rows) {
      if (seen.has(row.id)) throw new Error('The ladder timeline changed while loading. Reload to try again.');
      seen.add(row.id);
      items.push(row as T);
    }
    if (!page.next_cursor) break;
    if (rows.length === 0 || items.length >= count) throw new Error('Invalid ladder pagination.');
    params.set('after_created_at', page.next_cursor.created_at);
    params.set('after_id', page.next_cursor.id);
  }
  if (items.length !== count) throw new Error('The ladder timeline changed while loading. Reload to try again.');
  if (endpoint === 'tranches' && ladderTotal(items as LadderEvent[]) !== total) {
    throw new Error('The ladder timeline changed while loading. Reload to try again.');
  }
  if (endpoint === 'tranches' && totalOd !== undefined && ladderTotalOd(items as LadderEvent[]) !== totalOd) {
    throw new Error('The ladder timeline changed while loading. Reload to try again.');
  }
  return items;
}

export function getLadderEvents(accountID: string, provider: string): Promise<LadderEvent[]> {
  return completeTimeline<LadderEvent>('tranches', accountID, provider);
}

export function getLadderRuns(accountID: string, provider: string): Promise<LadderRun[]> {
  return completeTimeline<LadderRun>('runs', accountID, provider);
}

export interface LadderAmendment {
  expected_revision: number;
  scheduled_date: string;
  amount_usd_hr: string;
}

export function amendLadderEvent(id: string, amendment: LadderAmendment): Promise<{
  id: string; revision: number; scheduled_date: string; amount_usd_hr: string; run_total_usd_hr: string;
}> {
  return apiRequest(`/ladder/tranches/${encodeURIComponent(id)}`, {
    method: 'PATCH', body: JSON.stringify(amendment),
  });
}
