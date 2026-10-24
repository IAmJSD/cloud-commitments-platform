/**
 * GCP Cloud Storage has no commitment product (issue #543). The UI can no
 * longer set it, but stored rows and overrides must survive and stay visible.
 */

import * as fs from 'fs';
import * as path from 'path';
import { loadGlobalSettings, saveGlobalSettings, loadOverridesPanel } from '../settings';

jest.mock('../api', () => ({
  getConfig: jest.fn(),
  updateConfig: jest.fn(),
  updateServiceConfig: jest.fn(),
  getLadderConfigs: jest.fn().mockResolvedValue([]),
  listAccountServiceOverrides: jest.fn(),
  saveAccountServiceOverride: jest.fn(),
  deleteAccountServiceOverride: jest.fn(),
}));
jest.mock('../federation', () => ({
  initFederationPanel: jest.fn().mockResolvedValue(undefined),
}));
jest.mock('../confirmDialog', () => ({
  confirmDialog: jest.fn(() => Promise.resolve(true)),
}));
jest.mock('../toast', () => ({
  showToast: jest.fn(() => ({ dismiss: jest.fn() })),
}));

import * as api from '../api';

const storageOverride = { provider: 'gcp', service: 'storage', term: 1, payment: 'monthly', coverage: 40, enabled: true };

describe('GCP storage removal (issue #543)', () => {
  let panel: HTMLElement;

  beforeEach(async () => {
    jest.clearAllMocks();
    const html = fs.readFileSync(path.join(__dirname, '..', 'index.html'), 'utf-8');
    document.body.innerHTML = new DOMParser().parseFromString(html, 'text/html').body.innerHTML;
    (api.getConfig as jest.Mock).mockResolvedValue({
      global: { enabled_providers: ['gcp'], default_term: 3, default_payment: 'all-upfront', default_coverage: 80, notification_days_before: 3 },
      services: [{ provider: 'gcp', service: 'storage', term: 1, payment: 'monthly', enabled: true, coverage: 55 }],
    });
    (api.updateConfig as jest.Mock).mockResolvedValue(undefined);
    (api.updateServiceConfig as jest.Mock).mockResolvedValue(undefined);
    (api.listAccountServiceOverrides as jest.Mock).mockResolvedValue([storageOverride]);
    panel = document.createElement('div');
    document.body.appendChild(panel);
    await loadOverridesPanel('acct1', panel, 'gcp' as never);
    await new Promise(r => setTimeout(r, 0));
  });

  it('offers compute, relational-db and cache, not storage, in the override Add list', async () => {
    (api.listAccountServiceOverrides as jest.Mock).mockResolvedValue([]);
    panel.innerHTML = '';
    await loadOverridesPanel('acct1', panel, 'gcp' as never);
    await new Promise(r => setTimeout(r, 0));
    const add = Array.from(panel.querySelectorAll('button')).find(b => b.textContent === 'Add override') as HTMLButtonElement;
    add.click();
    await new Promise(r => setTimeout(r, 0));
    const options = Array.from((document.getElementById('override-service') as HTMLSelectElement).options).map(o => o.value);
    expect(options).toEqual(['compute', 'relational-db', 'cache']);
  });

  it('still renders a stored gcp/storage override with a Delete button', () => {
    const row = Array.from(panel.querySelectorAll('tbody tr')).find(r => r.textContent?.includes('gcp/storage'));
    expect(row).toBeDefined();
    expect(Array.from(row!.querySelectorAll('button')).some(b => /delete/i.test(b.textContent ?? ''))).toBe(true);
  });

  it('an unrelated Save does not touch the stored gcp/storage service row or override', async () => {
    await loadGlobalSettings();
    await saveGlobalSettings(new Event('submit'));
    const puts = (api.updateServiceConfig as jest.Mock).mock.calls.map(c => `${c[0]}/${c[1]}`);
    expect(puts).not.toContain('gcp/storage');
    expect(api.deleteAccountServiceOverride).not.toHaveBeenCalled();
    expect(api.saveAccountServiceOverride).not.toHaveBeenCalled();
  });
});
