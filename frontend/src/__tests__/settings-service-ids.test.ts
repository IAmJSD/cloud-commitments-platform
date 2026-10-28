/**
 * Settings service ids for Azure and GCP (issue #613).
 *
 * Recommendations carry the provider's common.ServiceType slug and the
 * scheduler / purchase flow match service configs and per-account overrides
 * on that exact id, so Settings must write the same ids. The DOM is the real
 * frontend/src/index.html and the backend slug list is read from
 * internal/purchase/execution.go (mapServiceSlug), so a drift fails here.
 */

import * as fs from 'fs';
import * as path from 'path';
import { loadGlobalSettings, saveGlobalSettings, loadOverridesPanel } from '../settings';
import { SETTINGS_SERVICE_IDS, LEGACY_SETTINGS_SERVICES, canonicalizeServiceConfigs, legacySettingsServiceTarget } from '../lib/settings-service-ids';

jest.mock('../api', () => ({
  getConfig: jest.fn(),
  updateConfig: jest.fn(),
  updateServiceConfig: jest.fn(),
  getLadderConfigs: jest.fn().mockResolvedValue([]),
  listAccountServiceOverrides: jest.fn(),
  saveAccountServiceOverride: jest.fn(),
  deleteAccountServiceOverride: jest.fn(),
}));
jest.mock('../federation', () => ({ initFederationPanel: jest.fn().mockResolvedValue(undefined) }));
jest.mock('../confirmDialog', () => ({ confirmDialog: jest.fn(() => Promise.resolve(true)) }));
jest.mock('../toast', () => ({ showToast: jest.fn(() => ({ dismiss: jest.fn() })) }));

import * as api from '../api';

function backendServiceSlugs(): string[] {
  const src = fs.readFileSync(path.join(__dirname, '..', '..', '..', 'internal', 'purchase', 'execution.go'), 'utf-8');
  const block = /func mapServiceSlug\(service string\)[\s\S]*?slugs := map\[string\]common\.ServiceType\{([\s\S]*?)\n\t\}/.exec(src);
  if (!block) throw new Error('mapServiceSlug slug map not found in execution.go');
  const slugs = [...block[1]!.matchAll(/"([\w-]+)":/g)].map(m => m[1]!);
  if (!slugs.includes('compute')) throw new Error(`unexpected mapServiceSlug parse: ${JSON.stringify(slugs)}`);
  return slugs;
}

function mountRealIndexHtml(): void {
  const html = fs.readFileSync(path.join(__dirname, '..', 'index.html'), 'utf-8');
  document.body.innerHTML = new DOMParser().parseFromString(html, 'text/html').body.innerHTML;
}

const flush = () => new Promise(r => setTimeout(r, 0));

function mockConfig(services: unknown[]): void {
  (api.getConfig as jest.Mock).mockResolvedValue({
    global: { enabled_providers: ['aws', 'azure', 'gcp'], default_term: 3, default_payment: 'all-upfront', default_coverage: 80, notification_days_before: 3 },
    services,
  });
}

function servicePuts(provider?: string): Array<{ provider: string; service: string; cfg: Record<string, unknown> }> {
  return (api.updateServiceConfig as jest.Mock).mock.calls
    .map(([p, s, cfg]) => ({ provider: p, service: s, cfg }))
    .filter(c => provider === undefined || c.provider === provider);
}

async function overrideOptions(provider: 'azure' | 'gcp'): Promise<string[]> {
  (api.listAccountServiceOverrides as jest.Mock).mockResolvedValue([]);
  const panel = document.createElement('div');
  document.body.appendChild(panel);
  await loadOverridesPanel('acct1', panel, provider);
  await flush();
  return Array.from((document.getElementById('override-service') as HTMLSelectElement).options).map(o => o.value);
}

describe('canonicalizeServiceConfigs (issue #613)', () => {
  it('maps every legacy id onto a current Settings id of the same provider', () => {
    for (const [key, target] of Object.entries(LEGACY_SETTINGS_SERVICES)) {
      const provider = key.split('/')[0] as 'azure' | 'gcp';
      expect(SETTINGS_SERVICE_IDS[provider]).toContain(target);
    }
  });

  it('re-keys a legacy row, keeps a current row over its legacy twin and passes others through', () => {
    const rows = [
      { provider: 'azure', service: 'vm', term: 1 },
      { provider: 'gcp', service: 'memorystore', term: 1 },
      { provider: 'gcp', service: 'cache', term: 3 },
      { provider: 'aws', service: 'ec2', term: 3 },
      { provider: 'gcp', service: 'storage', term: 1 },
    ];
    expect(canonicalizeServiceConfigs(rows)).toEqual([
      { provider: 'azure', service: 'compute', term: 1 },
      { provider: 'gcp', service: 'cache', term: 3 },
      { provider: 'aws', service: 'ec2', term: 3 },
      { provider: 'gcp', service: 'storage', term: 1 },
    ]);
    expect(rows[0]!.service).toBe('vm');
  });

  it.each([['aws', 'vm'], ['azure', 'compute'], ['gcp', 'cloudsql'], ['azure', 'toString'], ['', '']])(
    'does not treat %s/%s as a legacy Settings id', (provider, service) => {
      expect(legacySettingsServiceTarget(provider, service)).toBeNull();
    });
});

