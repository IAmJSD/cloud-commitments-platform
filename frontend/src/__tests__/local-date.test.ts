/**
 * Local-calendar date input helpers (issue #51). Jest runs in
 * America/New_York (globalSetup), so the UTC and local days differ for the
 * evening instants used here. The jest sandbox cannot change its own zone,
 * so the multi-zone range checks below run the module in a child `node`
 * with TZ set; the browser flow runs in Playwright under Pacific/Auckland
 * and America/Los_Angeles: tests-e2e/history-local-dates.spec.ts.
 */
import { execFileSync } from 'child_process';
import { readFileSync } from 'fs';
import { join } from 'path';
import * as ts from 'typescript';
import {
  MAX_HISTORY_RANGE_DAYS,
  localDayRangeToUtcDays,
  parseLocalDateInputValue,
  toLocalDateInputValue,
} from '../lib/local-date';

type Range = { start: string | undefined; end: string | undefined };

/** parseHistoryDateRange in internal/api/handler_history.go, both sides set. */
function serverAccepts({ start, end }: Range): boolean {
  const day = 86_400_000;
  const from = Date.parse(`${start}T00:00:00Z`);
  const to = Date.parse(`${end}T00:00:00Z`) + day - 1000; // end of the To day
  return from <= to && to - from <= MAX_HISTORY_RANGE_DAYS * day;
}

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

  test('keeps a one-year range within the server cap (west of UTC)', () => {
    // 2025-10-24..2026-10-24 is the server's 366-day maximum as typed.
    // Widening the To side to Oct 25 UTC would make it 367 days and get
    // "date range too large", so From gives the extra day back instead.
    const range = localDayRangeToUtcDays('2025-10-24', '2026-10-24');
    expect(range).toEqual({ start: '2025-10-25', end: '2026-10-25' });
    expect(serverAccepts(range)).toBe(true);
  });

  test('passes an over-cap or inverted range through for the server to reject', () => {
    expect(localDayRangeToUtcDays('2025-10-23', '2026-10-24')).toEqual({ start: '2025-10-23', end: '2026-10-24' });
    expect(localDayRangeToUtcDays('2026-07-29', '2026-07-28')).toEqual({ start: '2026-07-29', end: '2026-07-28' });
  });
});

describe('localDayRangeToUtcDays in other zones', () => {
  type Result = Range & { startCovered: boolean; endCovered: boolean };

  // Transpile the module and run it in a child node with TZ set. For each
  // case it reports the request and whether that request's UTC days cover
  // the first and last instant of the user's local From/To days.
  function runInZone(zone: string, cases: Array<[string, string]>): { offset: number; results: Result[] } {
    const source = ts.transpileModule(readFileSync(join(__dirname, '../lib/local-date.ts'), 'utf8'), {
      compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2020 },
    }).outputText;
    const script = `
      const m = { exports: {} };
      (function (module, exports) { ${source} })(m, m.exports);
      const { localDayRangeToUtcDays, parseLocalDateInputValue } = m.exports;
      const results = JSON.parse(process.argv[1]).map(([start, end]) => {
        const r = localDayRangeToUtcDays(start, end);
        const e = parseLocalDateInputValue(end);
        const lastInstant = new Date(e.getFullYear(), e.getMonth(), e.getDate() + 1).getTime() - 1;
        return {
          ...r,
          startCovered: Date.parse(r.start + 'T00:00:00Z') <= parseLocalDateInputValue(start).getTime(),
          endCovered: Date.parse(r.end + 'T23:59:59.999Z') >= lastInstant,
        };
      });
      process.stdout.write(JSON.stringify({ offset: new Date(2026, 0, 15).getTimezoneOffset(), results }));`;
    const out = execFileSync(process.execPath, ['-e', script, JSON.stringify(cases)], {
      env: { ...process.env, TZ: zone },
      encoding: 'utf8',
    });
    return JSON.parse(out) as { offset: number; results: Result[] };
  }

  const ONE_YEAR: [string, string] = ['2025-10-24', '2026-10-24']; // non-leap, 366 days counting both ends
  const LEAP_YEAR: [string, string] = ['2028-01-01', '2028-12-31']; // 366 days, includes Feb 29
  const OVER_CAP: [string, string] = ['2027-10-24', '2028-10-24']; // includes Feb 29: 367 days
  const UNDER_CAP: [string, string] = ['2026-01-01', '2026-12-31']; // 365 days: room to widen

  // [zone, January UTC offset in minutes, expected ONE_YEAR, LEAP_YEAR, UNDER_CAP requests]
  const zones: Array<[string, number, Range, Range, Range]> = [
    ['America/Los_Angeles', 480,
      { start: '2025-10-25', end: '2026-10-25' },
      { start: '2028-01-02', end: '2029-01-01' },
      { start: '2026-01-01', end: '2027-01-01' }],
    ['Pacific/Auckland', -780,
      { start: '2025-10-24', end: '2026-10-24' },
      { start: '2028-01-01', end: '2028-12-31' },
      { start: '2025-12-31', end: '2026-12-31' }],
    ['UTC', 0,
      { start: '2025-10-24', end: '2026-10-24' },
      { start: '2028-01-01', end: '2028-12-31' },
      { start: '2026-01-01', end: '2026-12-31' }],
  ];

  test.each(zones)('%s: one-year, leap-year and cap-boundary ranges', (zone, offset, oneYear, leapYear, underCap) => {
    const run = runInZone(zone, [ONE_YEAR, LEAP_YEAR, OVER_CAP, UNDER_CAP]);
    expect(run.offset).toBe(offset); // the child really ran in this zone
    const [one, leap, over, under] = run.results as [Result, Result, Result, Result];

    for (const [got, want] of [[one, oneYear], [leap, leapYear], [under, underCap]] as Array<[Result, Range]>) {
      expect({ start: got.start, end: got.end }).toEqual(want);
      expect(serverAccepts(got)).toBe(true);
      expect(got.endCovered).toBe(true); // the user's whole To day is requested
    }
    // Below the cap the full widening fits, so the whole From day is requested too.
    expect(under.startCovered).toBe(true);
    // Over the cap: sent exactly as typed, rejected exactly as before #51.
    expect({ start: over.start, end: over.end }).toEqual({ start: OVER_CAP[0], end: OVER_CAP[1] });
    expect(serverAccepts(over)).toBe(false);
  });

  test.each(zones.map(([zone]) => zone))('%s: every range up to the cap is accepted and keeps its To day', zone => {
    // To days around DST changes in both hemispheres, Feb 29, and today.
    const ends = ['2026-03-08', '2026-04-05', '2026-09-27', '2026-11-01', '2028-02-29', '2028-03-01', '2026-10-24'];
    const gaps = [0, 7, 364, MAX_HISTORY_RANGE_DAYS - 1];
    const cases = ends.flatMap(end => gaps.map((gap): [string, string] => {
      const e = Date.parse(`${end}T00:00:00Z`);
      return [new Date(e - gap * 86_400_000).toISOString().slice(0, 10), end];
    }));
    const { results } = runInZone(zone, cases);
    results.forEach((got, i) => {
      const accepted = serverAccepts(got);
      const covered = got.endCovered;
      expect({ range: cases[i], accepted, covered }).toEqual({ range: cases[i], accepted: true, covered: true });
    });
  });
});
