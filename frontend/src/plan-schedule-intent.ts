import { suggestedPurchaseStartDate } from './lib/calendar-date';

export interface PlanScheduleIntentInput {
  name: string;
  preset: string;
  coverage: string;
  stepPercent: string;
  intervalDays: string;
  notificationDays: string;
  term: string;
  payment: string;
}

export function integer(value: string, label: string, min: number, max: number): number {
  const parsed = value.trim() === '' ? NaN : Number(value);
  if (!Number.isInteger(parsed) || parsed < min || parsed > max) throw new Error(`${label} must be a whole number between ${min} and ${max}`);
  return parsed;
}

export function buildLocalScheduleIntent(input: PlanScheduleIntentInput): { coverage: number; notificationDays: number; dates: string[]; shares: number[] } {
  const coverage = integer(input.coverage, 'Target coverage', 1, 100);
  const notice = integer(input.notificationDays, 'Notification days', 1, 30);
  // Mirrors presets/shares in internal/config/types.go and custom
  // step counting in PlanRequest.buildCustomRampSchedule, internal/api/types.go.
  const presets: Record<string, readonly [number, number]> = { immediate: [100, 0], 'weekly-25pct': [25, 7], 'monthly-10pct': [10, 30] };
  const preset = Object.prototype.hasOwnProperty.call(presets, input.preset) ? presets[input.preset] : undefined;
  if (!preset && input.preset !== 'custom') throw new Error('Choose a valid ramp schedule before previewing');
  const percent = preset?.[0] ?? integer(input.stepPercent, 'Step percent', 1, 100);
  const interval = preset?.[1] ?? integer(input.intervalDays, 'Interval days', 1, 365);
  const count = Math.floor(100 / percent);
  const start = Date.parse(`${suggestedPurchaseStartDate(notice)}T00:00:00Z`);
  const dates = Array.from({ length: count }, (_, index) => new Date(start + index * interval * 86_400_000).toISOString().slice(0, 10));
  return { coverage, notificationDays: notice, dates, shares: dates.map((_, index) => Math.min(100, (index + 1) * percent)) };
}
