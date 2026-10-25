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
  renderModal(current);
});

/** Renders the open purchase modal with one section per bucket. */
function renderModal(buckets: Array<Record<string, unknown>>): void {
  document.body.innerHTML = `
    <div id="purchase-modal" class="modal">
      <div id="purchase-details">
        <div id="fanout-summary"></div>
        ${buckets.map((b) => `<section class="fanout-bucket" data-bucket-id="${b['id']}"><h4>${b['service']}</h4><select class="fanout-bucket-payment"><option>no-upfront</option></select></section>`).join('')}
      </div>
      <button id="execute-purchase-btn"></button>
    </div>`;
}

/** What Escape/Cancel does: hide the modal and clear its buckets (onClose). */
function closeModalMidSubmit(): void {
  document.getElementById('purchase-modal')!.classList.add('hidden');
  current = null;
}

/** executePurchase calls that stay pending until the test settles them. */
function deferredPosts(): Array<{ resolve: (v: unknown) => void; reject: (e: Error) => void }> {
  const posts: Array<{ resolve: (v: unknown) => void; reject: (e: Error) => void }> = [];
  (api.executePurchase as jest.Mock).mockImplementation(() => new Promise((resolve, reject) => { posts.push({ resolve, reject }); }));
  return posts;
}

const flush = async (): Promise<void> => { for (let i = 0; i < 10; i++) await Promise.resolve(); };
const toastMessages = (): string[] => showToast.mock.calls.map((c) => (c[0] as { message: string }).message);

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

  test('closing the modal mid-submit still reports a failed bucket in a toast, as before', async () => {
    const posts = deferredPosts();
    const run = handleExecutePurchase();
    await flush();
    expect(posts).toHaveLength(3);

    closeModalMidSubmit();
    posts[0]!.resolve({ execution_id: 'aaaa0000-1', status: 'pending', email_sent: true });
    posts[1]!.reject(new Error('quota exceeded'));
    posts[2]!.resolve({ execution_id: 'cccc0000-1', status: 'pending', email_sent: true });
    await run;

    expect(toastMessages()).toContainEqual(expect.stringMatching(/^2 of 3 submitted · 1 failed: acct-bucket-1: quota exceeded/));
    expect(document.getElementById('fanout-result')).toBeNull();
    expect(document.querySelector('.fanout-bucket-result')).toBeNull();
    expect(openArcheraOfferModal).toHaveBeenCalledWith('purchase');
  });

  test('results from a closed modal leave a newly opened bulk modal untouched', async () => {
    const posts = deferredPosts();
    const run = handleExecutePurchase();
    await flush();

    closeModalMidSubmit();
    const next = [bucket('next-0', 'ec2'), bucket('next-1', 'rds')];
    current = next;
    renderModal(next);

    posts[0]!.resolve({ execution_id: 'aaaa0000-1', status: 'pending', email_sent: true });
    posts[1]!.reject(new Error('quota exceeded'));
    posts[2]!.resolve({ execution_id: 'cccc0000-1', status: 'pending', email_sent: true });
    await run;

    expect(current!.map((b) => b['id'])).toEqual(['next-0', 'next-1']);
    expect(document.getElementById('purchase-modal')!.classList.contains('hidden')).toBe(false);
    expect(document.getElementById('fanout-result')).toBeNull();
    expect(document.querySelector('.fanout-bucket-submitted, .fanout-bucket-failed, .fanout-bucket-result')).toBeNull();
    expect([...document.querySelectorAll('select')].some((s) => (s as HTMLSelectElement).disabled)).toBe(false);
    expect(closePurchaseModal).not.toHaveBeenCalled();
    expect(clearFanOutBuckets).not.toHaveBeenCalled();
    expect(toastMessages()).toContainEqual(expect.stringMatching(/^2 of 3 submitted · 1 failed: acct-bucket-1: quota exceeded/));
  });

  test('the payment-adjustment notice is not sticky while the modal is in retry mode', async () => {
    (api.executePurchase as jest.Mock)
      .mockResolvedValueOnce({ execution_id: 'aaaa0000-1', status: 'pending', email_sent: true, payment_adjustments: [{ provider: 'aws', service: 'ec2', requested_payment_option: 'no-upfront', applied_payment_option: 'all-upfront' }] })
      .mockRejectedValueOnce(new Error('quota exceeded'))
      .mockRejectedValueOnce(new Error('timeout'));
    await handleExecutePurchase();

    expect(document.getElementById('fanout-result')).not.toBeNull();
    const notice = showToast.mock.calls.map((c) => c[0] as { kind: string; timeout?: number | null }).find((o) => o.kind === 'warning');
    expect(notice).toBeDefined();
    expect(notice!.timeout).not.toBeNull();
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
