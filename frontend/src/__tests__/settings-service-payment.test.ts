/**
 * Settings Save: every per-service PUT carries a payment token the backend
 * accepts for that service's provider (issue #545).
 *
 * The DOM is the real frontend/src/index.html and the accepted tokens are
 * read from internal/config/validation.go, so a card option or a backend
 * token change that makes them disagree fails here.
 */

import * as fs from 'fs';
import * as path from 'path';
import { loadGlobalSettings, saveGlobalSettings, setupSettingsHandlers } from '../settings';

jest.mock('../api', () => ({
  getConfig: jest.fn(),
  updateConfig: jest.fn(),
  updateServiceConfig: jest.fn(),
  getLadderConfigs: jest.fn().mockResolvedValue([]),
}));
jest.mock('../federation', () => ({
  initFederationPanel: jest.fn().mockResolvedValue(undefined),
}));
jest.mock('../confirmDialog', () => ({
  confirmDialog: jest.fn(() => Promise.resolve(true)),
}));
const mockShowToast = jest.fn<{ dismiss: () => void }, [unknown]>(() => ({ dismiss: jest.fn() }));
jest.mock('../toast', () => ({
  showToast: (opts: unknown) => mockShowToast(opts),
}));

import * as api from '../api';

function validPaymentsByProvider(): Record<string, string[]> {
  const src = fs.readFileSync(path.join(__dirname, '..', '..', '..', 'internal', 'config', 'validation.go'), 'utf-8');
  const block = /var ValidPaymentOptionsByProvider = map\[string\]\[\]string\{([\s\S]*?)\n\}/.exec(src);
  if (!block) throw new Error('ValidPaymentOptionsByProvider not found in validation.go');
  const out: Record<string, string[]> = {};
  for (const line of block[1]!.split('\n')) {
    const m = /"(\w+)":\s*\{([^}]*)\}/.exec(line);
    if (m) out[m[1]!] = [...m[2]!.matchAll(/"([\w-]+)"/g)].map(t => t[1]!);
  }
  if (Object.keys(out).length !== 3) throw new Error(`expected 3 providers in validation.go, parsed ${JSON.stringify(out)}`);
  return out;
}

function mountRealIndexHtml(): void {
  const html = fs.readFileSync(path.join(__dirname, '..', 'index.html'), 'utf-8');
  document.body.innerHTML = new DOMParser().parseFromString(html, 'text/html').body.innerHTML;
}

// Flip every per-service term card so each row counts as edited and is PUT.
function editEveryServiceTerm(): void {
  document.querySelectorAll<HTMLSelectElement>('select[id^="aws-"][id$="-term"], select[id^="azure-"][id$="-term"], select[id^="gcp-"][id$="-term"]')
    .forEach(sel => { sel.value = sel.value === '1' ? '3' : '1'; });
}

type ServiceCall = { provider: string; service: string; payment: string };

function serviceCalls(): ServiceCall[] {
  return (api.updateServiceConfig as jest.Mock).mock.calls.map(
    ([provider, service, cfg]) => ({ provider, service, payment: cfg.payment }),
  );
}

