/**
 * Submit toast tests: a submit that collapses onto an earlier execution
 * (duplicate=true, #758) must name the joined execution and its real state
 * instead of claiming "executed immediately" (#759).
 */
const showToast = jest.fn();
let executeMode = '';
let fanOutBuckets: unknown = null;

jest.mock('../toast', () => ({ showToast: (...a: unknown[]) => showToast(...a) }));
jest.mock('../confirmDialog', () => ({ confirmDialog: jest.fn().mockResolvedValue(true) }));
jest.mock('../api', () => ({ executePurchase: jest.fn() }));
jest.mock('../state', () => ({}));
jest.mock('../auth', () => ({}));
jest.mock('../dashboard', () => ({ loadDashboard: jest.fn(), setupDashboardHandlers: jest.fn() }));
jest.mock('../navigation', () => ({}));
jest.mock('../plans', () => ({ closePurchaseModal: jest.fn() }));
jest.mock('../settings', () => ({}));
jest.mock('../users', () => ({}));
jest.mock('../apikeys', () => ({}));
jest.mock('../history', () => ({}));
jest.mock('../modules/savings-history', () => ({}));
jest.mock('../riexchange', () => ({}));
jest.mock('../purchases-deeplink', () => ({}));
jest.mock('../archera', () => ({ handleArcheraDeeplink: jest.fn(), openArcheraOfferModal: jest.fn() }));
jest.mock('../modal', () => ({ closeModal: jest.fn() }));
jest.mock('../recommendations', () => ({
  setupRecommendationsHandlers: jest.fn(),
  getPurchaseModalRecommendations: () => [
    { service: 'ec2', provider: 'aws', region: 'us-east-1', resource_type: 'm5.large', count: 1, term: 1, payment: 'no-upfront', upfront_cost: 0 },
  ],
  clearPurchaseModalRecommendations: jest.fn(),
  getFanOutBuckets: () => fanOutBuckets,
  clearFanOutBuckets: jest.fn(),
  getExecuteMode: () => executeMode,
  clearExecuteMode: jest.fn(),
  fanOutBucketAccountLabel: () => 'acct',
  getAccountName: () => 'acct',
}));

import { handleExecutePurchase } from '../app';
import * as api from '../api';

const execId = 'abcd1234-0000-0000-0000-000000000000';
const messages = (): string[] => showToast.mock.calls.map((c) => (c[0] as { message: string }).message);
const submit = (r: Record<string, unknown>) =>
  (api.executePurchase as jest.Mock).mockResolvedValue({ execution_id: execId, ...r });

