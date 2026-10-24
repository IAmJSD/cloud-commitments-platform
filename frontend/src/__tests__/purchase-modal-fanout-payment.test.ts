/**
 * Issue #1904: payment changes inside fan-out purchase-modal buckets must
 * swap to the priced sibling variant (not relabel the row), so the totals,
 * the per-bucket controls and the body passed to api.executePurchase all
 * describe the same variant, including saved account payment overrides,
 * capacity scaling and legacy rows with no usable payment.
 *
 * Skipped-bucket and submission-state cases are in
 * purchase-modal-fanout.test.ts. Shared mocks, fixtures and the per-test
 * reset live in purchase-modal-harness.ts.
 */

// The harness registers the module mocks, so it must be imported first.
import {
  flush,
  setupPurchaseModalTest,
} from './purchase-modal-harness';
import * as api from '../api';
import * as state from '../state';
import {
  getFanOutBuckets,
  loadRecommendations,
} from '../recommendations';
import type { LocalRecommendation } from '../types';

beforeEach(setupPurchaseModalTest);

describe('Issue #1904: fan-out modal skips incompatible buckets', () => {
  test('fan-out payment changes replace the priced sibling in the displayed totals and POST', async () => {
    const rows: LocalRecommendation[] = [
      {
        id: 'ec2-1-all', provider: 'aws', cloud_account_id: 'a1', service: 'ec2',
        region: 'us-east-1', resource_type: 'm5.large', term: 1, payment: 'all-upfront',
        count: 4, upfront_cost: 4000, monthly_cost: 0, savings: 1000,
      },
      {
        id: 'ec2-1-partial', provider: 'aws', cloud_account_id: 'a1', service: 'ec2',
        region: 'us-east-1', resource_type: 'm5.large', term: 1, payment: 'partial-upfront',
        count: 4, upfront_cost: 2000, monthly_cost: 100, savings: 900,
      },
      {
        id: 'rds-3-all', provider: 'aws', cloud_account_id: 'a1', service: 'rds',
        region: 'us-east-1', resource_type: 'db.r5.large', term: 3, payment: 'all-upfront',
        count: 2, upfront_cost: 6000, monthly_cost: 0, savings: 1000,
      },
    ];
    (api.getConfig as jest.Mock).mockResolvedValue({ global: { default_payment: 'all-upfront' } });
    (api.getRecommendations as jest.Mock).mockResolvedValue({ summary: {}, recommendations: rows, regions: [] });
    (state.getRecommendations as jest.Mock).mockReturnValue(rows);
    (state.getVisibleRecommendations as jest.Mock).mockReturnValue(rows);
    (state.getSelectedRecommendationIDs as jest.Mock).mockReturnValue(new Set(['ec2-1-all', 'rds-3-all']));

    await loadRecommendations();
    (document.getElementById('bulk-purchase-btn') as HTMLButtonElement).click();
    await flush();
    expect(document.querySelectorAll('.fanout-bucket')).toHaveLength(2);

    const ec2Section = Array.from(document.querySelectorAll<HTMLElement>('.fanout-bucket'))
      .find((section) => section.textContent?.includes('ec2'))!;
    const paymentSelect = ec2Section.querySelector<HTMLSelectElement>('.fanout-bucket-payment')!;
    paymentSelect.value = 'partial-upfront';
    paymentSelect.dispatchEvent(new Event('change'));

    const liveEc2Section = Array.from(document.querySelectorAll<HTMLElement>('.fanout-bucket'))
      .find((section) => section.textContent?.includes('ec2'))!;
    expect(liveEc2Section.querySelector('.fanout-bucket-totals')!.textContent).toContain('$2,000');
    (document.getElementById('execute-purchase-btn') as HTMLButtonElement).click();
    await flush();

    const ec2Body = (api.executePurchase as jest.Mock).mock.calls
      .map(([body]) => body as Array<Record<string, unknown>>)
      .find((body) => body.some((rec) => rec['service'] === 'ec2'))!;
    expect(ec2Body).toEqual([
      expect.objectContaining({
        id: 'ec2-1-partial', payment: 'partial-upfront', count: 4, upfront_cost: 2000, monthly_cost: 100,
      }),
    ]);
  });

  test('fan-out resolves a saved payment override to its priced sibling on open', async () => {
    const rows: LocalRecommendation[] = [
      {
        id: 'ec2-1-all', provider: 'aws', cloud_account_id: 'a1', service: 'ec2',
        region: 'us-east-1', resource_type: 'm5.large', term: 1, payment: 'all-upfront',
        count: 4, upfront_cost: 4000, monthly_cost: 0, savings: 1000,
      },
      {
        id: 'ec2-1-partial', provider: 'aws', cloud_account_id: 'a1', service: 'ec2',
        region: 'us-east-1', resource_type: 'm5.large', term: 1, payment: 'partial-upfront',
        count: 4, upfront_cost: 2000, monthly_cost: 100, savings: 900,
      },
      {
        id: 'rds-3-all', provider: 'aws', cloud_account_id: 'a1', service: 'rds',
        region: 'us-east-1', resource_type: 'db.r5.large', term: 3, payment: 'all-upfront',
        count: 2, upfront_cost: 6000, monthly_cost: 0, savings: 1000,
      },
    ];
    (api.listAccountServiceOverrides as jest.Mock).mockResolvedValue([
      { id: 'ovr-a1-ec2', account_id: 'a1', provider: 'aws', service: 'ec2', payment: 'partial-upfront' },
    ]);
    (api.getConfig as jest.Mock).mockResolvedValue({ global: { default_payment: 'all-upfront' } });
    (api.getRecommendations as jest.Mock).mockResolvedValue({ summary: {}, recommendations: rows, regions: [] });
    (state.getRecommendations as jest.Mock).mockReturnValue(rows);
    (state.getVisibleRecommendations as jest.Mock).mockReturnValue(rows);
    (state.getSelectedRecommendationIDs as jest.Mock).mockReturnValue(new Set(['ec2-1-all', 'rds-3-all']));

    await loadRecommendations();
    (document.getElementById('bulk-purchase-btn') as HTMLButtonElement).click();
    await flush();

    const ec2Section = Array.from(document.querySelectorAll<HTMLElement>('.fanout-bucket'))
      .find((section) => section.textContent?.includes('ec2'))!;
    expect(ec2Section.querySelector<HTMLSelectElement>('.fanout-bucket-payment')!.disabled).toBe(false);
    expect(ec2Section.querySelector('.fanout-bucket-totals')!.textContent).toContain('$2,000');
    expect(getFanOutBuckets()!.find((bucket) => bucket.service === 'ec2')!.recs[0]).toMatchObject({
      id: 'ec2-1-partial', payment: 'partial-upfront', upfront_cost: 2000, monthly_cost: 100,
    });

    (document.getElementById('execute-purchase-btn') as HTMLButtonElement).click();
    await flush();
    const body = (api.executePurchase as jest.Mock).mock.calls
      .map(([payload]) => payload as Array<Record<string, unknown>>)
      .find((payload) => payload.some((rec) => rec['service'] === 'ec2'))!;
    expect(body).toEqual([expect.objectContaining({
      id: 'ec2-1-partial', payment: 'partial-upfront', upfront_cost: 2000, monthly_cost: 100,
    })]);
  });

  test('fan-out falls from an unpriced saved override to the row payment before toolbar default', async () => {
    const rows: LocalRecommendation[] = [
      {
        id: 'ec2-1-partial', provider: 'aws', cloud_account_id: 'a1', service: 'ec2',
        region: 'us-east-1', resource_type: 'm5.large', term: 1, payment: 'partial-upfront',
        count: 2, upfront_cost: 1000, monthly_cost: 50, savings: 400,
      },
      {
        id: 'ec2-1-all', provider: 'aws', cloud_account_id: 'a1', service: 'ec2',
        region: 'us-east-1', resource_type: 'm5.large', term: 1, payment: 'all-upfront',
        count: 2, upfront_cost: 2000, monthly_cost: 0, savings: 500,
      },
      {
        id: 'rds-3-all', provider: 'aws', cloud_account_id: 'a1', service: 'rds',
        region: 'us-east-1', resource_type: 'db.r5.large', term: 3, payment: 'all-upfront',
        count: 1, upfront_cost: 3000, monthly_cost: 0, savings: 500,
      },
    ];
    (api.listAccountServiceOverrides as jest.Mock).mockResolvedValue([
      { id: 'ovr-a1-ec2', account_id: 'a1', provider: 'aws', service: 'ec2', payment: 'no-upfront' },
    ]);
    (api.getConfig as jest.Mock).mockResolvedValue({ global: { default_payment: 'all-upfront' } });
    (api.getRecommendations as jest.Mock).mockResolvedValue({ summary: {}, recommendations: rows, regions: [] });
    (state.getRecommendations as jest.Mock).mockReturnValue(rows);
    (state.getVisibleRecommendations as jest.Mock).mockReturnValue(rows);
    (state.getSelectedRecommendationIDs as jest.Mock).mockReturnValue(new Set(['ec2-1-partial', 'rds-3-all']));

    await loadRecommendations();
    (document.getElementById('bulk-purchase-btn') as HTMLButtonElement).click();
    await flush();

    const ec2Section = Array.from(document.querySelectorAll<HTMLElement>('.fanout-bucket'))
      .find((section) => section.textContent?.includes('ec2'))!;
    expect(ec2Section.querySelector('.fanout-bucket-totals')!.textContent).toContain('$1,000');
    expect(getFanOutBuckets()!.find((bucket) => bucket.service === 'ec2')!.recs[0]).toMatchObject({
      id: 'ec2-1-partial', payment: 'partial-upfront', upfront_cost: 1000, monthly_cost: 50,
    });
  });

  test('fan-out initialization keeps sibling IDs and capacity scaling at 50 percent', async () => {
    const rows: LocalRecommendation[] = [
      {
        id: 'ec2-1-all', provider: 'aws', cloud_account_id: 'a1', service: 'ec2',
        region: 'us-east-1', resource_type: 'm5.large', term: 1, payment: 'all-upfront',
        count: 4, upfront_cost: 4000, monthly_cost: 0, savings: 1000,
      },
      {
        id: 'ec2-1-partial', provider: 'aws', cloud_account_id: 'a1', service: 'ec2',
        region: 'us-east-1', resource_type: 'm5.large', term: 1, payment: 'partial-upfront',
        count: 4, upfront_cost: 2000, monthly_cost: 100, savings: 900,
      },
      {
        id: 'rds-3-all', provider: 'aws', cloud_account_id: 'a1', service: 'rds',
        region: 'us-east-1', resource_type: 'db.r5.large', term: 3, payment: 'all-upfront',
        count: 2, upfront_cost: 6000, monthly_cost: 0, savings: 1000,
      },
    ];
    (localStorage.getItem as jest.Mock).mockReturnValue(JSON.stringify({ capacity: 50 }));
    (api.listAccountServiceOverrides as jest.Mock).mockResolvedValue([
      { id: 'ovr-a1-ec2', account_id: 'a1', provider: 'aws', service: 'ec2', payment: 'partial-upfront' },
    ]);
    (api.getConfig as jest.Mock).mockResolvedValue({ global: { default_payment: 'all-upfront' } });
    (api.getRecommendations as jest.Mock).mockResolvedValue({ summary: {}, recommendations: rows, regions: [] });
    (state.getRecommendations as jest.Mock).mockReturnValue(rows);
    (state.getVisibleRecommendations as jest.Mock).mockReturnValue(rows);
    (state.getSelectedRecommendationIDs as jest.Mock).mockReturnValue(new Set(['ec2-1-all', 'rds-3-all']));

    await loadRecommendations();
    (document.getElementById('bulk-purchase-btn') as HTMLButtonElement).click();
    await flush();

    const ec2Bucket = getFanOutBuckets()!.find((bucket) => bucket.service === 'ec2')!;
    expect(ec2Bucket.recs[0]).toMatchObject({
      id: 'ec2-1-partial', payment: 'partial-upfront', count: 2, recommended_count: 4,
      upfront_cost: 1000, monthly_cost: 50,
    });
    (document.getElementById('execute-purchase-btn') as HTMLButtonElement).click();
    await flush();
    const body = (api.executePurchase as jest.Mock).mock.calls
      .map(([payload]) => payload as Array<Record<string, unknown>>)
      .find((payload) => payload.some((rec) => rec['service'] === 'ec2'))!;
    expect(body).toEqual([expect.objectContaining({
      id: 'ec2-1-partial', count: 2, recommended_count: 4, upfront_cost: 1000, monthly_cost: 50,
    })]);
  });

  test('fan-out resolves a viable legacy sibling instead of repairing an unpriced label', async () => {
    const rows: LocalRecommendation[] = [
      {
        id: 'ec2-1-all', provider: 'aws', cloud_account_id: 'a1', service: 'ec2',
        region: 'us-east-1', resource_type: 'm5.large', term: 1, payment: 'all-upfront',
        count: 2, upfront_cost: 2000, monthly_cost: 0, savings: 500,
      },
      {
        id: 'rds-3-no', provider: 'aws', cloud_account_id: 'a1', service: 'rds',
        region: 'us-east-1', resource_type: 'db.r5.large', term: 3, payment: 'no-upfront',
        count: 2, upfront_cost: 0, monthly_cost: 800, savings: 500,
      },
      {
        id: 'rds-3-all', provider: 'aws', cloud_account_id: 'a1', service: 'rds',
        region: 'us-east-1', resource_type: 'db.r5.large', term: 3, payment: 'all-upfront',
        count: 2, upfront_cost: 6000, monthly_cost: 0, savings: 1000,
      },
    ];
    (api.getConfig as jest.Mock).mockResolvedValue({ global: { default_payment: 'no-upfront' } });
    (api.getRecommendations as jest.Mock).mockResolvedValue({ summary: {}, recommendations: rows, regions: [] });
    (state.getRecommendations as jest.Mock).mockReturnValue(rows);
    (state.getVisibleRecommendations as jest.Mock).mockReturnValue(rows);
    (state.getSelectedRecommendationIDs as jest.Mock).mockReturnValue(new Set(['ec2-1-all', 'rds-3-no']));

    await loadRecommendations();
    (document.getElementById('bulk-purchase-btn') as HTMLButtonElement).click();
    await flush();

    const rdsSection = Array.from(document.querySelectorAll<HTMLElement>('.fanout-bucket'))
      .find((section) => section.textContent?.includes('rds'))!;
    expect(rdsSection.querySelector('.fanout-bucket-totals')!.textContent).toContain('$6,000');
    expect(getFanOutBuckets()!.find((bucket) => bucket.service === 'rds')!.recs[0]).toMatchObject({
      id: 'rds-3-all', payment: 'all-upfront', upfront_cost: 6000, monthly_cost: 0,
    });
  });


  test('fan-out payment changes keep capacity scaling and recommended_count on the sibling', async () => {
    const rows: LocalRecommendation[] = [
      {
        id: 'ec2-1-all', provider: 'aws', cloud_account_id: 'a1', service: 'ec2',
        region: 'us-east-1', resource_type: 'm5.large', term: 1, payment: 'all-upfront',
        count: 4, upfront_cost: 4000, monthly_cost: 0, savings: 1000,
      },
      {
        id: 'ec2-1-partial', provider: 'aws', cloud_account_id: 'a1', service: 'ec2',
        region: 'us-east-1', resource_type: 'm5.large', term: 1, payment: 'partial-upfront',
        count: 4, upfront_cost: 2000, monthly_cost: 100, savings: 900,
      },
      {
        id: 'rds-3-all', provider: 'aws', cloud_account_id: 'a1', service: 'rds',
        region: 'us-east-1', resource_type: 'db.r5.large', term: 3, payment: 'all-upfront',
        count: 2, upfront_cost: 6000, monthly_cost: 0, savings: 1000,
      },
    ];
    (localStorage.getItem as jest.Mock).mockReturnValue(JSON.stringify({ capacity: 50 }));
    (api.getConfig as jest.Mock).mockResolvedValue({ global: { default_payment: 'all-upfront' } });
    (api.getRecommendations as jest.Mock).mockResolvedValue({ summary: {}, recommendations: rows, regions: [] });
    (state.getRecommendations as jest.Mock).mockReturnValue(rows);
    (state.getVisibleRecommendations as jest.Mock).mockReturnValue(rows);
    (state.getSelectedRecommendationIDs as jest.Mock).mockReturnValue(new Set(['ec2-1-all', 'rds-3-all']));

    await loadRecommendations();
    (document.getElementById('bulk-purchase-btn') as HTMLButtonElement).click();
    await flush();
    expect(document.querySelectorAll('.fanout-bucket')).toHaveLength(2);

    const ec2Section = Array.from(document.querySelectorAll<HTMLElement>('.fanout-bucket'))
      .find((section) => section.textContent?.includes('ec2'))!;
    const paymentSelect = ec2Section.querySelector<HTMLSelectElement>('.fanout-bucket-payment')!;
    paymentSelect.value = 'partial-upfront';
    paymentSelect.dispatchEvent(new Event('change'));
    (document.getElementById('execute-purchase-btn') as HTMLButtonElement).click();
    await flush();

    const ec2Body = (api.executePurchase as jest.Mock).mock.calls
      .map(([body]) => body as Array<Record<string, unknown>>)
      .find((body) => body.some((rec) => rec['service'] === 'ec2'))!;
    expect(ec2Body).toEqual([
      expect.objectContaining({
        id: 'ec2-1-partial', payment: 'partial-upfront', count: 2, recommended_count: 4,
        upfront_cost: 1000, monthly_cost: 50,
      }),
    ]);
  });
});
