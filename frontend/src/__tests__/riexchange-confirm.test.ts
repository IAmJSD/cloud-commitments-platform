/**
 * RI exchange confirm bodies (issue #709): instance types lead, the exact
 * offering id stays visible, and the quoted hourly price change is shown.
 */
import { buildApproveConfirmBody, buildExchangeConfirmBody, hourlyPriceDelta } from '../riexchange-confirm';
import type { ExchangeQuoteSummary, RIExchangeHistoryRecord } from '../api';

const offering = '4b2293b4-5fbc-4017-9c75-d5a9d3aa8c91';

function quote(over: Partial<ExchangeQuoteSummary> = {}): ExchangeQuoteSummary {
  return {
    IsValidExchange: true,
    ValidationFailureReason: '',
    CurrencyCode: 'USD',
    PaymentDueRaw: '12.50',
    SourceHourlyPriceRaw: '0.1000',
    SourceRemainingUpfrontRaw: '',
    SourceRemainingTotalRaw: '',
    TargetHourlyPriceRaw: '0.1250',
    TargetRemainingUpfrontRaw: '',
    TargetRemainingTotalRaw: '',
    Region: 'us-west-2',
    ...over,
  };
}

function record(over: Partial<RIExchangeHistoryRecord> = {}): RIExchangeHistoryRecord {
  return {
    id: 'h1', account_id: 'a1', exchange_id: '', region: 'us-east-1',
    source_ri_ids: ['ri-1', 'ri-2'], source_instance_type: 'm5.large', source_count: 2,
    target_offering_id: offering, target_instance_type: 'm5.xlarge', target_count: 1,
    payment_due: '7.25', status: 'pending', mode: 'manual',
    created_at: '2026-10-01T00:00:00Z', updated_at: '2026-10-01T00:00:00Z',
    ...over,
  };
}

describe('hourlyPriceDelta', () => {
  it('describes an increase with the monthly estimate', () => {
    expect(hourlyPriceDelta(quote())).toBe('+0.0250 USD/hr, about +18.25 USD/month');
  });

  it('describes a decrease with a minus sign', () => {
    expect(hourlyPriceDelta(quote({ SourceHourlyPriceRaw: '0.10', TargetHourlyPriceRaw: '0.08' })))
      .toBe('-0.0200 USD/hr, about -14.60 USD/month');
  });

  it('returns null when either price is missing or not a number', () => {
    expect(hourlyPriceDelta(quote({ SourceHourlyPriceRaw: '' }))).toBeNull();
    expect(hourlyPriceDelta(quote({ TargetHourlyPriceRaw: 'n/a' }))).toBeNull();
  });
});

describe('buildExchangeConfirmBody', () => {
  it('omits the price change line when the quote has no hourly prices', () => {
    const text = buildExchangeConfirmBody({
      quote: quote({ SourceHourlyPriceRaw: '', TargetHourlyPriceRaw: '' }),
      sources: ['1 × m5.large (ri-1)'],
      targets: ['1 × m5.xlarge'],
    }).textContent ?? '';
    expect(text).toContain('Pay now: USD 12.50');
    expect(text).not.toContain('Hourly price change');
  });
});

describe('buildApproveConfirmBody', () => {
  it('leads with instance types and keeps the offering id next to the target', () => {
    const text = buildApproveConfirmBody(record()).textContent ?? '';
    expect(text).toContain('Previously quoted payment: USD 7.25');
    expect(text).toContain('Exchange: 2 × m5.large (ri-1, ri-2)');
    expect(text).toContain(`For: 1 × m5.xlarge (offering ${offering})`);
    expect(text).toContain('Region: us-east-1');
    expect(text).toContain('cannot be reversed');
  });

  it('falls back to ids when the record carries no instance types', () => {
    const text = buildApproveConfirmBody(record({ source_instance_type: '', target_instance_type: '' })).textContent ?? '';
    expect(text).toContain('Exchange: ri-1, ri-2');
    expect(text).toContain(`For: 1 × offering ${offering}`);
  });
});
