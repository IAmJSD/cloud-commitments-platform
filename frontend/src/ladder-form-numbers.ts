/**
 * Reads the required numeric fields of the per-account ladder config form.
 *
 * A cleared <input type="number"> has value '', and Number('') is 0. The
 * backend rejects an explicit 0 for most of these fields, but accepts it for
 * buffer_fraction as a deliberate "no buffer", so a field the user emptied
 * and forgot about silently removed the over-commitment safety buffer. An
 * empty or non-numeric field now blocks the save with a message naming it.
 * A missing element (form not rendered) still falls back to the documented
 * default.
 */
import type { LadderConfig } from './api';

export type LadderFormNumbers = Pick<LadderConfig,
  'target_coverage' | 'buffer_fraction' | 'baseline_percentile' | 'lookback_days'
  | 'buffer_utilization_threshold' | 'max_actions_per_run'>;

export type LadderNumbersResult =
  | { ok: true; values: LadderFormNumbers }
  | { ok: false; message: string; field: HTMLInputElement | null };

const FIELDS: ReadonlyArray<[keyof LadderFormNumbers, string, string, number]> = [
  ['target_coverage', 'ladder-cfg-target-coverage', 'Target coverage', 100],
  ['buffer_fraction', 'ladder-cfg-buffer-fraction', 'Buffer fraction', 0.10],
  ['baseline_percentile', 'ladder-cfg-baseline-percentile', 'Baseline percentile', 5],
  ['lookback_days', 'ladder-cfg-lookback-days', 'Lookback days', 30],
  ['buffer_utilization_threshold', 'ladder-cfg-buf-util-threshold', 'Buffer utilization threshold', 90],
  ['max_actions_per_run', 'ladder-cfg-max-actions', 'Max actions per run', 10],
];

export function readLadderNumbers(): LadderNumbersResult {
  const values: Partial<LadderFormNumbers> = {};
  for (const [key, id, label, fallback] of FIELDS) {
    const field = document.getElementById(id) as HTMLInputElement | null;
    const raw = field ? field.value.trim() : String(fallback);
    const n = Number(raw);
    if (raw === '' || !Number.isFinite(n)) {
      return { ok: false, message: `${label} is required. Enter a number before saving.`, field };
    }
    values[key] = n;
  }
  return { ok: true, values: values as LadderFormNumbers };
}
