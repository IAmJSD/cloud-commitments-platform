/**
 * Issue #757: a partial bulk (fan-out) purchase failure keeps the modal open
 * with per-bucket status and a retry that resubmits only the failed buckets.
 */
const showToast = jest.fn();
const closePurchaseModal = jest.fn();
const clearFanOutBuckets = jest.fn();
const openArcheraOfferModal = jest.fn();
let current: Array<Record<string, unknown>> | null = null;

jest.mock('../toast', () => ({ showToast: (...a: unknown[]) => showToast(...a) }));
jest.mock('../confirmDialog', () => ({ confirmDialog: jest.fn().mockResolvedValue(true) }));
jest.mock('../api', () => ({ executePurchase: jest.fn() }));
jest.mock('../state', () => ({}));
jest.mock('../auth', () => ({}));
jest.mock('../dashboard', () => ({ loadDashboard: jest.fn(), setupDashboardHandlers: jest.fn() }));
jest.mock('../navigation', () => ({}));
jest.mock('../plans', () => ({ closePurchaseModal: () => closePurchaseModal() }));
jest.mock('../settings', () => ({}));
jest.mock('../users', () => ({}));
jest.mock('../apikeys', () => ({}));
jest.mock('../history', () => ({}));
jest.mock('../modules/savings-history', () => ({}));
jest.mock('../riexchange', () => ({}));
jest.mock('../purchases-deeplink', () => ({}));
jest.mock('../archera', () => ({ handleArcheraDeeplink: jest.fn(), openArcheraOfferModal: (k: string) => openArcheraOfferModal(k) }));
jest.mock('../modal', () => ({ closeModal: jest.fn() }));
jest.mock('../recommendations', () => ({
  setupRecommendationsHandlers: jest.fn(),
  getPurchaseModalRecommendations: () => [],
  clearPurchaseModalRecommendations: jest.fn(),
  getFanOutBuckets: () => (current ? current.map((b) => ({ ...b })) : null),
  clearFanOutBuckets: () => { clearFanOutBuckets(); current = null; },
  retainFanOutBuckets: (ids: Set<string>) => { current = (current ?? []).filter((b) => ids.has(b['id'] as string)); },
  getExecuteMode: () => '',
  clearExecuteMode: jest.fn(),
  fanOutBucketAccountLabel: (b: { id?: string }) => `acct-${b.id}`,
  getAccountName: () => 'acct',
}));

import { handleExecutePurchase } from '../app';
import * as api from '../api';

const bucket = (id: string, service: string): Record<string, unknown> => ({
  id, provider: 'aws', service, term: 1, payment: 'no-upfront', capacityPercent: 100, paymentSource: 'toolbar',
  recs: [{ service }],
});

const section = (id: string): HTMLElement =>
  document.querySelector<HTMLElement>(`section.fanout-bucket[data-bucket-id="${id}"]`)!;
const btn = (): HTMLButtonElement => document.getElementById('execute-purchase-btn') as HTMLButtonElement;
const postedServices = (): string[] =>
  (api.executePurchase as jest.Mock).mock.calls.map((c) => (c[0] as Array<{ service: string }>)[0]!.service);

beforeEach(() => {
  jest.clearAllMocks();
  current = [bucket('bucket-0', 'ec2'), bucket('bucket-1', 'rds'), bucket('bucket-2', 'elasticache')];
  document.body.innerHTML = `
    <div id="purchase-details">
      <div id="fanout-summary"></div>
      ${current.map((b) => `<section class="fanout-bucket" data-bucket-id="${b['id']}"><h4>${b['service']}</h4><select class="fanout-bucket-payment"><option>no-upfront</option></select></section>`).join('')}
    </div>
    <button id="execute-purchase-btn"></button>`;
});

