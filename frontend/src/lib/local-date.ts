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

/**
 * Translate a local-calendar From/To pair into the UTC calendar days an API
 * that filters on whole UTC days needs to cover it. The From day starts at
 * local midnight and the To day ends at the following local midnight; each
 * instant is mapped to the UTC day containing it. The result is the
 * smallest UTC-day range that includes every row the user's local range
 * covers, so no row on the chosen local days is silently filtered out (it
 * may include a few hours past either edge). Empty or malformed values pass
 * through unchanged so the server keeps reporting its own validation error.
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
  return {
    start: startDay && !Number.isNaN(startDay.getTime()) ? utcDay(startDay.getTime()) : start,
    end: Number.isFinite(nextMidnight) ? utcDay(nextMidnight - 1) : end,
  };
}
