/**
 * Registrations module — default-filter behaviour (refs #45).
 *
 * The Account Registrations status dropdown defaults to "Pending" in
 * index.html. `loadRegistrations` reads that select and forwards the value
 * to the API, so on first paint the panel must request only pending rows.
 */
import { initRegistrations, loadRegistrations } from '../modules/registrations';

jest.mock('../api', () => ({
  listRegistrations: jest.fn(),
}));

import * as api from '../api';

describe('loadRegistrations default filter', () => {
  beforeEach(() => {
    document.body.replaceChildren();
    jest.clearAllMocks();
  });

  function buildDOM(initialFilterValue: string): void {
    const container = document.createElement('div');
    container.id = 'registrations-list';
    document.body.appendChild(container);

    const select = document.createElement('select');
    select.id = 'registrations-status-filter';
    for (const v of ['', 'pending', 'approved', 'rejected']) {
      const opt = document.createElement('option');
      opt.value = v;
      // Mirror index.html: "pending" carries the `selected` attribute.
      if (v === 'pending') opt.defaultSelected = true;
      select.appendChild(opt);
    }
    select.value = initialFilterValue;
    document.body.appendChild(select);
  }

  test('passes "pending" to listRegistrations on first load', async () => {
    buildDOM('pending');
    (api.listRegistrations as jest.Mock).mockResolvedValue([]);

    await loadRegistrations();

    expect(api.listRegistrations).toHaveBeenCalledTimes(1);
    expect(api.listRegistrations).toHaveBeenCalledWith('pending');
  });

  test('passes undefined when the user picks "All" (empty value)', async () => {
    buildDOM('');
    (api.listRegistrations as jest.Mock).mockResolvedValue([]);

    await loadRegistrations();

    expect(api.listRegistrations).toHaveBeenCalledWith(undefined);
  });

  test('repeated initRegistrations (one per Accounts visit) wires the filter only once', async () => {
    buildDOM('pending');
    (api.listRegistrations as jest.Mock).mockResolvedValue([]);
    for (let i = 0; i < 5; i++) initRegistrations();
    await Promise.resolve();
    (api.listRegistrations as jest.Mock).mockClear();

    const select = document.getElementById('registrations-status-filter') as HTMLSelectElement;
    select.value = '';
    select.dispatchEvent(new Event('change'));

    expect(api.listRegistrations).toHaveBeenCalledTimes(1);
    expect(api.listRegistrations).toHaveBeenCalledWith(undefined);
  });
});

describe('registration row actions', () => {
  beforeEach(() => {
    document.body.replaceChildren();
    jest.clearAllMocks();
  });

  test('Approve stays a text button; Reject and Delete are named icon buttons in one row', async () => {
    const container = document.createElement('div');
    container.id = 'registrations-list';
    document.body.appendChild(container);
    (api.listRegistrations as jest.Mock).mockResolvedValue([{
      id: 'r1', provider: 'aws', account_name: 'Checkout', external_id: '650412398877',
      contact_email: 'c@example.com', status: 'pending', created_at: '2026-10-28T12:00:00Z',
    }]);

    await loadRegistrations();

    const actions = container.querySelector('tbody tr td:last-child .row-actions')!;
    const buttons = Array.from(actions.querySelectorAll('button'));
    expect(buttons.map(b => b.getAttribute('aria-label'))).toEqual([
      'Approve registration Checkout',
      'Reject registration Checkout',
      'Delete registration Checkout',
    ]);
    expect(buttons[0]!.textContent).toBe('Approve');
    for (const [btn, icon, title] of [[buttons[1]!, 'x-circle', 'Reject registration'], [buttons[2]!, 'trash-2', 'Delete registration']] as const) {
      expect(btn.classList.contains('icon-btn')).toBe(true);
      expect(btn.querySelector(`svg.icon-${icon}`)).not.toBeNull();
      expect(btn.title).toBe(title);
    }
    expect(buttons[2]!.classList.contains('btn-danger')).toBe(true);
  });
});
