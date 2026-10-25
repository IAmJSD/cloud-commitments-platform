/**
 * Compile-time guard for issue #363: the exchange region is required on the
 * quote response and on the execute request, so dropping it again (the gap
 * behind #238) fails `npm run typecheck` / the build instead of reaching the
 * backend's 400. If either field becomes optional again, the
 * @ts-expect-error lines below turn into "unused directive" errors.
 */
import type { ExchangeExecuteRequest, ExchangeQuoteSummary } from '../api/types';

describe('RI exchange region typing (#363)', () => {
  it('requires region on the execute request', () => {
    // @ts-expect-error -- region is required on execute
    const missing: ExchangeExecuteRequest = { ri_ids: ['ri-1'], max_payment_due_usd: '0' };
    const ok: ExchangeExecuteRequest = { ri_ids: ['ri-1'], max_payment_due_usd: '0', region: 'us-west-2' };
    expect(missing.region).toBeUndefined();
    expect(ok.region).toBe('us-west-2');
  });

  it('requires Region on the quote response', () => {
    const base = {
      IsValidExchange: true, ValidationFailureReason: '', CurrencyCode: 'USD', PaymentDueRaw: '0',
      SourceHourlyPriceRaw: '', SourceRemainingUpfrontRaw: '', SourceRemainingTotalRaw: '',
      TargetHourlyPriceRaw: '', TargetRemainingUpfrontRaw: '', TargetRemainingTotalRaw: '',
    };
    // @ts-expect-error -- Region is required on the quote
    const missing: ExchangeQuoteSummary = base;
    const ok: ExchangeQuoteSummary = { ...base, Region: 'us-west-2' };
    expect(missing.Region).toBeUndefined();
    expect(ok.Region).toBe('us-west-2');
  });
});
