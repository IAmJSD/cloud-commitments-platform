import { buildLocalScheduleIntent, PlanScheduleIntentInput } from '../plan-schedule-intent';
import { suggestedPurchaseStartDate } from '../lib/calendar-date';

const draft: PlanScheduleIntentInput = { name: 'Compute ramp', preset: 'weekly-25pct', coverage: '80', stepPercent: '25', intervalDays: '7', notificationDays: '3', term: '1', payment: 'monthly' };
beforeEach(() => { jest.useFakeTimers({ now: new Date('2026-10-15T23:59:00Z') }); });
afterEach(() => { jest.useRealTimers(); });

test('weekly 25% builds four weekly steps from the suggested start', () => {
  const intent = buildLocalScheduleIntent(draft);
  expect(intent.dates).toEqual(['2026-10-20', '2026-10-27', '2026-11-03', '2026-11-10']);
  expect(intent.shares).toEqual([25, 50, 75, 100]);
  expect(intent.coverage).toBe(80);
});

test('preserves legacy custom 33% shares (3 steps, last share 99)', () => {
  const intent = buildLocalScheduleIntent({ ...draft, preset: 'custom', stepPercent: '33' });
  expect(intent.dates).toHaveLength(3);
  expect(intent.shares).toEqual([33, 66, 99]);
  expect(intent.coverage * intent.shares[2]! / 100).toBeCloseTo(79.2);
});

test('preset wins over hidden custom fields', () => {
  expect(buildLocalScheduleIntent({ ...draft, stepPercent: '1', intervalDays: '1' }).dates).toHaveLength(4);
});

test.each([
  { coverage: '' }, { coverage: '101' }, { coverage: '1.5' }, { notificationDays: '0' }, { notificationDays: 'Infinity' },
  { preset: 'toString' }, { preset: '' }, { preset: 'custom', stepPercent: '0' }, { preset: 'custom', intervalDays: '366' },
])('rejects invalid input %j', change => {
  expect(() => buildLocalScheduleIntent({ ...draft, ...change })).toThrow();
});

test('uses UTC calendar dates across a year boundary', () => {
  jest.setSystemTime(new Date('2026-12-31T23:59:00Z'));
  expect(suggestedPurchaseStartDate(3)).toBe('2027-01-05');
  expect(buildLocalScheduleIntent(draft).dates[0]).toBe('2027-01-05');
});
