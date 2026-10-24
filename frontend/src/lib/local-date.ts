/**
 * Local-calendar helpers for `<input type="date">` values (issue #51).
 *
 * A date input shows and edits a YYYY-MM-DD day in the user's LOCAL
 * calendar. `toISOString()` converts to UTC first, so seeding an input with
 * `date.toISOString().split('T')[0]` shows "tomorrow" to an evening user
 * west of UTC and "yesterday" to a morning user east of it. Build and parse
 * those values from local date components instead.
 */

/** Format a Date as the local-calendar YYYY-MM-DD value a date input expects. */
export function toLocalDateInputValue(date: Date): string {
  const year = date.getFullYear();
  const month = String(date.getMonth() + 1).padStart(2, '0');
  const day = String(date.getDate()).padStart(2, '0');
  return `${year}-${month}-${day}`;
}

/**
 * Parse a date input value (YYYY-MM-DD) as LOCAL midnight. `new Date(value)`
 * parses a bare date as UTC midnight, which reads as "yesterday evening"
 * west of UTC and rejects a same-day or next-day date that should be valid.
 */
export function parseLocalDateInputValue(value: string): Date {
  const [year, month, day] = value.split('-').map(Number);
  return new Date(year ?? 0, (month ?? 1) - 1, day ?? 1);
}

const DATE_VALUE = /^\d{4}-\d{2}-\d{2}$/;
const DAY_MS = 86_400_000;

/**
 * The longest range /api/history accepts (MaxHistoryDateRangeDays in
 * internal/api/handler_history.go). The server reads `end` as 23:59:59 UTC
 * of that day and rejects `end - start > 366 * 24h`, so the two YYYY-MM-DD
 * values may be at most 365 days apart: 366 days counting both ends.
 */
export const MAX_HISTORY_RANGE_DAYS = 366;

/** Whole days since the epoch for a YYYY-MM-DD value, ignoring time zones. */
function dayNumber(value: string): number {
  const [year, month, day] = value.split('-').map(Number);
  return Date.UTC(year ?? 0, (month ?? 1) - 1, day ?? 1) / DAY_MS;
}

/**
 * Translate a local-calendar From/To pair into the UTC calendar days an API
 * that filters on whole UTC days needs to cover it. The From day starts at
 * local midnight and the To day ends at the following local midnight; each
 * instant is mapped to the UTC day containing it. The result is the
 * smallest UTC-day range that includes every row the user's local range
 * covers, so no row on the chosen local days is silently filtered out (it
 * may include a few hours past either edge).
 *
 * Widening adds a day, so a range the server accepts as typed (up to
 * MAX_HISTORY_RANGE_DAYS days) could go one day over its cap. When it
 * would, the From side gives the extra day back: the request then keeps
 * the whole To day (usually today, the newest rows) and loses only the
 * hours of the oldest day that fall outside the server's window.
 *
 * Empty or malformed values, a From after To, and a range already over the
 * cap pass through unchanged so the server keeps reporting its own
 * validation error.
 */
export function localDayRangeToUtcDays(
  start: string | undefined,
  end: string | undefined,
): { start: string | undefined; end: string | undefined } {
  const utcDay = (instant: number): string => new Date(instant).toISOString().slice(0, 10);
  const startDay = start && DATE_VALUE.test(start) ? parseLocalDateInputValue(start) : null;
  const endDay = end && DATE_VALUE.test(end) ? parseLocalDateInputValue(end) : null;
  const nextMidnight = endDay
    ? new Date(endDay.getFullYear(), endDay.getMonth(), endDay.getDate() + 1).getTime()
    : NaN;
  const hasStart = startDay !== null && !Number.isNaN(startDay.getTime());
  const hasEnd = Number.isFinite(nextMidnight);
  let utcStart = hasStart ? utcDay(startDay.getTime()) : start;
  const utcEnd = hasEnd ? utcDay(nextMidnight - 1) : end;
  if (hasStart && hasEnd) {
    const maxGap = MAX_HISTORY_RANGE_DAYS - 1;
    const localGap = dayNumber(end as string) - dayNumber(start as string);
    if (localGap < 0 || localGap > maxGap) return { start, end };
    const lastDay = dayNumber(utcEnd as string);
    if (lastDay - dayNumber(utcStart as string) > maxGap) {
      utcStart = utcDay((lastDay - maxGap) * DAY_MS);
    }
  }
  return { start: utcStart, end: utcEnd };
}
