/**
 * Deep-link parser and approve-handler tests. The parser decides whether a
 * given URL *is* a deep-link; the handler tests drive the real toast and
 * confirm dialog with only the network layer mocked.
 */

jest.mock('../api', () => ({
  getPurchaseDetails: jest.fn(),
  getApprovalDetails: jest.fn(),
  listAccounts: jest.fn().mockResolvedValue([]),
  getDeploymentInfo: jest.fn().mockResolvedValue({}),
}));
jest.mock('../api/client', () => ({ apiRequest: jest.fn() }));

import * as api from '../api';
import { apiRequest } from '../api/client';
import { handlePurchaseDeeplink, parsePurchaseDeeplink } from '../purchases-deeplink';

const EXEC_ID = '11111111-1111-1111-1111-111111111111';

describe('handlePurchaseDeeplink approve (issue #247)', () => {
  beforeEach(() => {
    document.body.innerHTML = '';
    jest.clearAllMocks();
    (apiRequest as jest.Mock).mockResolvedValue({ status: 'approved' });
    window.history.replaceState({}, '', `/purchases/approve/${EXEC_ID}?token=tok-xyz`);
    jest.spyOn(console, 'error').mockImplementation(() => undefined);
  });

  afterEach(() => jest.restoreAllMocks());

  it.each([
    ['the details fetch fails', () => (api.getApprovalDetails as jest.Mock).mockRejectedValue(new Error('403 Forbidden'))],
    ['the details carry no recommendations', () => (api.getApprovalDetails as jest.Mock).mockResolvedValue({
      execution_id: EXEC_ID, status: 'pending', total_upfront_cost: 0, estimated_savings: 0, recommendations: [],
    })],
  ])('does not offer Approve when %s', async (_label, arrange) => {
    arrange();

    await expect(handlePurchaseDeeplink()).resolves.toBe(true);

    expect(document.querySelector('.modal-confirm-backdrop')).toBeNull();
    const buttons = Array.from(document.querySelectorAll('button')).map(b => b.textContent);
    expect(buttons).not.toContain('Approve purchase');
    const alert = document.querySelector('[role="alert"]');
    expect(alert?.textContent).toContain('cannot be approved without showing the amount');
    expect(apiRequest).not.toHaveBeenCalled();
    expect(window.location.pathname).toBe('/purchases');
  });

  it.each([
    [403, 'forbidden', 'view:purchases'],
    [404, 'execution not found', 'not found or is outside your account access'],
    [410, 'approval token has expired', 'The approval link has expired.'],
  ])('details fetch failing with %s is permanent: no retry advice, keeps server message', async (status, message, wording) => {
    (api.getApprovalDetails as jest.Mock).mockRejectedValue(Object.assign(new Error(message), { status }));

    await expect(handlePurchaseDeeplink()).resolves.toBe(true);

    const text = document.querySelector('[role="alert"]')?.textContent ?? '';
    expect(text).toContain(wording);
    expect(text).toContain(`(${message})`);
    expect(text).not.toContain('retry');
  });

  it.each([
    [500, 'internal error'],
    [401, 'unauthorized'],
    [undefined, 'Failed to fetch'],
  ])('details fetch failing with %s keeps the retry path', async (status, message) => {
    (api.getApprovalDetails as jest.Mock).mockRejectedValue(Object.assign(new Error(message), { status }));

    await expect(handlePurchaseDeeplink()).resolves.toBe(true);

    const text = document.querySelector('[role="alert"]')?.textContent ?? '';
    expect(text).toContain('Open the approval link again to retry.');
    expect(text).toContain(`(${message})`);
    expect(text).not.toContain('view:purchases');
  });

  it('shows the upfront amount and approves when details load', async () => {
    (api.getApprovalDetails as jest.Mock).mockResolvedValue({
      execution_id: EXEC_ID, status: 'pending', total_upfront_cost: 1200, estimated_savings: 10,
      recommendations: [{ id: 'r1', provider: 'aws', service: 'ec2', region: 'us-east-1', resource_type: 'm5.large',
        count: 1, term: 1, payment: 'all-upfront', upfront_cost: 1200, monthly_cost: 0, savings: 10, selected: true, purchased: false }],
    });

    const done = handlePurchaseDeeplink();
    await new Promise(resolve => setTimeout(resolve, 0));
    expect(document.querySelector('.approval-details-header')?.textContent).toContain('$1,200');
    const approve = Array.from(document.querySelectorAll<HTMLButtonElement>('.modal-confirm-actions button'))
      .find(b => b.textContent === 'Approve purchase');
    approve!.click();
    await done;

    expect(apiRequest).toHaveBeenCalledWith(`/purchases/approve/${EXEC_ID}`, expect.objectContaining({ method: 'POST' }));
  });

  it('loads the details through the token-authorized read, not the view:purchases route (issue #529)', async () => {
    (api.getApprovalDetails as jest.Mock).mockRejectedValue(Object.assign(new Error('execution not found'), { status: 404 }));

    await handlePurchaseDeeplink();

    expect(api.getApprovalDetails).toHaveBeenCalledWith(EXEC_ID, 'tok-xyz');
    expect(api.getPurchaseDetails).not.toHaveBeenCalled();
  });
});

describe('parsePurchaseDeeplink', () => {
  it('parses an approve deep-link with a token', () => {
    const dl = parsePurchaseDeeplink(
      '/purchases/approve/abc-123',
      '?token=tok-xyz',
    );
    expect(dl).toEqual({ action: 'approve', id: 'abc-123', token: 'tok-xyz' });
  });

  it('parses a cancel deep-link with a token', () => {
    const dl = parsePurchaseDeeplink(
      '/purchases/cancel/abc-123',
      '?token=tok-xyz',
    );
    expect(dl).toEqual({ action: 'cancel', id: 'abc-123', token: 'tok-xyz' });
  });

  it('parses a deep-link WITHOUT a token (handler surfaces the gap as a toast)', () => {
    const dl = parsePurchaseDeeplink('/purchases/approve/abc-123', '');
    expect(dl).toEqual({ action: 'approve', id: 'abc-123', token: '' });
  });

  it('rejects unknown action segments', () => {
    expect(parsePurchaseDeeplink('/purchases/hijack/abc', '?token=t')).toBeNull();
  });

  it('rejects paths with the wrong top-level segment', () => {
    expect(parsePurchaseDeeplink('/recommendations', '')).toBeNull();
    expect(parsePurchaseDeeplink('/history', '')).toBeNull();
    expect(parsePurchaseDeeplink('/', '')).toBeNull();
  });

  it('rejects paths missing the execution id', () => {
    expect(parsePurchaseDeeplink('/purchases/approve', '')).toBeNull();
    expect(parsePurchaseDeeplink('/purchases/approve/', '')).toBeNull();
  });

  it('rejects paths with extra segments (potential path-injection)', () => {
    expect(parsePurchaseDeeplink('/purchases/approve/abc/extra', '')).toBeNull();
  });

  it('tolerates trailing slashes on the id segment', () => {
    // split('/').filter(Boolean) collapses trailing /, so "/purchases/approve/abc/"
    // parses identically to the no-slash form.
    const dl = parsePurchaseDeeplink('/purchases/approve/abc/', '?token=t');
    expect(dl).toEqual({ action: 'approve', id: 'abc', token: 't' });
  });
});