describe('fan-out partial failure retry (issue #757)', () => {
  test('keeps the modal open, marks each bucket, and narrows to the failed ones', async () => {
    (api.executePurchase as jest.Mock)
      .mockResolvedValueOnce({ execution_id: 'aaaa0000-1', status: 'pending', email_sent: true, approval_recipient: 'boss@example.com' })
      .mockRejectedValueOnce(new Error('quota exceeded'))
      .mockResolvedValueOnce({ execution_id: 'cccc0000-1', status: 'failed', email_sent: false, email_reason: 'SES throttled' });

    await handleExecutePurchase();

    expect(closePurchaseModal).not.toHaveBeenCalled();
    expect(clearFanOutBuckets).not.toHaveBeenCalled();
    expect(openArcheraOfferModal).not.toHaveBeenCalled();

    expect(section('bucket-0').classList.contains('fanout-bucket-submitted')).toBe(true);
    expect(section('bucket-0').querySelector('.fanout-bucket-result')!.textContent)
      .toBe('Submitted: awaiting approval from boss@example.com');
    expect(section('bucket-0').querySelector('select')!.disabled).toBe(true);
    expect(section('bucket-1').querySelector('.fanout-bucket-result')!.textContent).toBe('Failed: quota exceeded');
    expect(section('bucket-2').querySelector('.fanout-bucket-result')!.textContent).toBe('Failed: SES throttled');
    expect(section('bucket-1').querySelector('select')!.disabled).toBe(false);

    const banner = document.getElementById('fanout-result')!;
    expect(banner.getAttribute('role')).toBe('alert');
    expect(banner.textContent).toContain('1 bucket submitted, 2 failed.');
    expect(banner.textContent).toContain('Submitted buckets will not be sent again');

    expect(current!.map((b) => b['id'])).toEqual(['bucket-1', 'bucket-2']);
    expect(btn().textContent).toBe('Retry 2 failed');
    expect(btn().disabled).toBe(false);
    // The modal carries the details; no sticky failure toast covers it.
    expect(showToast.mock.calls.map((c) => (c[0] as { message: string }).message).join('\n')).not.toMatch(/failed/);
  });

  test('retry resubmits only the failed buckets and closes the modal once all succeed', async () => {
    (api.executePurchase as jest.Mock)
      .mockResolvedValueOnce({ execution_id: 'aaaa0000-1', status: 'pending', email_sent: true })
      .mockRejectedValueOnce(new Error('quota exceeded'))
      .mockRejectedValueOnce(new Error('timeout'));
    await handleExecutePurchase();
    expect(postedServices()).toEqual(['ec2', 'rds', 'elasticache']);

    (api.executePurchase as jest.Mock).mockClear();
    (api.executePurchase as jest.Mock)
      .mockResolvedValueOnce({ execution_id: 'bbbb0000-1', status: 'pending', email_sent: true })
      .mockResolvedValueOnce({ execution_id: 'cccc0000-1', status: 'pending', email_sent: true });
    await handleExecutePurchase();

    expect(postedServices()).toEqual(['rds', 'elasticache']);
    expect(closePurchaseModal).toHaveBeenCalledTimes(1);
    expect(openArcheraOfferModal).toHaveBeenCalledWith('purchase');
  });

  test('a second partial failure keeps the earlier successes marked and counted', async () => {
    (api.executePurchase as jest.Mock)
      .mockResolvedValueOnce({ execution_id: 'aaaa0000-1', status: 'pending', email_sent: true })
      .mockRejectedValueOnce(new Error('e1'))
      .mockRejectedValueOnce(new Error('e2'));
    await handleExecutePurchase();
    (api.executePurchase as jest.Mock)
      .mockResolvedValueOnce({ execution_id: 'bbbb0000-1', status: 'completed', duplicate: true })
      .mockRejectedValueOnce(new Error('e3'));
    await handleExecutePurchase();

    expect(section('bucket-0').classList.contains('fanout-bucket-submitted')).toBe(true);
    expect(section('bucket-1').querySelector('.fanout-bucket-result')!.textContent)
      .toBe('Submitted: joined existing request bbbb0000; it was already executed');
    expect(section('bucket-2').querySelector('.fanout-bucket-result')!.textContent).toBe('Failed: e3');
    expect(document.getElementById('fanout-result')!.textContent).toContain('2 buckets submitted, 1 failed.');
    expect(btn().textContent).toBe('Retry 1 failed');
  });

  test('all buckets failing also stays open for a retry', async () => {
    (api.executePurchase as jest.Mock).mockRejectedValue(new Error('down'));
    await handleExecutePurchase();
    expect(closePurchaseModal).not.toHaveBeenCalled();
    expect(document.getElementById('fanout-result')!.textContent).toContain('Nothing was submitted.');
    expect(btn().textContent).toBe('Retry 3 failed');
  });

  test('buckets without ids (no rendered fan-out modal) keep the old close behavior', async () => {
    current = [{ ...bucket('x', 'ec2'), id: undefined }, { ...bucket('y', 'rds'), id: undefined }];
    (api.executePurchase as jest.Mock)
      .mockResolvedValueOnce({ execution_id: 'aaaa0000-1', status: 'pending', email_sent: true })
      .mockRejectedValueOnce(new Error('boom'));
    await handleExecutePurchase();
    expect(closePurchaseModal).toHaveBeenCalledTimes(1);
    expect(document.getElementById('fanout-result')).toBeNull();
  });

  test('bucket failure reason text cannot inject markup', async () => {
    (api.executePurchase as jest.Mock)
      .mockResolvedValueOnce({ execution_id: 'aaaa0000-1', status: 'pending', email_sent: true })
      .mockRejectedValueOnce(new Error('<img src=x onerror=alert(1)>'))
      .mockResolvedValueOnce({ execution_id: 'cccc0000-1', status: 'pending', email_sent: true });
    await handleExecutePurchase();
    expect(section('bucket-1').querySelector('img')).toBeNull();
    expect(section('bucket-1').textContent).toContain('<img src=x onerror=alert(1)>');
  });
});
