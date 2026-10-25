import {
  billsMonthly, buildCommitmentBand, buildCommitmentSummary, commitmentLine, commitmentTotals, formatCommitmentTotals,
  paymentLabel, withIntro, type CommitmentLine,
} from '../commitmentSummary';

const line = (over: Partial<CommitmentLine> = {}): CommitmentLine => ({
  service: 'ec2',
  resourceType: 'm5.large',
  count: 2,
  term: 3,
  payment: 'no-upfront',
  upfront: 0,
  monthly: 100,
  ...over,
});

describe('commitmentTotals', () => {
  it('sums upfront, monthly and the term total (3 years = 36 months)', () => {
    const t = commitmentTotals([line({ upfront: 500, monthly: 100 }), line({ upfront: 0, monthly: 50, term: 1 })]);
    expect(t.upfront).toBe(500);
    expect(t.monthly).toBe(150);
    expect(t.termTotal).toBe(500 + 100 * 36 + 50 * 12);
  });

  it('is null for monthly and term total when any line has no monthly cost', () => {
    const t = commitmentTotals([line(), line({ monthly: null, upfront: 10 })]);
    expect(t.upfront).toBe(10);
    expect(t.monthly).toBeNull();
    expect(t.termTotal).toBeNull();
  });
});

describe('summary options and hints', () => {
  it('relabels the upfront stat for the approval path', () => {
    const text = buildCommitmentSummary([line({ upfront: 8400 })], { upfrontLabel: 'Upfront on approval' }).textContent;
    expect(text).toContain('Upfront on approval$8,400.00');
    expect(text).not.toContain('Charged today');
  });

  it('treats the Azure "upfront" spelling as all upfront (no monthly-fee hint)', () => {
    const text = buildCommitmentSummary([line({ monthly: null, payment: 'upfront' })]).textContent;
    expect(text).toContain('MonthlyNot reported');
    expect(text).not.toContain('fees apply');
    expect(text).toContain('All upfront');
  });

  it('hints that monthly fees apply for non all-upfront lines with no monthly figure', () => {
    expect(buildCommitmentSummary([line({ monthly: null, payment: 'partial-upfront' })]).textContent)
      .toContain('MonthlyNot reported (monthly fees apply)');
    expect(buildCommitmentSummary([line({ monthly: null, payment: 'all-upfront' })]).textContent)
      .toContain('MonthlyNot reported');
    expect(buildCommitmentSummary([line({ monthly: null, payment: 'all-upfront' })]).textContent)
      .not.toContain('fees apply');
  });
});

describe('paymentLabel', () => {
  it.each([
    ['all-upfront', 'All upfront'],
    ['partial_upfront', 'Partial upfront'],
    ['no-upfront', 'No upfront'],
    ['upfront', 'All upfront'],
    ['monthly', 'Monthly'],
    ['custom', 'custom'],
  ])('%s -> %s', (raw, label) => expect(paymentLabel(raw)).toBe(label));
});

describe('buildCommitmentSummary', () => {
  it('shows charged today, monthly and total over term', () => {
    const el = buildCommitmentSummary([line({ upfront: 1200, monthly: 10, term: 1, account: 'prod' })]);
    const text = el.textContent ?? '';
    expect(text).toContain('Charged today$1,200.00');
    expect(text).toContain('Monthly$10.00');
    expect(text).toContain('Total over term$1,320.00');
    expect(text).toContain('2 × ec2 m5.large · 1 Year, No upfront · prod');
  });

  it('says so when the monthly cost is not reported, and truncates long lists', () => {
    const el = buildCommitmentSummary(Array.from({ length: 7 }, () => line({ monthly: null })));
    expect(el.textContent).toContain('MonthlyNot reported');
    expect(el.textContent).toContain('Total over termNot available');
    expect(el.querySelectorAll('li')).toHaveLength(6);
    expect(el.textContent).toContain('and 2 more');
  });

  it('withIntro puts the sentence before the summary', () => {
    const body = withIntro('Intro.', buildCommitmentSummary([line()]));
    expect(body.firstElementChild?.textContent).toBe('Intro.');
    expect(body.querySelector('.commitment-summary')).not.toBeNull();
  });
});

describe('commitmentLine (issue #711)', () => {
  const rec = {
    service: 'rds', provider: 'aws', region: 'us-east-1', resource_type: 'db.r5.large',
    count: 2, term: 3, payment: 'partial-upfront', upfront_cost: 900, monthly_cost: 40,
  };

  it('maps a priced recommendation onto a line', () => {
    expect(commitmentLine(rec)).toEqual({
      service: 'rds', provider: 'aws', region: 'us-east-1', resourceType: 'db.r5.large',
      count: 2, term: 3, payment: 'partial-upfront', upfront: 900, monthly: 40,
    });
  });

  it('takes a bucket payment and account, and keeps an absent monthly cost as null', () => {
    const l = commitmentLine({ ...rec, monthly_cost: undefined }, { payment: 'no-upfront', account: 'prod' });
    expect(l.payment).toBe('no-upfront');
    expect(l.account).toBe('prod');
    expect(l.monthly).toBeNull();
  });
});

describe('formatCommitmentTotals and buildCommitmentBand (issue #711)', () => {
  it('formats the three totals at the requested precision', () => {
    expect(formatCommitmentTotals([line({ monthly: 100.4, term: 1 })], 0))
      .toEqual({ upfront: '$0', monthly: '$100', termTotal: '$1,205' });
  });

  it('never prints a partial monthly sum when one line is unreported', () => {
    expect(formatCommitmentTotals([line(), line({ monthly: null })]))
      .toEqual({ upfront: '$0.00', monthly: 'Not reported (monthly fees apply)', termTotal: 'Not available' });
  });

  it('billsMonthly is false only when every line is all upfront', () => {
    expect(billsMonthly([line({ payment: 'all-upfront' })])).toBe(false);
    expect(billsMonthly([line({ payment: 'all-upfront' }), line()])).toBe(true);
  });

  it('builds the band alone, without the line list', () => {
    const band = buildCommitmentBand([line({ upfront: 50, monthly: 10, term: 1 })], { upfrontLabel: 'Upfront' });
    expect(band.textContent).toBe('Upfront$50.00Monthly$10.00Total over term$170.00');
    expect(band.querySelector('li')).toBeNull();
  });
});
