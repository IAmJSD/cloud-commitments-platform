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
    await saveGlobalSettings(new Event('submit'));
    expect(serviceCalls().filter(c => c.provider === 'gcp').every(c => c.payment === 'monthly')).toBe(true);
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

    expect(api.updateServiceConfig).toHaveBeenCalledTimes(17);
    expect(mockShowToast).toHaveBeenCalledTimes(1);
    expect(mockShowToast).toHaveBeenCalledWith({
      message: 'Settings saved, but 1 service failed: azure/compute: invalid payment option',
      kind: 'error',
    });
    expect(azurePayment.classList.contains('dirty')).toBe(true);
    expect(awsPayment.classList.contains('dirty')).toBe(false);
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
