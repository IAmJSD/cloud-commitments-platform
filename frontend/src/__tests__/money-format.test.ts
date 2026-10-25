/**
 * Issue #739: money goes through one formatter. The period-scaled cost
 * cells used to print `$${x.toFixed(n)}`, so a yearly figure read
 * "$123456" with no separators, and absent values used three different
 * placeholders.
 */
import { formatCostForPeriod } from '../recommendations';
import { formatCurrency, NO_VALUE } from '../utils';

describe('money formatting (issue #739)', () => {
  it('uses one em-dash placeholder for absent values', () => {
    expect(NO_VALUE).toBe('—');
    expect(formatCurrency(null)).toBe(NO_VALUE);
    expect(formatCostForPeriod(null, 'yearly')).toBe(NO_VALUE);
  });

  it('groups thousands in every cost period', () => {
    expect(formatCostForPeriod(10288, 'yearly')).toBe('$123,456');
    expect(formatCostForPeriod(100000, 'daily')).toBe('$3,333.33');
    expect(formatCostForPeriod(2000000, 'hourly')).toBe('$2,777.7778');
    expect(formatCostForPeriod(1234.4, 'monthly')).toBe('$1,234');
  });
});
