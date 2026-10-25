/**
 * buildLastUpdatedStamp: the "Updated <when>" line on config rows (issue #8).
 */
import { buildLastUpdatedStamp } from '../lib/last-updated';
import { formatDateTime } from '../utils';

describe('buildLastUpdatedStamp', () => {
  test('renders a <time> with the canonical date-time text and the exact UTC instant', () => {
    const stamp = buildLastUpdatedStamp('2026-04-27T12:34:56Z');
    expect(stamp).not.toBeNull();
    expect(stamp!.tagName).toBe('TIME');
    expect(stamp!.className).toBe('last-updated-stamp');
    expect(stamp!.textContent).toBe(`Updated ${formatDateTime('2026-04-27T12:34:56Z')}`);
    expect(stamp!.dateTime).toBe('2026-04-27T12:34:56.000Z');
    expect(stamp!.title).toBe('Last updated 2026-04-27 12:34 UTC');
  });

  test('normalizes an offset timestamp to UTC in the tooltip', () => {
    const stamp = buildLastUpdatedStamp('2026-04-27T14:34:00+02:00');
    expect(stamp!.title).toBe('Last updated 2026-04-27 12:34 UTC');
  });

  test.each([
    ['undefined', undefined],
    ['null', null],
    ['empty string', ''],
    ['garbage', 'not-a-date'],
    ["Go's zero time", '0001-01-01T00:00:00Z'],
  ])('returns null for %s', (_label, input) => {
    expect(buildLastUpdatedStamp(input)).toBeNull();
  });
});
