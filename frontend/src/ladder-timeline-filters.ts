import type { LadderEvent } from './api/ladder';
import { formatCurrency, formatDate, formatTerm } from './utils';
import { getPaymentLabel } from './commitmentOptions';

export const ladderDimensions = ['run_id', 'layer_type', 'term', 'payment_option', 'status'] as const;
export type LadderDimension = typeof ladderDimensions[number];
export type LadderFilters = Record<LadderDimension, string> & { from: string; to: string };

export function emptyLadderFilters(): LadderFilters {
  return { run_id: '', layer_type: '', term: '', payment_option: '', status: '', from: '', to: '' };
}

export function filterLadderEvents(events: readonly LadderEvent[], filters: LadderFilters, except?: LadderDimension): LadderEvent[] {
  return events.filter(event => {
    if (ladderDimensions.some(key => key !== except && filters[key] !== '' && filters[key] !== event[key])) return false;
    const stamp = Date.parse(event.scheduled_date);
    if (filters.from && stamp < Date.parse(`${filters.from}Z`)) return false;
    if (filters.to && stamp > Date.parse(`${filters.to}Z`)) return false;
    return true;
  });
}

export function ladderFilterOptions(events: readonly LadderEvent[], filters: LadderFilters, key: LadderDimension): string[] {
  return Array.from(new Set(filterLadderEvents(events, filters, key).map(event => event[key]))).sort();
}

export function ladderAmountUnits(amount: string): bigint {
  if (!/^(0|[1-9]\d{0,13})(\.\d{1,6})?$/.test(amount)) throw new Error('Invalid planning budget.');
  const [whole = '0', fraction = ''] = amount.split('.');
  return BigInt(whole) * 1000000n + BigInt(fraction.padEnd(6, '0'));
}

export function formatLadderUnits(units: bigint): string {
  return `${units / 1000000n}.${(units % 1000000n).toString().padStart(6, '0')}`;
}

/** On-demand-equivalent amount; absent on an older API, whose amounts were all on-demand-equivalent. */
export function ladderOdAmount(event: LadderEvent): string {
  return event.od_usd_hr ?? event.amount_usd_hr;
}

export function ladderTotalOd(events: readonly LadderEvent[]): string {
  return formatLadderUnits(events.reduce((total, event) => total + ladderAmountUnits(ladderOdAmount(event)), 0n));
}

export function ladderTotal(events: readonly LadderEvent[]): string {
  return formatLadderUnits(events.reduce((total, event) => total + ladderAmountUnits(event.amount_usd_hr), 0n));
}

export function ladderEditReason(event: LadderEvent, now = Date.now()): string | undefined {
  if (event.run_status !== 'planned') return `Run is ${runStatusLabels[event.run_status] ?? event.run_status}.`;
  if (event.status !== 'scheduled') return `Tranche is ${ladderLabel('status', event.status).toLowerCase()}.`;
  if (event.execution_id) return 'An execution is already linked.';
  if (Date.parse(event.scheduled_date) <= now) return 'The scheduled date has passed.';
  if (!Number.isSafeInteger(event.revision) || event.revision < 0) return 'Editing is unavailable until the timeline API is upgraded.';
  return undefined;
}

// Enum values from cloud-commitments-go pkg/ladder (types.go, store.go).
const layerLabels: Record<string, string> = {
  'ec2-instance-sp': 'EC2 Instance Savings Plan', 'compute-sp': 'Compute Savings Plan', 'convertible-ri': 'Convertible RI',
  'azure-reservation': 'Azure Reservation', 'azure-savings-plan': 'Azure Savings Plan',
};
const statusLabels: Record<string, string> = {
  scheduled: 'Scheduled', fired: 'Fired', completed: 'Completed', cancelled: 'Cancelled', failed: 'Failed',
};

const runStatusLabels: Record<string, string> = {
  planned: 'planned', awaiting_approval: 'awaiting approval', approved: 'approved', executing: 'executing',
  completed: 'completed', failed: 'failed', cancelled: 'cancelled', expired: 'expired',
};

/** Label for a ladder enum code. An unknown code is shown raw so it stays visible. */
export function ladderLabel(dimension: LadderDimension, value: string): string {
  switch (dimension) {
    case 'layer_type': return layerLabels[value] ?? value;
    case 'status': return statusLabels[value] ?? value;
    case 'payment_option': return getPaymentLabel(value);
    case 'term': {
      const years = /^([1-9]\d?)yr$/.exec(value);
      return years ? formatTerm(Number(years[1])) : value;
    }
    case 'run_id': return value;
  }
}

