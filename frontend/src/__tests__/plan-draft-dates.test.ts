import { draftDateDay, redistributeDraftDates, validateDraftDates } from '../lib/plan-draft-dates';

beforeEach(() => jest.useFakeTimers({ now: new Date('2026-10-15T23:59:00Z') }));
afterEach(() => jest.useRealTimers());

test.each(['', '2026-02-30', '2027-02-29', '2026-13-01', '2026-1-01', '0000-01-01', '10000-01-01', 'Infinity'])('rejects invalid UTC date %s', date => {
  expect(() => draftDateDay(date)).toThrow(/valid UTC/);
});

test('uses calendar days through leap years, year rollover and DST boundaries', () => {
  for (const [first, last] of [['2028-02-28', '2028-02-29'], ['2026-12-31', '2027-01-01'], ['2026-10-24', '2026-10-25']]) {
    expect(draftDateDay(last!) - draftDateDay(first!)).toBe(1);
  }
  expect(Number.isFinite(draftDateDay('0001-01-01'))).toBe(true);
  expect(Number.isFinite(draftDateDay('9999-12-31'))).toBe(true);
});

test('checks fresh UTC today and strict step order at every operation', () => {
  expect(() => validateDraftDates(['2026-10-16', '2026-10-17'])).not.toThrow();
  for (const dates of [['2026-10-15'], ['2026-10-20', '2026-10-20'], ['2026-10-21', '2026-10-20']]) {
    expect(() => validateDraftDates(dates)).toThrow();
  }
  jest.setSystemTime(new Date('2026-10-16T00:00:00Z'));
  expect(() => validateDraftDates(['2026-10-16'])).toThrow(/future/);
});

test('redistributes the same count with rounded dates, including one and one hundred steps', () => {
  const dates = ['2026-10-20', '2026-10-27', '2026-11-03'];
  expect(redistributeDraftDates(dates, '2026-10-20', '2026-10-25')).toEqual(['2026-10-20', '2026-10-23', '2026-10-25']);
  expect(dates[1]).toBe('2026-10-27');
  expect(redistributeDraftDates([dates[0]!], '2026-10-21', '2026-10-21')).toEqual(['2026-10-21']);
  const dense = redistributeDraftDates(Array.from({ length: 100 }, () => dates[0]!), '2026-10-20', '2027-01-27');
  expect(new Set(dense).size).toBe(100);
  expect(dense[99]).toBe('2027-01-27');
  expect(() => redistributeDraftDates(dense, '2026-10-20', '2027-01-26')).toThrow(/one calendar day/);
  expect(() => redistributeDraftDates(dates, '2026-10-21', '2026-10-20')).toThrow();
});
