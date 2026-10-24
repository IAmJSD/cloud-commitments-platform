/**
 * RI-exchange approve/reject deep links (issue #579). The email links land on
 * an SPA path; the handler confirms and then POSTs. Only the network layer is
 * mocked: the real confirm dialog and toasts are driven.
 */

jest.mock('../api', () => ({ getRIExchangeHistory: jest.fn() }));
jest.mock('../api/client', () => ({ apiRequest: jest.fn() }));

import * as api from '../api';
import { apiRequest } from '../api/client';
import { handleRIExchangeDeeplink, parseRIExchangeDeeplink, RI_EXCHANGE_LANDING_PATH } from '../riexchange-deeplink';

const ID = '22222222-2222-2222-2222-222222222222';

const RECORD = {
  id: ID, account_id: 'acct-001', exchange_id: '', region: 'us-east-1', source_ri_ids: ['ri-1'],
  source_instance_type: 'm5.large', source_count: 2, target_offering_id: 'off-1', target_instance_type: 'm6i.large',
  target_count: 2, payment_due: '12.34', status: 'pending', mode: 'manual', created_at: '', updated_at: '',
};

function dialogButton(label: string): HTMLButtonElement | undefined {
  return Array.from(document.querySelectorAll<HTMLButtonElement>('.modal-confirm-actions button'))
    .find(b => b.textContent === label);
}

const flush = (): Promise<void> => new Promise(resolve => setTimeout(resolve, 0));

describe('parseRIExchangeDeeplink', () => {
  it.each(['approve', 'reject'] as const)('parses a %s link with its token', action => {
    expect(parseRIExchangeDeeplink(`/inventory/ri-exchange/${action}/${ID}`, '?token=tok-1'))
      .toEqual({ action, id: ID, token: 'tok-1' });
  });

  it('keeps an empty token so the handler can explain the gap', () => {
    expect(parseRIExchangeDeeplink(`/inventory/ri-exchange/approve/${ID}`, '')).toEqual({ action: 'approve', id: ID, token: '' });
  });

  it('tolerates a trailing slash', () => {
    expect(parseRIExchangeDeeplink(`/inventory/ri-exchange/reject/${ID}/`, '?token=t')?.id).toBe(ID);
  });

  it.each([
    ['the plain sub-tab', '/inventory/ri-exchange'],
    ['an unknown action', `/inventory/ri-exchange/execute/${ID}`],
    ['a missing id', '/inventory/ri-exchange/approve'],
    ['extra segments', `/inventory/ri-exchange/approve/${ID}/extra`],
    ['another inventory sub-tab', `/inventory/coverage/approve/${ID}`],
    ['the legacy prefix the router rewrites', `/ri-exchange/approve/${ID}`],
    ['the purchase deep link', `/purchases/approve/${ID}`],
  ])('ignores %s', (_label, path) => {
    expect(parseRIExchangeDeeplink(path, '?token=t')).toBeNull();
  });
});