describe('Settings Save payment tokens (issue #545)', () => {
  beforeEach(() => {
    jest.clearAllMocks();
    mountRealIndexHtml();
    (api.getConfig as jest.Mock).mockResolvedValue({
      global: {
        enabled_providers: ['aws', 'azure', 'gcp'],
        default_term: 3,
        default_payment: 'all-upfront',
        default_coverage: 80,
        notification_days_before: 3,
      },
      services: [],
    });
    (api.updateConfig as jest.Mock).mockResolvedValue(undefined);
    (api.updateServiceConfig as jest.Mock).mockResolvedValue(undefined);
  });

  async function save(): Promise<void> {
    await loadGlobalSettings();
    editEveryServiceTerm();
    await saveGlobalSettings(new Event('submit'));
  }

  it('sends only backend-valid payments for the untouched card defaults', async () => {
    await save();

    const valid = validPaymentsByProvider();
    const calls = serviceCalls();
    expect(calls.filter(c => c.provider === 'azure')).toHaveLength(5);
    expect(calls.filter(c => c.provider === 'gcp')).toHaveLength(3);
    for (const c of calls) {
      expect({ ...c, valid: valid[c.provider]!.includes(c.payment) }).toEqual({ ...c, valid: true });
    }
  });

  it('sends the Azure option the user picked, and GCP monthly whatever the global default is', async () => {
    await loadGlobalSettings();
    editEveryServiceTerm();
    (document.getElementById('azure-vm-payment') as HTMLSelectElement).value = 'monthly';
    (document.getElementById('setting-default-payment') as HTMLSelectElement).value = 'partial-upfront';
    await saveGlobalSettings(new Event('submit'));

    const calls = serviceCalls();
    expect(calls.find(c => c.provider === 'azure' && c.service === 'compute')!.payment).toBe('monthly');
    expect(calls.find(c => c.provider === 'azure' && c.service === 'relational-db')!.payment).toBe('upfront');
    for (const c of calls.filter(x => x.provider === 'gcp')) expect(c.payment).toBe('monthly');
  });

  it('has no GCP Cloud Storage card and never PUTs gcp/storage (issue #543)', async () => {
    expect(document.getElementById('gcp-storage-term')).toBeNull();
    await save();
    expect(serviceCalls().filter(c => c.provider === 'gcp').map(c => c.service))
      .toEqual(['compute', 'relational-db', 'cache']);
  });

  it('ignores a stored gcp/storage row on load and keeps the other GCP cards', async () => {
    (api.getConfig as jest.Mock).mockResolvedValue({
      global: { enabled_providers: ['gcp'], default_term: 3, default_payment: 'all-upfront', default_coverage: 80 },
      services: [
        { provider: 'gcp', service: 'storage', term: 1, payment: 'monthly', enabled: true, coverage: 80 },
        { provider: 'gcp', service: 'compute', term: 1, payment: 'monthly', enabled: true, coverage: 80 },
      ],
    });
    await expect(loadGlobalSettings()).resolves.not.toThrow();
    expect(document.getElementById('gcp-storage-term')).toBeNull();
    expect((document.getElementById('gcp-compute-term') as HTMLSelectElement).value).toBe('1');
    editEveryServiceTerm();
    await saveGlobalSettings(new Event('submit'));
    // gcp/compute has a stored row, so only its term goes out; the new rows carry monthly.
    const gcp = serviceCalls().filter(c => c.provider === 'gcp');
    expect(gcp.map(c => [c.service, c.payment])).toEqual([
      ['compute', undefined],
      ['relational-db', 'monthly'],
      ['cache', 'monthly'],
    ]);
    expect(serviceCalls().some(c => c.service === 'storage')).toBe(false);
  });

  it('does not push the AWS default payment into Azure cards', async () => {
    await loadGlobalSettings();
    setupSettingsHandlers();
    const def = document.getElementById('setting-default-payment') as HTMLSelectElement;
    def.value = 'partial-upfront';
    def.dispatchEvent(new Event('change'));
    await new Promise(resolve => setTimeout(resolve, 0));

    expect((document.getElementById('aws-ec2-payment') as HTMLSelectElement).value).toBe('partial-upfront');
    expect((document.getElementById('azure-vm-payment') as HTMLSelectElement).value).toBe('upfront');
  });

  it('loads a persisted Azure payment into its card', async () => {
    (api.getConfig as jest.Mock).mockResolvedValue({
      global: { enabled_providers: ['azure'], default_term: 3, default_payment: 'all-upfront', default_coverage: 80 },
      services: [{ provider: 'azure', service: 'vm', term: 3, payment: 'monthly', enabled: true, coverage: 80 }],
    });
    await loadGlobalSettings();
    expect((document.getElementById('azure-vm-payment') as HTMLSelectElement).value).toBe('monthly');
  });

  it('one failed service PUT reports that service, still saves the rest and keeps only its fields dirty', async () => {
    const consoleError = jest.spyOn(console, 'error').mockImplementation(() => undefined);
    (api.updateServiceConfig as jest.Mock).mockImplementation(async (provider: string, service: string) => {
      if (provider === 'azure' && service === 'compute') throw new Error('invalid payment option');
    });
    await loadGlobalSettings();
    const azurePayment = document.getElementById('azure-vm-payment') as HTMLSelectElement;
    azurePayment.value = 'monthly';
    const awsPayment = document.getElementById('aws-ec2-payment') as HTMLSelectElement;
    awsPayment.value = 'no-upfront';

    await saveGlobalSettings(new Event('submit'));

    // Only the two edited rows are PUT (#539).
    expect(api.updateServiceConfig).toHaveBeenCalledTimes(2);
    expect(mockShowToast).toHaveBeenCalledTimes(1);
    expect(mockShowToast).toHaveBeenCalledWith({
      message: 'Settings saved, but 1 service failed: azure/compute: invalid payment option',
      kind: 'error',
    });
    expect(azurePayment.classList.contains('dirty')).toBe(true);
    expect(awsPayment.classList.contains('dirty')).toBe(false);
    consoleError.mockRestore();
  });
  // Issue #617: after #614 a scoped admin's service PUTs all 403. With no
  // global change and no service saved, the toast must not claim "Settings
  // saved" or repeat the same denial 17 times.
  function denied(): Error {
    return Object.assign(new Error('permission denied: global configuration requires unrestricted account access'), { status: 403 });
  }

  it('scoped admin: every service PUT denied shows one permission-denied failure toast', async () => {
    const consoleError = jest.spyOn(console, 'error').mockImplementation(() => undefined);
    (api.updateServiceConfig as jest.Mock).mockRejectedValue(denied());
    await loadGlobalSettings();
    const awsPayment = document.getElementById('aws-ec2-payment') as HTMLSelectElement;
    awsPayment.value = 'no-upfront';

    await saveGlobalSettings(new Event('submit'));

    expect(api.updateConfig).not.toHaveBeenCalled();
    expect(mockShowToast).toHaveBeenCalledTimes(1);
    expect(mockShowToast).toHaveBeenCalledWith({
      message: 'Failed to save settings: permission denied: global configuration requires unrestricted account access',
      kind: 'error',
    });
    // Nothing saved, so the edited card stays dirty for a retry.
    expect(awsPayment.classList.contains('dirty')).toBe(true);
    consoleError.mockRestore();
  });

  it('every service PUT failing for other reasons says the save failed, listing each service', async () => {
    const consoleError = jest.spyOn(console, 'error').mockImplementation(() => undefined);
    (api.updateServiceConfig as jest.Mock).mockImplementation(async (provider: string, service: string) => {
      throw provider === 'aws' && service === 'ec2' ? new Error('boom') : denied();
    });
    await loadGlobalSettings();
    editEveryServiceTerm();

    await saveGlobalSettings(new Event('submit'));

    const { message } = mockShowToast.mock.calls[0]![0] as { message: string };
    expect(message).toMatch(/^Failed to save settings: 17 services failed: /);
    expect(message).toContain('aws/ec2: boom');
    consoleError.mockRestore();
  });

  it('a saved global change still reports "Settings saved, but" when every service PUT fails', async () => {
    const consoleError = jest.spyOn(console, 'error').mockImplementation(() => undefined);
    (api.updateServiceConfig as jest.Mock).mockRejectedValue(new Error('unavailable'));
    await loadGlobalSettings();
    const notify = document.getElementById('setting-notification-days') as HTMLInputElement;
    notify.value = '9';
    editEveryServiceTerm();

    await saveGlobalSettings(new Event('submit'));

    expect(api.updateConfig).toHaveBeenCalledTimes(1);
    const { message } = mockShowToast.mock.calls[0]![0] as { message: string };
    expect(message).toMatch(/^Settings saved, but 17 services failed: /);
    consoleError.mockRestore();
  });

  it.each([
    ['no-upfront', 'monthly'],
    ['partial-upfront', 'monthly'],
    ['all-upfront', 'upfront'],
    ['', 'monthly'],
    ['junk', 'monthly'],
  ])('loads legacy stored Azure payment %p into the %s option and Save sends it', async (stored, expected) => {
    (api.getConfig as jest.Mock).mockResolvedValue({
      global: { enabled_providers: ['azure'], default_term: 3, default_payment: 'all-upfront', default_coverage: 80 },
      services: [{ provider: 'azure', service: 'vm', term: 3, payment: stored, enabled: true, coverage: 80 }],
    });
    await save();

    expect((document.getElementById('azure-vm-payment') as HTMLSelectElement).value).toBe(expected);
    expect(serviceCalls().find(c => c.provider === 'azure' && c.service === 'compute')!.payment).toBe(expected);
  });

  it('a stored legacy payment the card normalised on load goes out with a partial PUT of its row', async () => {
    (api.getConfig as jest.Mock).mockResolvedValue({
      global: { enabled_providers: ['azure'], default_term: 3, default_payment: 'all-upfront', default_coverage: 80 },
      services: [{ provider: 'azure', service: 'compute', term: 3, payment: 'no-upfront', enabled: true, coverage: 80 }],
    });
    await loadGlobalSettings();
    (document.getElementById('azure-vm-term') as HTMLSelectElement).value = '1';
    await saveGlobalSettings(new Event('submit'));

    expect((api.updateServiceConfig as jest.Mock).mock.calls).toEqual([
      ['azure', 'compute', { provider: 'azure', service: 'compute', term: 1, payment: 'monthly' }],
    ]);
  });

  it.each([
    ['all-upfront', { provider: 'gcp', service: 'compute', term: 1, payment: 'monthly' }],
    ['monthly', { provider: 'gcp', service: 'compute', term: 1 }],
  ])('a GCP row holding payment %p: a term edit PUTs %j', async (stored, body) => {
    (api.getConfig as jest.Mock).mockResolvedValue({
      global: { enabled_providers: ['gcp'], default_term: 3, default_payment: 'all-upfront', default_coverage: 80 },
      services: [{ provider: 'gcp', service: 'compute', term: 3, payment: stored, enabled: true, coverage: 80 }],
    });
    await loadGlobalSettings();
    (document.getElementById('gcp-compute-term') as HTMLSelectElement).value = '1';
    await saveGlobalSettings(new Event('submit'));

    expect((api.updateServiceConfig as jest.Mock).mock.calls).toEqual([['gcp', 'compute', body]]);
  });

  it('an emptied Savings Plan coverage card shows an inline error', async () => {
    await loadGlobalSettings();
    setupSettingsHandlers();
    const card = document.getElementById('aws-savings-plans-compute-coverage') as HTMLInputElement;
    card.value = '';
    card.dispatchEvent(new Event('blur'));

    expect(card.getAttribute('aria-invalid')).toBe('true');
  });

  it('never falls back to upfront when a payment select has no matching option at Save', async () => {
    await loadGlobalSettings();
    (document.getElementById('azure-vm-payment') as HTMLSelectElement).value = 'junk';
    expect((document.getElementById('azure-vm-payment') as HTMLSelectElement).value).toBe('');
    await saveGlobalSettings(new Event('submit'));

    expect(serviceCalls().find(c => c.provider === 'azure' && c.service === 'compute')!.payment).toBe('monthly');
  });

  it('counts only AWS services when asking to propagate the default payment', async () => {
    const { confirmDialog } = jest.requireMock('../confirmDialog') as { confirmDialog: jest.Mock };
    await loadGlobalSettings();
    setupSettingsHandlers();
    const awsCount = document.querySelectorAll('select[id^="aws-"][id$="-payment"]').length;
    const def = document.getElementById('setting-default-payment') as HTMLSelectElement;
    def.value = 'partial-upfront';
    def.dispatchEvent(new Event('change'));
    await new Promise(resolve => setTimeout(resolve, 0));

    expect(confirmDialog).toHaveBeenCalledTimes(1);
    expect(confirmDialog.mock.calls[0]![0].title).toBe(`Apply "Partial Upfront" to ${awsCount} AWS services?`);
  });

  it('a failed service keeps its term, payment and coverage fields dirty', async () => {
    const consoleError = jest.spyOn(console, 'error').mockImplementation(() => undefined);
    (api.updateServiceConfig as jest.Mock).mockImplementation(async (provider: string, service: string) => {
      if (provider === 'aws' && service === 'savings-plans-compute') throw new Error('boom');
    });
    await loadGlobalSettings();
    const term = document.getElementById('aws-savings-plans-compute-term') as HTMLSelectElement;
    const payment = document.getElementById('aws-savings-plans-compute-payment') as HTMLSelectElement;
    const coverage = document.getElementById('aws-savings-plans-compute-coverage') as HTMLInputElement;
    term.value = term.value === '1' ? '3' : '1';
    payment.value = payment.value === 'no-upfront' ? 'all-upfront' : 'no-upfront';
    coverage.value = '55';

    await saveGlobalSettings(new Event('submit'));

    for (const el of [term, payment, coverage]) expect({ id: el.id, dirty: el.classList.contains('dirty') }).toEqual({ id: el.id, dirty: true });
    consoleError.mockRestore();
  });
});