describe('submit toast', () => {
  beforeEach(() => {
    jest.clearAllMocks();
    executeMode = '';
    fanOutBuckets = null;
    document.body.innerHTML = '<button id="execute-purchase-btn"></button>';
  });

  it('direct, duplicate, pending: names the pending execution, no "executed immediately"', async () => {
    executeMode = 'direct';
    submit({ duplicate: true, status: 'pending', email_sent: true });
    await handleExecutePurchase();
    const all = messages().join('\n');
    expect(all).not.toMatch(/executed immediately/);
    expect(all).toContain('abcd1234');
    expect(all).toMatch(/existing execution/);
    expect(all).toMatch(/awaiting approval/);
  });

  it.each([
    ['completed', /already executed/],
    ['running', /being executed/],
  ])('direct, duplicate, %s: reports the real state', async (status, re) => {
    executeMode = 'direct';
    submit({ duplicate: true, status });
    await handleExecutePurchase();
    const all = messages().join('\n');
    expect(all).not.toMatch(/executed immediately/);
    expect(all).toMatch(re);
    expect(all).toContain('abcd1234');
  });

  it('approval, duplicate, pending: does not claim a new approval request was sent', async () => {
    submit({ duplicate: true, status: 'pending', email_sent: true, approval_recipient: 'a@b.c' });
    await handleExecutePurchase();
    const all = messages().join('\n');
    expect(all).toMatch(/existing execution/);
    expect(all).not.toMatch(/Approval request sent/);
  });

  it('non-duplicate direct keeps the executed-immediately message', async () => {
    executeMode = 'direct';
    submit({ status: 'completed' });
    await handleExecutePurchase();
    expect(messages().join('\n')).toMatch(/Purchase executed immediately \(id abcd1234\)/);
  });

  it('non-duplicate approval keeps the approval message', async () => {
    submit({ status: 'pending', email_sent: true, approval_recipient: 'a@b.c' });
    await handleExecutePurchase();
    expect(messages()).toContain('Approval request sent to a@b.c.');
  });

  it('fan-out: a duplicate bucket is reported as joined, not as a new approval request', async () => {
    fanOutBuckets = [
      { provider: 'aws', service: 'ec2', term: 1, payment: 'no-upfront', capacityPercent: 100, paymentSource: 'toolbar', recs: [{ service: 'ec2' }] },
      { provider: 'aws', service: 'rds', term: 1, payment: 'no-upfront', capacityPercent: 100, paymentSource: 'toolbar', recs: [{ service: 'rds' }] },
    ];
    (api.executePurchase as jest.Mock)
      .mockResolvedValueOnce({ execution_id: 'ffff0000-1', status: 'pending', email_sent: true, approval_recipient: 'a@b.c' })
      .mockResolvedValueOnce({ execution_id: execId, status: 'completed', duplicate: true });
    await handleExecutePurchase();
    const all = messages().join('\n');
    expect(all).toMatch(/1 purchase sent for approval to a@b\.c/);
    expect(all).toMatch(/existing execution abcd1234/);
    expect(all).toMatch(/already executed/);
    expect(all).not.toMatch(/2 purchases/);
  });

  it.each([
    ['pending', 'still awaiting approval'],
    ['notified', 'still awaiting approval'],
    ['approved', 'being executed'],
    ['running', 'being executed'],
    ['completed', 'already executed'],
    ['partially_completed', 'partially executed'],
    ['scheduled', 'its status is scheduled'],
    ['weird_new_status', 'its status is weird_new_status'],
  ])('duplicate wording for %s', async (status, phrase) => {
    submit({ duplicate: true, status });
    await handleExecutePurchase();
    const all = messages().join('\n');
    expect(all).toContain(phrase);
    if (phrase !== 'still awaiting approval') expect(all).not.toMatch(/awaiting approval/);
    if (status === 'partially_completed') expect(all).not.toContain('already executed');
    expect(showToast.mock.calls[0][0].kind).toBe('info');
  });

  it('fan-out partial failure: a duplicate bucket is never listed as awaiting approval', async () => {
    fanOutBuckets = [
      { provider: 'aws', service: 'ec2', term: 1, payment: 'no-upfront', capacityPercent: 100, paymentSource: 'toolbar', recs: [{ service: 'ec2' }] },
      { provider: 'aws', service: 'rds', term: 1, payment: 'no-upfront', capacityPercent: 100, paymentSource: 'toolbar', recs: [{ service: 'rds' }] },
      { provider: 'aws', service: 'elasticache', term: 1, payment: 'no-upfront', capacityPercent: 100, paymentSource: 'toolbar', recs: [{ service: 'elasticache' }] },
    ];
    (api.executePurchase as jest.Mock)
      .mockResolvedValueOnce({ execution_id: 'ffff0000-1', status: 'pending', email_sent: true })
      .mockResolvedValueOnce({ execution_id: execId, status: 'completed', duplicate: true })
      .mockRejectedValueOnce(new Error('boom'));
    await handleExecutePurchase();
    const msg = messages().find((m) => m.includes('failed'))!;
    const awaiting = msg.match(/Submitted \(awaiting approval\): ([^.]*)\./)![1];
    expect(awaiting).toContain('ec2');
    expect(awaiting).not.toContain('rds');
    expect(msg).toMatch(/rds.*abcd1234.*already executed/);
  });
});