/** USD/hour budget for display: 2 digits from $1, 4 below $1 so a small non-zero value never reads as $0.00. */
export function formatLadderAmount(amount: string | null | undefined): string {
  if (amount === null || amount === undefined) return 'unavailable';
  ladderAmountUnits(amount);
  const value = Number(amount);
  if (value > 0 && value < 0.0001) return '<$0.0001/hr';
  return `${formatCurrency(value, '$', value > 0 && value < 1 ? 4 : 2)}/hr`;
}

/** Calendar date and UTC time ("Jan 2, 2030, 00:00 UTC"); never browser-local. Unparseable input is shown raw. */
export function formatLadderDateTime(iso: string): string {
  const stamp = Date.parse(iso);
  if (!Number.isFinite(stamp)) return iso;
  const utc = new Date(stamp).toISOString();
  return `${formatDate(utc.slice(0, 10))}, ${utc.slice(11, 16)} UTC`;
}

const UTC_INPUT = /^(\d{4})-(\d{2})-(\d{2})(?:[T ](\d{2}):(\d{2})(?::(\d{2}))?)?(?:\s*(?:Z|UTC))?$/i;

/**
 * Parses a typed UTC filter date ("YYYY-MM-DD", optionally with "HH:MM" or
 * "HH:MM:SS", "T" or a space between) into the filter's "YYYY-MM-DDTHH:MM:SS"
 * form. A date without a time covers the whole day: 00:00:00 for "from" and
 * 23:59:59 for "to". Returns '' for blank input and null when it is not a real
 * date, so callers never guess at an ambiguous value.
 */
export function parseLadderUtcInput(text: string, edge: 'from' | 'to'): string | null {
  const trimmed = text.trim();
  if (!trimmed) return '';
  const match = UTC_INPUT.exec(trimmed);
  if (!match) return null;
  const [, year, month, day, hour, minute, second] = match;
  const time = hour === undefined ? (edge === 'from' ? '00:00:00' : '23:59:59') : `${hour}:${minute}:${second ?? '00'}`;
  const value = `${year}-${month}-${day}T${time}`;
  const stamp = Date.parse(`${value}Z`);
  // Date.parse rolls 2026-02-30 over to March; the round trip rejects it.
  return Number.isFinite(stamp) && new Date(stamp).toISOString().slice(0, 19) === value ? value : null;
}

/** Shows a "YYYY-MM-DDTHH:MM:SS" filter value as "YYYY-MM-DD HH:MM", keeping seconds only when set. */
export function formatLadderUtcInput(value: string): string {
  if (!value) return '';
  const [date, time = '00:00:00'] = value.split('T');
  return `${date} ${time.endsWith(':00') ? time.slice(0, 5) : time}`;
}

/** Statuses that count toward cumulative commitment; cancelled and failed tranches were never bought. */
export const ladderCountedStatuses: readonly string[] = ['scheduled', 'fired', 'completed'];
export const LADDER_COUNTED_NOTE = 'Counts scheduled, fired and completed tranches; cancelled, failed and unrecognised statuses are excluded.';

/** Note for a row that is excluded from the cumulative, or undefined when it is counted. */
export function ladderNotCountedNote(event: LadderEvent): string | undefined {
  if (ladderCountedStatuses.includes(event.status)) return undefined;
  return event.status in statusLabels ? 'Not counted' : 'Not counted (unknown status)';
}

export interface LadderCumulative {
  times: number[];
  layers: { layer: string; units: bigint[] }[];
  /** Base plus every layer, per entry of `times`. */
  totals: bigint[];
  /** Running total (base plus counted tranches up to and including this row), keyed by event id; null for rows that are not counted. */
  rowTotals: Map<string, bigint | null>;
}

/** Cumulative planned commitment per layer over `times`; `sorted` must be ordered by scheduled_date. Uncounted statuses are skipped. */
export function ladderCumulative(sorted: readonly LadderEvent[], base: bigint): LadderCumulative {
  const counted = sorted.filter(event => ladderNotCountedNote(event) === undefined);
  const times = Array.from(new Set(counted.map(event => Date.parse(event.scheduled_date))));
  const layers = Array.from(new Set(counted.map(event => event.layer_type))).map(layer => ({ layer, units: times.map(() => 0n) }));
  const rowTotals = new Map<string, bigint | null>();
  let running = base;
  for (const event of sorted) {
    if (ladderNotCountedNote(event) !== undefined) { rowTotals.set(event.id, null); continue; }
    const amount = ladderAmountUnits(ladderOdAmount(event));
    running += amount;
    rowTotals.set(event.id, running);
    const at = times.indexOf(Date.parse(event.scheduled_date));
    const series = layers.find(item => item.layer === event.layer_type)!;
    for (let i = at; i < times.length; i++) series.units[i] = series.units[i]! + amount;
  }
  const totals = times.map((_, i) => layers.reduce((sum, item) => sum + item.units[i]!, base));
  return { times, layers, totals, rowTotals };
}
