import {
  formatDecimalStringCurrency, formatIsoTermShort, formatPaymentOption, formatTermShort,
} from '../lib/table-format';

describe('table cell formatters', () => {
  test('payment slugs and AWS spellings map to one label', () => {
    expect(formatPaymentOption('all-upfront')).toBe('All Upfront');
    expect(formatPaymentOption('partial_upfront')).toBe('Partial Upfront');
    expect(formatPaymentOption('No Upfront')).toBe('No Upfront');
    expect(formatPaymentOption('monthly')).toBe('Monthly');
    expect(formatPaymentOption('weird-thing')).toBe('weird-thing');
    expect(formatPaymentOption('constructor')).toBe('constructor');
    expect(formatPaymentOption(undefined)).toBe('');
  });

  test('terms are compact', () => {
    expect(formatTermShort(1)).toBe('1 yr');
    expect(formatTermShort(3)).toBe('3 yr');
    expect(formatTermShort(null)).toBe('');
    expect(formatTermShort(Number.NaN)).toBe('');
    expect(formatIsoTermShort('P3Y')).toBe('3 yr');
    expect(formatIsoTermShort('P1M')).toBe('P1M');
    expect(formatIsoTermShort(undefined)).toBe('');
  });

  test('decimal strings from AWS become currency', () => {
    expect(formatDecimalStringCurrency('184.220000')).toBe('$184.22');
    expect(formatDecimalStringCurrency('1210.500000')).toBe('$1,210.50');
    expect(formatDecimalStringCurrency('0.000000')).toBe('$0.00');
    expect(formatDecimalStringCurrency('n/a')).toBe('n/a');
    expect(formatDecimalStringCurrency(' ')).toBe(' ');
  });
});
