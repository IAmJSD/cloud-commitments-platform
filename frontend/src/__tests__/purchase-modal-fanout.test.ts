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
  buildRows,
  deferred,
  flush,
  setupPurchaseModalTest,
} from './purchase-modal-harness';
import { handleExecutePurchase } from '../app';
import * as api from '../api';
import * as state from '../state';
import { openModal } from '../modal';
import {
  getPurchaseModalRecommendations,
  getFanOutBuckets,
  getExecuteMode,
  loadRecommendations,
} from '../recommendations';
import { formatCurrency } from '../utils';
import type { LocalRecommendation } from '../types';

beforeEach(setupPurchaseModalTest);

describe('Issue #1904: fan-out modal skips incompatible buckets', () => {
  function buildFanOutRows(): LocalRecommendation[] {
    return [
      {
        id: 'ec2-1', provider: 'aws', cloud_account_id: 'a1', service: 'ec2',
        region: 'us-east-1', resource_type: 'm5.large', term: 1, payment: 'no-upfront',
        count: 1, upfront_cost: 0, monthly_cost: 100, savings: 50,
      },
      {
        id: 'rds-3', provider: 'aws', cloud_account_id: 'a1', service: 'rds',
        region: 'us-east-1', resource_type: 'db.r5.large', term: 3, payment: undefined,
        count: 1, upfront_cost: 1000, savings: 200,
      },
    ];
  }

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

  test('fan-out open and close reset the execute button label and mode (#754)', async () => {
    const rows = buildFanOutRows();
    (api.getConfig as jest.Mock).mockResolvedValue({ global: { default_payment: 'no-upfront' } });
    (api.getRecommendations as jest.Mock).mockResolvedValue({ summary: {}, recommendations: rows, regions: [] });
    (state.getRecommendations as jest.Mock).mockReturnValue(rows);
    (state.getVisibleRecommendations as jest.Mock).mockReturnValue(rows);
    (state.getSelectedRecommendationIDs as jest.Mock).mockReturnValue(new Set(['ec2-1', 'rds-3']));
    await loadRecommendations();

    const btn = document.getElementById('execute-purchase-btn') as HTMLButtonElement;
    btn.textContent = 'Execute Purchase Now';
    (document.getElementById('bulk-purchase-btn') as HTMLButtonElement).click();
    await flush();
    expect(btn.textContent).toBe('Send for Approval');
    expect(getExecuteMode()).toBe('');

    btn.textContent = 'Execute Purchase Now';
    const onClose = (openModal as jest.Mock).mock.calls.slice(-1)[0][1].onClose as () => void;
    onClose();
    expect(btn.textContent).toBe('Send for Approval');
    expect(getExecuteMode()).toBe('');
  });

  test('busy fan-out stays disabled while its requests are pending', async () => {
    const requests = [
      deferred<Awaited<ReturnType<typeof api.executePurchase>>>(),
      deferred<Awaited<ReturnType<typeof api.executePurchase>>>(),
    ];
    (api.executePurchase as jest.Mock)
      .mockReturnValueOnce(requests[0]!.promise)
      .mockReturnValueOnce(requests[1]!.promise);
    const rows = buildFanOutRows().map((row) => row.service === 'rds'
      ? { ...row, payment: 'partial-upfront' as const, upfront_cost: 1000 }
      : row);
    (api.getConfig as jest.Mock).mockResolvedValue({ global: { default_payment: 'partial-upfront' } });
    (api.getRecommendations as jest.Mock).mockResolvedValue({
      summary: {}, recommendations: rows, regions: [],
    });
    (state.getRecommendations as jest.Mock).mockReturnValue(rows);
    (state.getVisibleRecommendations as jest.Mock).mockReturnValue(rows);
    (state.getSelectedRecommendationIDs as jest.Mock).mockReturnValue(new Set(['ec2-1', 'rds-3']));

    await loadRecommendations();
    (document.getElementById('bulk-purchase-btn') as HTMLButtonElement).click();
    await flush();
    const executeBtn = document.getElementById('execute-purchase-btn') as HTMLButtonElement;
    executeBtn.click();
    await flush();
    expect(api.executePurchase).toHaveBeenCalledTimes(2);

    const paymentSelect = document.querySelector<HTMLSelectElement>('.fanout-bucket-payment')!;
    paymentSelect.value = 'partial-upfront';
    paymentSelect.dispatchEvent(new Event('change'));

    expect(executeBtn.disabled).toBe(true);
    executeBtn.click();
    await flush();
    expect(api.executePurchase).toHaveBeenCalledTimes(2);

    for (const [i, request] of requests.entries()) {
      request.resolve({
        execution_id: `exec-fanout-${i}`,
        status: 'pending',
        email_sent: true,
        approval_recipient: 'approver@example.com',
      });
    }
    await flush();
    expect(executeBtn.dataset['submitting']).toBeUndefined();
  });

  test('pending fan-out bucket payment restores its value and preserves the captured request', async () => {
    const rows: LocalRecommendation[] = [
      ...buildRows(),
      {
        id: 'rds-3', provider: 'aws', cloud_account_id: 'a1', service: 'rds', region: 'us-east-1',
        resource_type: 'db.r5.large', term: 3, payment: 'all-upfront', count: 2,
        upfront_cost: 3000, monthly_cost: 0, savings: 500,
      },
    ];
    const request = deferred<Awaited<ReturnType<typeof api.executePurchase>>>();
    (api.executePurchase as jest.Mock).mockReturnValue(request.promise);
    (api.getConfig as jest.Mock).mockResolvedValue({ global: { default_payment: 'all-upfront' } });
    (api.getRecommendations as jest.Mock).mockResolvedValue({ summary: {}, recommendations: rows, regions: [] });
    (state.getRecommendations as jest.Mock).mockReturnValue(rows);
    (state.getVisibleRecommendations as jest.Mock).mockReturnValue(rows);
    (state.getSelectedRecommendationIDs as jest.Mock).mockReturnValue(new Set(['v-1-all', 'rds-3']));

    await loadRecommendations();
    (document.getElementById('bulk-purchase-btn') as HTMLButtonElement).click();
    await flush();
    const executeBtn = document.getElementById('execute-purchase-btn') as HTMLButtonElement;
    const paymentSelect = document.querySelector<HTMLSelectElement>('.fanout-bucket-payment')!;
    const beforeValue = paymentSelect.value;
    const alternate = Array.from(paymentSelect.options).find((option) => !option.disabled && option.value !== beforeValue)!;
    expect(alternate).toBeTruthy();
    executeBtn.click();
    await flush();
    expect((api.executePurchase as jest.Mock).mock.calls).toHaveLength(2);
    expect(executeBtn.dataset['submitting']).toBe('true');
    const callsBefore = (api.executePurchase as jest.Mock).mock.calls
      .map(([payload, capacity, mode]) => [JSON.parse(JSON.stringify(payload)), capacity, mode]);
    const summaryBefore = document.getElementById('fanout-summary')!.textContent;
    paymentSelect.value = alternate.value;
    expect(paymentSelect.value).not.toBe(beforeValue);
    paymentSelect.dispatchEvent(new Event('change'));
    expect(paymentSelect.value).toBe(beforeValue);
    expect(document.getElementById('fanout-summary')!.textContent).toBe(summaryBefore);
    const livePaymentSelect = document.querySelector<HTMLSelectElement>('.fanout-bucket-payment')!;
    expect(livePaymentSelect.isConnected).toBe(true);
    expect(livePaymentSelect.value).toBe(beforeValue);
    expect((api.executePurchase as jest.Mock).mock.calls).toHaveLength(2);
    expect((api.executePurchase as jest.Mock).mock.calls.map(([payload, capacity, mode]) => [JSON.parse(JSON.stringify(payload)), capacity, mode]))
      .toEqual(callsBefore);
    request.resolve({ execution_id: 'pending-bucket', status: 'pending' });
    await flush();
  });

  test('fan-out clears submitting state when result processing throws', async () => {
    const rows = buildFanOutRows().map((row) => row.service === 'rds'
      ? { ...row, payment: 'partial-upfront' as const, upfront_cost: 1000 }
      : row);
    (api.getConfig as jest.Mock).mockResolvedValue({ global: { default_payment: 'partial-upfront' } });
    (api.getRecommendations as jest.Mock).mockResolvedValue({
      summary: {}, recommendations: rows, regions: [],
    });
    (state.getRecommendations as jest.Mock).mockReturnValue(rows);
    (state.getVisibleRecommendations as jest.Mock).mockReturnValue(rows);
    (state.getSelectedRecommendationIDs as jest.Mock).mockReturnValue(new Set(['ec2-1', 'rds-3']));
    (api.executePurchase as jest.Mock)
      .mockResolvedValueOnce(null)
      .mockResolvedValueOnce({
        execution_id: 'exec-valid',
        status: 'pending',
        email_sent: true,
      });

    await loadRecommendations();
    (document.getElementById('bulk-purchase-btn') as HTMLButtonElement).click();
    await flush();
    const executeBtn = document.getElementById('execute-purchase-btn') as HTMLButtonElement;
    await expect(handleExecutePurchase()).rejects.toThrow(TypeError);

    expect(api.executePurchase).toHaveBeenCalledTimes(2);
    expect(executeBtn.dataset['submitting']).toBeUndefined();
    expect(executeBtn.disabled).toBe(false);
  });
});
