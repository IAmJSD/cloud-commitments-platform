/**
 * Unit tests for the shared chart helpers in modules/chart-utils.ts.
 */
import { formatSparseTrendMessage, MIN_TREND_POINTS } from '../modules/chart-utils';

describe('formatSparseTrendMessage (issue #188)', () => {
  test('names the single value and its date, with the year', () => {
    // Midday UTC so the calendar day is the same in every timezone jest runs in.
    expect(formatSparseTrendMessage({ timestamp: '2026-10-20T12:00:00Z', cumulative_savings: 1.1 }))
      .toBe('Not enough history yet to show a trend. So far: $1.10 in cumulative savings as of Oct 20, 2026.');
  });

  test('formats large values with thousands separators and cents', () => {
    expect(formatSparseTrendMessage({ timestamp: '2025-01-05T12:00:00Z', cumulative_savings: 12345.678 }))
      .toContain('$12,345.68 in cumulative savings as of Jan 5, 2025');
  });

  test('treats a missing value as $0.00 rather than printing NaN', () => {
    expect(formatSparseTrendMessage({ timestamp: '2025-01-05T12:00:00Z', cumulative_savings: undefined as unknown as number }))
      .toContain('So far: $0.00');
  });

  test('a trend needs at least two points', () => {
    expect(MIN_TREND_POINTS).toBe(2);
  });
});
