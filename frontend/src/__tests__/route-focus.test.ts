/**
 * Route-change focus and sidebar aria-current (issue #730).
 *
 * After a user-driven page change, focus moves to the new page's heading
 * so a screen reader announces the page and keyboard users continue from
 * its top instead of staying on the sidebar link.
 */
import { switchTab, switchSettingsSubTab, initRouter } from '../navigation';

jest.mock('../dashboard', () => ({ loadDashboard: jest.fn().mockResolvedValue(undefined) }));
jest.mock('../recommendations', () => ({ loadRecommendations: jest.fn().mockResolvedValue(undefined) }));
jest.mock('../plans', () => ({ loadPlans: jest.fn().mockResolvedValue(undefined) }));
jest.mock('../history', () => ({
  initHistoryDateRange: jest.fn(),
  loadHistory: jest.fn().mockResolvedValue(undefined),
}));
jest.mock('../settings', () => ({
  loadGlobalSettings: jest.fn().mockResolvedValue(undefined),
  isUnsavedChanges: jest.fn().mockReturnValue(false),
  loadAccountsTab: jest.fn().mockResolvedValue(undefined),
}));
jest.mock('../users', () => ({ loadUsers: jest.fn() }));
jest.mock('../apikeys', () => ({ loadApiKeys: jest.fn() }));
jest.mock('../modules/savings-history', () => ({ loadSavingsHistory: jest.fn() }));
jest.mock('../riexchange', () => ({
  loadRIExchange: jest.fn().mockResolvedValue(undefined),
  loadAutomationSettings: jest.fn().mockResolvedValue(undefined),
}));
jest.mock('../auth', () => ({ isAdmin: jest.fn().mockReturnValue(true) }));
jest.mock('../permissions', () => ({ canAccess: jest.fn().mockReturnValue(true) }));
jest.mock('../inventory', () => ({
  DEFAULT_INVENTORY_SUB_SECTION: 'active-commitments',
  isValidInventorySubSection: (n: string): boolean => ['active-commitments', 'coverage', 'ri-exchange'].includes(n),
  switchInventorySubSection: jest.fn((n: string) => n),
  loadInventory: jest.fn(),
}));

import { canAccess } from '../permissions';

const TABS = ['home', 'opportunities', 'plans', 'purchases', 'inventory', 'admin'];

function buildDOM(): void {
  document.body.innerHTML = `
    <nav class="app-sidebar-nav" aria-label="Main navigation">
      ${TABS.map(t => `<a class="tab-btn" id="${t}-tab-btn" href="/${t}" data-tab="${t}">${t}</a>`).join('')}
    </nav>
    ${TABS.map(t => `
      <div id="${t}-tab" class="tab-content">
        <h2 class="sr-only page-heading" tabindex="-1">${t} page</h2>
        <section><button class="inner">Inner</button></section>
      </div>`).join('')}
  `;
  const admin = document.getElementById('admin-tab')!;
  admin.insertAdjacentHTML('beforeend', `
    <div class="settings-tabs" role="tablist">
      <button class="sub-tab-btn" data-settings-tab="general" role="tab" aria-selected="true" tabindex="0">General</button>
      <button class="sub-tab-btn" data-settings-tab="purchasing" role="tab" aria-selected="false" tabindex="-1">Purchasing</button>
    </div>
    <section id="settings-section"></section>
    <section id="purchasing-panel" class="hidden"></section>`);
}

const heading = (tab: string): HTMLElement =>
  document.querySelector<HTMLElement>(`#${tab}-tab > .page-heading`)!;

describe('route-change focus (issue #730)', () => {
  beforeEach(() => {
    buildDOM();
    (canAccess as jest.Mock).mockReturnValue(true);
    switchTab('home', { push: false });
  });

  test('sidebar marks only the current page with aria-current="page"', () => {
    switchTab('plans');
    const current = document.querySelectorAll('.tab-btn[aria-current="page"]');
    expect(current.length).toBe(1);
    expect((current[0] as HTMLElement).dataset['tab']).toBe('plans');
    expect(document.querySelector('.tab-btn[aria-selected]')).toBeNull();
  });

  test('focusContent moves focus from the sidebar link to the new page heading', () => {
    const link = document.getElementById('opportunities-tab-btn')!;
    link.focus();
    switchTab('opportunities', { focusContent: true });
    expect(document.activeElement).toBe(heading('opportunities'));
  });

  test('programmatic switches leave focus where it is', () => {
    const inner = document.querySelector<HTMLElement>('#home-tab .inner')!;
    inner.focus();
    switchTab('plans');
    expect(document.activeElement).toBe(inner);
  });

  test('re-activating the current page still moves focus to its heading', () => {
    switchTab('plans', { focusContent: true });
    document.getElementById('plans-tab-btn')!.focus();
    switchTab('plans', { focusContent: true });
    expect(document.activeElement).toBe(heading('plans'));
  });

  test('the no-access placeholder keeps the heading as the focus target', () => {
    (canAccess as jest.Mock).mockReturnValue(false);
    switchTab('purchases', { focusContent: true });
    const page = document.getElementById('purchases-tab')!;
    expect(page.firstElementChild).toBe(heading('purchases'));
    expect(page.querySelector('.empty-state')).not.toBeNull();
    expect(page.querySelector('.inner')).toBeNull();
    expect(document.activeElement).toBe(heading('purchases'));
  });

  test('browser back/forward moves focus to the restored page heading', () => {
    initRouter();
    window.history.replaceState(null, '', '/plans');
    window.dispatchEvent(new PopStateEvent('popstate', { state: null }));
    expect(document.getElementById('plans-tab')?.classList.contains('active')).toBe(true);
    expect(document.activeElement).toBe(heading('plans'));
  });

  test('switching Admin sub-tabs moves the roving tab stop with the selection', () => {
    switchTab('admin', { push: false });
    switchSettingsSubTab('purchasing');
    const [general, purchasing] = Array.from(document.querySelectorAll<HTMLElement>('.settings-tabs [role="tab"]'));
    expect(purchasing!.getAttribute('aria-selected')).toBe('true');
    expect(purchasing!.getAttribute('tabindex')).toBe('0');
    expect(general!.getAttribute('tabindex')).toBe('-1');
  });
});
