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

export function ladderTotal(events: readonly LadderEvent[]): string {
  return formatLadderUnits(events.reduce((total, event) => total + ladderAmountUnits(event.amount_usd_hr), 0n));
}

export function ladderEditReason(event: LadderEvent, now = Date.now()): string | undefined {
  if (event.run_status !== 'planned') return `Run is ${event.run_status}.`;
  if (event.status !== 'scheduled') return `Tranche is ${event.status}.`;
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
  return `${formatCurrency(value, '$', value > 0 && value < 1 ? 4 : 2)}/hr`;
}

/** Calendar date and UTC time ("Jan 2, 2030, 00:00 UTC"); never browser-local. Unparseable input is shown raw. */
export function formatLadderDateTime(iso: string): string {
  const stamp = Date.parse(iso);
  if (!Number.isFinite(stamp)) return iso;
  const utc = new Date(stamp).toISOString();
  return `${formatDate(utc.slice(0, 10))}, ${utc.slice(11, 16)} UTC`;
}