describe('Settings Azure/GCP service ids (issue #613)', () => {
  beforeEach(() => {
    jest.clearAllMocks();
    mountRealIndexHtml();
    mockConfig([]);
    (api.updateConfig as jest.Mock).mockResolvedValue(undefined);
    (api.updateServiceConfig as jest.Mock).mockResolvedValue(undefined);
  });

  it('every Azure/GCP id the cards save and the override list offers is a backend service slug', async () => {
    await loadGlobalSettings();
    // Only edited rows are saved (#539): edit each Azure/GCP term.
    document.querySelectorAll<HTMLSelectElement>('select[id^="azure-"][id$="-term"], select[id^="gcp-"][id$="-term"]')
      .forEach(sel => { sel.value = sel.value === '1' ? '3' : '1'; });
    await saveGlobalSettings(new Event('submit'));
    const slugs = backendServiceSlugs();
    const offered: string[] = [];
    for (const provider of ['azure', 'gcp'] as const) {
      const saved = servicePuts(provider).map(c => c.service);
      expect(saved).toEqual(SETTINGS_SERVICE_IDS[provider]);
      expect(await overrideOptions(provider)).toEqual(SETTINGS_SERVICE_IDS[provider]);
      offered.push(...saved.map(s => `${provider}/${s}`));
    }
    expect(offered.filter(id => !slugs.includes(id.split('/')[1]!))).toEqual([]);
  });

  it('shows rows stored under the old ids on their cards and saves them under the current ids', async () => {
    mockConfig([
      { provider: 'azure', service: 'vm', term: 1, payment: 'monthly', enabled: true, coverage: 80, ramp_schedule: 'r1', include_regions: ['eastus'] },
      { provider: 'azure', service: 'cosmosdb', term: 1, payment: 'monthly', enabled: true, coverage: 80 },
      { provider: 'gcp', service: 'memorystore', term: 1, payment: 'monthly', enabled: true, coverage: 80 },
    ]);
    await loadGlobalSettings();
    expect((document.getElementById('azure-vm-term') as HTMLSelectElement).value).toBe('1');
    expect((document.getElementById('azure-vm-payment') as HTMLSelectElement).value).toBe('monthly');
    expect((document.getElementById('azure-cosmosdb-term') as HTMLSelectElement).value).toBe('1');
    expect((document.getElementById('gcp-memorystore-term') as HTMLSelectElement).value).toBe('1');
    expect((document.getElementById('azure-sql-term') as HTMLSelectElement).value).toBe('3');

    await saveGlobalSettings(new Event('submit'));
    const puts = servicePuts();
    const legacy = new Set(Object.keys(LEGACY_SETTINGS_SERVICES));
    expect(puts.filter(c => legacy.has(`${c.provider}/${c.service}`))).toEqual([]);
    const compute = puts.find(c => c.provider === 'azure' && c.service === 'compute')!.cfg;
    expect(compute).toEqual(expect.objectContaining({ term: 1, payment: 'monthly', ramp_schedule: 'r1', include_regions: ['eastus'] }));
    expect(puts.find(c => c.provider === 'azure' && c.service === 'nosql')!.cfg['term']).toBe(1);
    expect(puts.find(c => c.provider === 'gcp' && c.service === 'cache')!.cfg['term']).toBe(1);
  });

  it('writes a re-keyed row in full once, then only its changed fields', async () => {
    mockConfig([
      { provider: 'azure', service: 'vm', term: 1, payment: 'monthly', enabled: true, coverage: 80, ramp_schedule: 'r1' },
    ]);
    await loadGlobalSettings();
    await saveGlobalSettings(new Event('submit'));
    const first = servicePuts().filter(c => c.provider === 'azure' && c.service === 'compute');
    expect(first).toHaveLength(1);
    expect(first[0]!.cfg).toEqual(expect.objectContaining({ ramp_schedule: 'r1', coverage: 80 }));
    (api.updateServiceConfig as jest.Mock).mockClear();

    await saveGlobalSettings(new Event('submit'));
    expect(servicePuts()).toEqual([]);

    (document.getElementById('azure-vm-term') as HTMLSelectElement).value = '3';
    await saveGlobalSettings(new Event('submit'));
    const second = servicePuts();
    expect(second).toHaveLength(1);
    expect(Object.keys(second[0]!.cfg).sort()).toEqual(['provider', 'service', 'term']);
  });

  it('prefers a row already stored under the current id over its legacy twin', async () => {
    mockConfig([
      { provider: 'azure', service: 'vm', term: 1, payment: 'monthly', enabled: true, coverage: 80 },
      { provider: 'azure', service: 'compute', term: 3, payment: 'upfront', enabled: true, coverage: 80 },
    ]);
    await loadGlobalSettings();
    expect((document.getElementById('azure-vm-term') as HTMLSelectElement).value).toBe('3');
    expect((document.getElementById('azure-vm-payment') as HTMLSelectElement).value).toBe('upfront');
  });

  it('flags an override stored under an old id and still offers the current id', async () => {
    (api.listAccountServiceOverrides as jest.Mock).mockResolvedValue([
      { id: 'o1', account_id: 'acct1', provider: 'azure', service: 'vm', term: 1, enabled: true },
      { id: 'o2', account_id: 'acct1', provider: 'azure', service: 'relational-db', term: 1, enabled: true },
    ]);
    const panel = document.createElement('div');
    document.body.appendChild(panel);
    await loadOverridesPanel('acct1', panel, 'azure');
    await flush();
    const rows = Array.from(panel.querySelectorAll('tbody tr'));
    const vmRow = rows.find(r => r.querySelector('td')!.textContent!.startsWith('azure/vm'))!;
    const dbRow = rows.find(r => r.querySelector('td')!.textContent!.startsWith('azure/relational-db'))!;
    expect(vmRow.querySelector('.override-legacy-hint')!.textContent).toContain('azure/compute');
    expect(dbRow.querySelector('.override-legacy-hint')).toBeNull();

    (panel.querySelector('button') as HTMLButtonElement).click();
    await flush();
    const options = Array.from((document.getElementById('override-service') as HTMLSelectElement).options).map(o => o.value);
    expect(options).toEqual(['compute', 'nosql', 'cache', 'search']);
  });
});
