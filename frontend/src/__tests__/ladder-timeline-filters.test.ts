import type { LadderEvent } from '../api/ladder';
import { emptyLadderFilters, filterLadderEvents, ladderFilterOptions, ladderTotal, ladderEditReason, ladderLabel, formatLadderAmount, formatLadderDateTime, ladderCumulative, ladderNotCountedNote } from '../ladder-timeline-filters';

const event: LadderEvent = {
  id: 'one', config_id: 'config', run_id: 'run', layer_type: 'compute-sp', term: '1yr',
  payment_option: 'no-upfront', status: 'scheduled', run_status: 'planned', revision: 0,
  amount_usd_hr: '0.123456', scheduled_date: '2030-01-01T12:00:00Z', created_at: '2026-01-01T00:00:00Z',
};

test('combined filters and cascading options preserve the original events', () => {
  const events = [event, { ...event, id: 'two', term: '3yr', payment_option: 'all-upfront' }];
  const original = JSON.stringify(events);
  const filters = { ...emptyLadderFilters(), term: '1yr', payment_option: 'no-upfront' };
  expect(filterLadderEvents(events, filters)).toEqual([event]);
  expect(ladderFilterOptions(events, filters, 'payment_option')).toEqual(['no-upfront']);
  filters.payment_option = 'all-upfront';
  expect(filterLadderEvents(events, filters)).toEqual([]);
  expect(filterLadderEvents(events, emptyLadderFilters())).toHaveLength(2);
  expect(JSON.stringify(events)).toBe(original);
});

test('six decimal totals and UTC date boundaries stay exact', () => {
  expect(ladderTotal([event, event, event])).toBe('0.370368');
  expect(ladderTotal([])).toBe('0.000000');
  expect(filterLadderEvents([event], { ...emptyLadderFilters(), from: '2030-01-01T12:00:00', to: '2030-01-01T12:00:00' })).toEqual([event]);
  expect(filterLadderEvents([event], { ...emptyLadderFilters(), from: '2030-01-01T12:00:01' })).toEqual([]);
});

test('eligibility covers status, execution, deadline, and unavailable revision API', () => {
  expect(ladderEditReason(event)).toBeUndefined();
  expect(ladderEditReason({ ...event, run_status: 'awaiting_approval' })).toContain('awaiting approval');
  expect(ladderEditReason({ ...event, status: 'completed' })).toContain('completed');
  expect(ladderEditReason({ ...event, execution_id: 'execution' })).toContain('execution');
  expect(ladderEditReason(event, Date.parse(event.scheduled_date))).toContain('passed');
  expect(ladderEditReason({ ...event, revision: undefined as unknown as number })).toContain('upgraded');
});

// FIXTURE: enum codes from cloud-commitments-go pkg/ladder.
test('ladder labels cover every known enum and show unknown codes raw', () => {
  expect(['ec2-instance-sp', 'compute-sp', 'convertible-ri', 'azure-reservation', 'azure-savings-plan'].map(v => ladderLabel('layer_type', v)))
    .toEqual(['EC2 Instance Savings Plan', 'Compute Savings Plan', 'Convertible RI', 'Azure Reservation', 'Azure Savings Plan']);
  expect(['scheduled', 'fired', 'completed', 'cancelled', 'failed'].map(v => ladderLabel('status', v)))
    .toEqual(['Scheduled', 'Fired', 'Completed', 'Cancelled', 'Failed']);
  expect(['1yr', '3yr'].map(v => ladderLabel('term', v))).toEqual(['1 Year', '3 Years']);
  expect(['no-upfront', 'partial-upfront', 'all-upfront'].map(v => ladderLabel('payment_option', v))).toEqual(['No Upfront', 'Partial Upfront', 'All Upfront']);
  expect(ladderLabel('layer_type', 'new-thing')).toBe('new-thing');
  expect(ladderLabel('term', '5yrs')).toBe('5yrs');
  expect(ladderLabel('status', '')).toBe('');
});

