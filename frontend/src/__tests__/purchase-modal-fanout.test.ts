/**
 * Issue #1904: the fan-out purchase modal must never submit or total a
 * bucket it told the user would be skipped, and its Execute button must
 * stay consistent with what is submittable and in flight.
 *
 * These tests drive app.ts and recommendations.ts through the bulk
 * purchase button and assert on the bodies passed to api.executePurchase.
 * Payment changes inside fan-out buckets are covered separately in
 * purchase-modal-fanout-payment.test.ts. Shared mocks, fixtures and the
 * per-test reset live in purchase-modal-harness.ts.
 */

// The harness registers the module mocks, so it must be imported first.
import {
  buildFanOutRows,
  flush,
  setupPurchaseModalTest,
} from './purchase-modal-harness';
import * as api from '../api';
import * as state from '../state';
import {
  getPurchaseModalRecommendations,
  getFanOutBuckets,
  loadRecommendations,
} from '../recommendations';
import { formatCurrency } from '../utils';
import type { LocalRecommendation } from '../types';

beforeEach(setupPurchaseModalTest);

describe('Issue #1904: fan-out modal skips incompatible buckets', () => {
  test('T8 skipped bucket is not submitted and not totalled', async () => {
    const [ec2Rec, rdsRec] = buildFanOutRows();
    (api.getConfig as jest.Mock).mockResolvedValue({ global: { default_payment: 'no-upfront' } });
    (api.getRecommendations as jest.Mock).mockResolvedValue({
      summary: {}, recommendations: [ec2Rec, rdsRec], regions: [],
    });
    (state.getRecommendations as jest.Mock).mockReturnValue([ec2Rec, rdsRec]);
    (state.getVisibleRecommendations as jest.Mock).mockReturnValue([ec2Rec, rdsRec]);
    (state.getSelectedRecommendationIDs as jest.Mock).mockReturnValue(new Set(['ec2-1', 'rds-3']));

    await loadRecommendations();
    (document.getElementById('bulk-purchase-btn') as HTMLButtonElement).click();
    await flush();

    const errorSections = document.querySelectorAll('.fanout-bucket-error');
    expect(errorSections).toHaveLength(1);
    expect(errorSections[0]!.textContent).toContain('will be skipped');

    const summaryText = document.getElementById('fanout-summary')!.textContent ?? '';
    expect(summaryText).toContain('Will send 1 approval email');
    expect(summaryText).toContain('1 incompatible bucket will be skipped');

    const totalUpfrontLine = Array.from(document.querySelectorAll('#fanout-summary p'))
      .find((p) => p.textContent?.startsWith('Total upfront'))!;
    expect(totalUpfrontLine.querySelector('strong')!.textContent).toBe(formatCurrency(0));
    const totalCommitmentsLine = Array.from(document.querySelectorAll('#fanout-summary p'))
      .find((p) => p.textContent?.startsWith('Total commitments'))!;
    expect(totalCommitmentsLine.querySelector('strong')!.textContent).toBe('1');

    (document.getElementById('execute-purchase-btn') as HTMLButtonElement).click();
    await flush();

    expect(api.executePurchase).toHaveBeenCalledTimes(1);
    const body = (api.executePurchase as jest.Mock).mock.calls[0]![0] as Array<Record<string, unknown>>;
    for (const rec of body) {
      expect(rec['service']).toBe('ec2');
      expect(rec['id']).not.toBe('rds-3');
    }
  });

  test('T9 an unavailable bucket cannot be repaired by label-only mutation', async () => {
    const [ec2Rec, rdsRec] = buildFanOutRows();
    (api.getConfig as jest.Mock).mockResolvedValue({ global: { default_payment: 'no-upfront' } });
    (api.getRecommendations as jest.Mock).mockResolvedValue({
      summary: {}, recommendations: [ec2Rec, rdsRec], regions: [],
    });
    (state.getRecommendations as jest.Mock).mockReturnValue([ec2Rec, rdsRec]);
    (state.getVisibleRecommendations as jest.Mock).mockReturnValue([ec2Rec, rdsRec]);
    (state.getSelectedRecommendationIDs as jest.Mock).mockReturnValue(new Set(['ec2-1', 'rds-3']));

    await loadRecommendations();
    (document.getElementById('bulk-purchase-btn') as HTMLButtonElement).click();
    await flush();

    const rdsSection = Array.from(document.querySelectorAll<HTMLElement>('.fanout-bucket'))
      .find((s) => s.querySelector('.fanout-bucket-error') != null)!;
    const rdsPaymentSelect = rdsSection.querySelector<HTMLSelectElement>('.fanout-bucket-payment')!;
    expect(rdsPaymentSelect.disabled).toBe(true);
    rdsPaymentSelect.value = 'partial-upfront';
    rdsPaymentSelect.dispatchEvent(new Event('change'));

    expect(rdsSection.querySelector('.fanout-bucket-error')).not.toBeNull();
    const summaryText = document.getElementById('fanout-summary')!.textContent ?? '';
    expect(summaryText).toContain('Will send 1 approval email');
    expect(summaryText).toContain('1 incompatible bucket will be skipped');
    const totalUpfrontLine = Array.from(document.querySelectorAll('#fanout-summary p'))
      .find((p) => p.textContent?.startsWith('Total upfront'))!;
    expect(totalUpfrontLine.querySelector('strong')!.textContent).toBe(formatCurrency(0));

    (document.getElementById('execute-purchase-btn') as HTMLButtonElement).click();
    await flush();

    expect(api.executePurchase).toHaveBeenCalledTimes(1);
  });

  test('T10 nothing submittable disables Execute', async () => {
    const [, rdsRec] = buildFanOutRows();
    (api.getConfig as jest.Mock).mockResolvedValue({ global: { default_payment: 'no-upfront' } });
    (api.getRecommendations as jest.Mock).mockResolvedValue({
      summary: {}, recommendations: [rdsRec], regions: [],
    });
    (state.getRecommendations as jest.Mock).mockReturnValue([rdsRec]);
    (state.getVisibleRecommendations as jest.Mock).mockReturnValue([rdsRec]);
    (state.getSelectedRecommendationIDs as jest.Mock).mockReturnValue(new Set(['rds-3']));

    await loadRecommendations();
    (document.getElementById('bulk-purchase-btn') as HTMLButtonElement).click();
    await flush();

    const executeBtn = document.getElementById('execute-purchase-btn') as HTMLButtonElement;
    expect(executeBtn.disabled).toBe(true);
    expect(getFanOutBuckets()).toEqual([]);
    expect(document.getElementById('fanout-summary')!.textContent).toContain('Will send 0 approval emails');

    executeBtn.click();
    await flush();

    expect(api.executePurchase).not.toHaveBeenCalled();
  });
  // Regression for the double-scale CodeRabbit found on #2071. loadedCellVariants
  // pushes `rec` itself when the loaded list no longer holds its id, and rec is
  // already scaled, so re-scaling halved count and cost a second time. Uses a
  // count of 4 deliberately: at count 2 the second scale floors to zero units
  // and pricedCellVariant returns null, so the row is left alone and the test
  // would pass with or without the guard.
  test('T11 the fallback row is not re-scaled when the loaded list is replaced during open', async () => {
    const rec: LocalRecommendation = {
      id: 'x-1-all', provider: 'aws', cloud_account_id: 'a1', service: 'ec2',
      region: 'us-east-1', resource_type: 'm5.xlarge', count: 4, term: 1,
      payment: 'all-upfront', upfront_cost: 24000, monthly_cost: 0, savings: 1400,
    };
    (localStorage.getItem as jest.Mock).mockReturnValue(JSON.stringify({ capacity: 50 }));
    (api.getRecommendations as jest.Mock).mockResolvedValue({ summary: {}, recommendations: [rec], regions: [] });
    (state.getRecommendations as jest.Mock).mockReturnValue([rec]);
    (state.getVisibleRecommendations as jest.Mock).mockReturnValue([rec]);
    (state.getSelectedRecommendationIDs as jest.Mock).mockReturnValue(new Set(['x-1-all']));
    // A reload landing during openPurchaseModal's override fetch replaces the
    // loaded list; the override matches the rec's own payment, so the seed
    // path resolves to the fallback push, which is `rec` itself.
    (api.listAccountServiceOverrides as jest.Mock).mockImplementation(async () => {
      (state.getRecommendations as jest.Mock).mockReturnValue([]);
      return [{ id: 'ovr-1', account_id: 'a1', provider: 'aws', service: 'ec2', payment: 'all-upfront' }];
    });

    await loadRecommendations();
    (document.getElementById('bulk-purchase-btn') as HTMLButtonElement).click();
    await flush();

    expect(getPurchaseModalRecommendations()[0]).toMatchObject({
      id: 'x-1-all', count: 2, recommended_count: 4, upfront_cost: 12000,
    });

    (document.getElementById('execute-purchase-btn') as HTMLButtonElement).click();
    await flush();

    expect(api.executePurchase).toHaveBeenCalledWith(
      expect.arrayContaining([expect.objectContaining({
        id: 'x-1-all', count: 2, recommended_count: 4, upfront_cost: 12000,
      })]),
      50,
      undefined,
    );
  });
});