describe('handleRIExchangeDeeplink', () => {
  beforeEach(() => {
    document.body.innerHTML = '';
    jest.clearAllMocks();
    (api.getRIExchangeHistory as jest.Mock).mockResolvedValue([RECORD]);
    (apiRequest as jest.Mock).mockResolvedValue({ status: 'ok' });
  });

  it('does nothing outside a deep link', async () => {
    window.history.replaceState({}, '', '/inventory/ri-exchange');
    await expect(handleRIExchangeDeeplink()).resolves.toBe(false);
    expect(api.getRIExchangeHistory).not.toHaveBeenCalled();
    expect(apiRequest).not.toHaveBeenCalled();
  });

  it('opens a confirm dialog instead of acting on landing, then POSTs the token on approve', async () => {
    window.history.replaceState({}, '', `/inventory/ri-exchange/approve/${ID}?token=tok%2Babc`);

    const done = handleRIExchangeDeeplink();
    await flush();
    // Landing alone has not changed anything.
    expect(apiRequest).not.toHaveBeenCalled();
    const body = document.querySelector('.modal-confirm-backdrop')?.textContent ?? '';
    expect(body).toContain('Approve RI exchange?');
    expect(body).toContain('2x m5.large -> 2x m6i.large in us-east-1');
    expect(body).toContain('USD 12.34');
    expect(body).toContain('cannot be reversed');
    // Money moves on approve: Enter must not confirm by reflex.
    expect(document.activeElement).toBe(dialogButton('Not now'));

    dialogButton('Approve exchange')!.click();
    await expect(done).resolves.toBe(true);

    expect(apiRequest).toHaveBeenCalledTimes(1);
    expect(apiRequest).toHaveBeenCalledWith(
      `/ri-exchange/approve/${ID}?token=tok%2Babc`,
      expect.objectContaining({ method: 'POST' }),
    );
    expect(document.querySelector('[role="status"]')?.textContent).toContain('RI exchange approved and executing.');
    expect(window.location.pathname).toBe(RI_EXCHANGE_LANDING_PATH);
    expect(window.location.search).toBe('');
  });

  it('POSTs to the reject route on reject', async () => {
    window.history.replaceState({}, '', `/inventory/ri-exchange/reject/${ID}?token=tok-r`);

    const done = handleRIExchangeDeeplink();
    await flush();
    expect(document.querySelector('.modal-confirm-backdrop')?.textContent).toContain('no reservations are changed');
    dialogButton('Reject exchange')!.click();
    await done;

    expect(apiRequest).toHaveBeenCalledWith(`/ri-exchange/reject/${ID}?token=tok-r`, expect.objectContaining({ method: 'POST' }));
    expect(document.querySelector('[role="status"]')?.textContent).toContain('RI exchange rejected.');
  });

  it('does not POST when the user dismisses the dialog, and leaves the deep link', async () => {
    window.history.replaceState({}, '', `/inventory/ri-exchange/approve/${ID}?token=tok-1`);

    const done = handleRIExchangeDeeplink();
    await flush();
    dialogButton('Not now')!.click();
    await expect(done).resolves.toBe(true);

    expect(apiRequest).not.toHaveBeenCalled();
    expect(window.location.pathname).toBe(RI_EXCHANGE_LANDING_PATH);
  });

  it('falls back to generic wording when the session cannot read the history', async () => {
    (api.getRIExchangeHistory as jest.Mock).mockRejectedValue(Object.assign(new Error('forbidden'), { status: 403 }));
    window.history.replaceState({}, '', `/inventory/ri-exchange/approve/${ID}?token=tok-1`);

    const done = handleRIExchangeDeeplink();
    await flush();
    expect(document.querySelector('.modal-confirm-backdrop')?.textContent).toContain(`RI exchange ${ID}.`);
    dialogButton('Approve exchange')!.click();
    await done;
    expect(apiRequest).toHaveBeenCalledTimes(1);
  });

  it('reports a missing token without asking or POSTing', async () => {
    window.history.replaceState({}, '', `/inventory/ri-exchange/approve/${ID}`);

    await expect(handleRIExchangeDeeplink()).resolves.toBe(true);

    expect(document.querySelector('.modal-confirm-backdrop')).toBeNull();
    expect(document.querySelector('[role="alert"]')?.textContent).toContain('Missing approval token');
    expect(apiRequest).not.toHaveBeenCalled();
    expect(window.location.pathname).toBe(RI_EXCHANGE_LANDING_PATH);
  });

  it('says so instead of asking when the exchange is no longer pending', async () => {
    (api.getRIExchangeHistory as jest.Mock).mockResolvedValue([{ ...RECORD, status: 'completed' }]);
    window.history.replaceState({}, '', `/inventory/ri-exchange/reject/${ID}?token=tok-1`);

    await handleRIExchangeDeeplink();

    expect(document.querySelector('.modal-confirm-backdrop')).toBeNull();
    expect(document.querySelector('[role="status"]')?.textContent).toContain('no longer pending (status: completed)');
    expect(apiRequest).not.toHaveBeenCalled();
  });

  it('surfaces the server error when the POST fails', async () => {
    (apiRequest as jest.Mock).mockRejectedValue(Object.assign(new Error('invalid approval token'), { status: 403 }));
    window.history.replaceState({}, '', `/inventory/ri-exchange/approve/${ID}?token=bad`);

    const done = handleRIExchangeDeeplink();
    await flush();
    dialogButton('Approve exchange')!.click();
    await done;

    expect(document.querySelector('[role="alert"]')?.textContent).toContain('Failed to approve RI exchange: invalid approval token');
    expect(window.location.pathname).toBe(RI_EXCHANGE_LANDING_PATH);
  });
});
