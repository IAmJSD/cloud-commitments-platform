/**
 * Local-calendar date input helpers (issue #51). Jest runs in
 * America/New_York (globalSetup), so the UTC and local days differ for the
 * evening instants used here. The east-of-UTC case (From widens back a
 * day) runs in Playwright under Pacific/Auckland: tests-e2e/history-local-dates.spec.ts.
 */
import { localDayRangeToUtcDays, parseLocalDateInputValue, toLocalDateInputValue } from '../lib/local-date';

describe('toLocalDateInputValue', () => {
  test('uses the local calendar day, not the UTC day', () => {
    const eveningEdt = new Date('2026-07-28T03:30:00Z'); // Jul 27, 23:30 EDT
    expect(eveningEdt.toISOString().slice(0, 10)).toBe('2026-07-28');
    expect(toLocalDateInputValue(eveningEdt)).toBe('2026-07-27');
  });

  test('zero-pads month and day', () => {
    expect(toLocalDateInputValue(new Date(2026, 0, 5))).toBe('2026-01-05');
  });
});

describe('parseLocalDateInputValue', () => {
  test('parses a bare date as local midnight', () => {
    const d = parseLocalDateInputValue('2026-03-15');
    expect([d.getFullYear(), d.getMonth(), d.getDate(), d.getHours()]).toEqual([2026, 2, 15, 0]);
  });

  test('round-trips with toLocalDateInputValue', () => {
    expect(toLocalDateInputValue(parseLocalDateInputValue('2026-11-01'))).toBe('2026-11-01');
  });
});

describe('localDayRangeToUtcDays', () => {
  test('extends the To day to the UTC day holding its local evening (west of UTC)', () => {
    // Local Jul 20 00:00 EDT = Jul 20 04:00Z; local Jul 27 23:59 EDT = Jul 28 03:59Z.
    expect(localDayRangeToUtcDays('2026-07-20', '2026-07-27')).toEqual({ start: '2026-07-20', end: '2026-07-28' });
  });

  test('handles the day DST ends (25-hour local day)', () => {
    // Nov 1 2026 ends at 23:59:59.999 EST = Nov 2 04:59Z.
    expect(localDayRangeToUtcDays('2026-11-01', '2026-11-01')).toEqual({ start: '2026-11-01', end: '2026-11-02' });
  });

  test('passes empty, missing and malformed values through unchanged', () => {
    expect(localDayRangeToUtcDays('', undefined)).toEqual({ start: '', end: undefined });
    expect(localDayRangeToUtcDays('yesterday', '2026-7-1')).toEqual({ start: 'yesterday', end: '2026-7-1' });
  });
});