test('amounts keep zero and absent distinct and never round a small value to $0.00', () => {
  expect(formatLadderAmount('0.000000')).toBe('$0.00/hr');
  expect(formatLadderAmount(null)).toBe('unavailable');
  expect(formatLadderAmount('0.001200')).toBe('$0.0012/hr');
  expect(formatLadderAmount('1234.500000')).toBe('$1,234.50/hr');
  expect(() => formatLadderAmount('abc')).toThrow('Invalid planning budget.');
});

test('dates render the UTC calendar day and time regardless of browser zone', () => {
  expect(formatLadderDateTime('2030-01-02T23:30:00Z')).toBe('Jan 2, 2030, 23:30 UTC');
  expect(formatLadderDateTime('2030-01-02T23:30:00-05:00')).toBe('Jan 3, 2030, 04:30 UTC');
  expect(formatLadderDateTime('garbage')).toBe('garbage');
});

test('sub-0.0001 amounts are not shown as $0.0000 and edit reasons use readable run status (follow-ups to #697)', () => {
  expect(formatLadderAmount('0.000040')).toBe('<$0.0001/hr');
  expect(formatLadderAmount('0.000100')).toBe('$0.0001/hr');
  expect(ladderEditReason({ ...event, run_status: 'awaiting_approval' })).toBe('Run is awaiting approval.');
  expect(ladderEditReason({ ...event, status: 'cancelled' })).toBe('Tranche is cancelled.');
});

// FIXTURE: hand-written tranches.
test('ladderCumulative carries each layer forward on shared times and keeps zero-amount tranches', () => {
  const a = { ...event, id: 'a', amount_usd_hr: '0.500000', scheduled_date: '2030-01-01T00:00:00Z' };
  const b = { ...event, id: 'b', layer_type: 'ec2-instance-sp', amount_usd_hr: '1.000000', scheduled_date: '2030-02-01T00:00:00Z' };
  const c = { ...event, id: 'c', layer_type: 'ec2-instance-sp', amount_usd_hr: '0.000000', scheduled_date: '2030-02-01T00:00:00Z' };
  const result = ladderCumulative([a, b, c], 2000000n);
  expect(result.times).toEqual([Date.parse(a.scheduled_date), Date.parse(b.scheduled_date)]);
  expect(result.layers).toEqual([{ layer: 'compute-sp', units: [500000n, 500000n] }, { layer: 'ec2-instance-sp', units: [0n, 1000000n] }]);
  expect(result.totals).toEqual([2500000n, 3500000n]);
  expect([...result.rowTotals]).toEqual([['a', 2500000n], ['b', 3500000n], ['c', 3500000n]]);
});

// FIXTURE: hand-written statuses; the gate repro was scheduled 1.0 + cancelled 5.0 + failed 7.0 showing $13.00/hr instead of $1.00/hr.
test('ladderCumulative counts only scheduled, fired and completed tranches (#698 gate)', () => {
  const at = (id: string, status: string, amount: string, day: number) => ({ ...event, id, status, amount_usd_hr: amount, scheduled_date: `2030-01-0${day}T00:00:00Z` });
  const rows = [at('s', 'scheduled', '1.000000', 1), at('c', 'cancelled', '5.000000', 2), at('f', 'failed', '7.000000', 3), at('x', 'weird', '9.000000', 4), at('fi', 'fired', '2.000000', 5), at('d', 'completed', '3.000000', 6)];
  const result = ladderCumulative(rows, 0n);
  expect(result.totals).toEqual([1000000n, 3000000n, 6000000n]);
  expect([...result.rowTotals]).toEqual([['s', 1000000n], ['c', null], ['f', null], ['x', null], ['fi', 3000000n], ['d', 6000000n]]);
  expect(rows.map(ladderNotCountedNote)).toEqual([undefined, 'Not counted', 'Not counted', 'Not counted (unknown status)', undefined, undefined]);
  expect(ladderCumulative([at('c', 'cancelled', '5.000000', 1)], 0n).times).toEqual([]);
});
