/**
 * Issue #756: first-run empty state card shared by Opportunities and Home.
 */
import {
  buildFirstRunEmptyState,
  describeFreshness,
  isScopeFiltered,
  setOpenAccountsHandler,
} from '../first-run-empty-state';
import { isAdmin } from '../permissions';

jest.mock('../permissions', () => ({
  isAdmin: jest.fn().mockReturnValue(true),
}));

const flush = (): Promise<void> => new Promise((r) => setTimeout(r, 0));
const buttons = (el: HTMLElement): string[] =>
  [...el.querySelectorAll('button')].map((b) => b.textContent ?? '');

beforeEach(() => {
  jest.clearAllMocks();
  (isAdmin as jest.Mock).mockReturnValue(true);
  setOpenAccountsHandler(null);
});

describe('isScopeFiltered', () => {
  test.each([
    ['', [], false],
    ['all', [], false],
    ['ALL', [], false],
    ['aws', [], true],
    ['', ['acct-1'], true],
  ] as const)('provider=%p accounts=%p -> %p', (provider, accounts, expected) => {
    expect(isScopeFiltered(provider, accounts)).toBe(expected);
  });
});

describe('describeFreshness', () => {
  test('in-flight collection wins over timestamps', () => {
    expect(describeFreshness({
      last_collected_at: '2026-10-01T10:00:00Z',
      last_collection_error: null,
      last_collection_started_at: '2026-10-01T11:00:00Z',
    })).toBe('A recommendations collection is running now.');
  });

  test('never collected', () => {
    expect(describeFreshness({ last_collected_at: null, last_collection_error: null }))
      .toBe('Recommendations have not been collected yet.');
  });

  test('last collected time plus error', () => {
    const text = describeFreshness({ last_collected_at: '2026-10-01T10:00:00Z', last_collection_error: 'AccessDenied' });
    expect(text).toMatch(/^Last collected Oct 1, 2026, \d\d:\d\d\./);
    expect(text).toContain('The last collection reported errors: AccessDenied');
  });
});

describe('buildFirstRunEmptyState', () => {
  test('no accounts, admin: connect CTA opens Admin > Accounts, no refresh', () => {
    const open = jest.fn();
    setOpenAccountsHandler(open);
    const card = buildFirstRunEmptyState({ hasAccounts: false, scopeFiltered: false, onRefresh: jest.fn() });
    expect(card.classList.contains('empty-state')).toBe(true);
    expect(card.querySelector('h3')?.textContent).toBe('No cloud accounts connected yet');
    expect(card.textContent).toContain('Connect an AWS, Azure or GCP account');
    expect(buttons(card)).toEqual(['Connect a cloud account']);
    card.querySelector('button')!.click();
    expect(open).toHaveBeenCalledTimes(1);
  });

  test('no accounts, non-admin: points at an administrator instead of a button', () => {
    (isAdmin as jest.Mock).mockReturnValue(false);
    setOpenAccountsHandler(jest.fn());
    const card = buildFirstRunEmptyState({ hasAccounts: false, scopeFiltered: false });
    expect(buttons(card)).toEqual([]);
    expect(card.textContent).toContain('Ask an administrator to connect one under Admin > Accounts.');
  });

  test('accounts connected: "No opportunities yet" with refresh and manage accounts', async () => {
    setOpenAccountsHandler(jest.fn());
    let resolve: () => void = () => undefined;
    const onRefresh = jest.fn(() => new Promise<void>((r) => { resolve = r; }));
    const card = buildFirstRunEmptyState({ hasAccounts: true, scopeFiltered: false, onRefresh });
    expect(card.querySelector('h3')?.textContent).toBe('No opportunities yet');
    expect(buttons(card)).toEqual(['Refresh recommendations', 'Manage cloud accounts']);
    const refresh = card.querySelector('button')!;
    refresh.click();
    expect(onRefresh).toHaveBeenCalledTimes(1);
    expect(refresh.disabled).toBe(true);
    resolve();
    await flush();
    expect(refresh.disabled).toBe(false);
  });

  test('unknown account state (fetch failed) does not claim no accounts', () => {
    const card = buildFirstRunEmptyState({ hasAccounts: null, scopeFiltered: false });
    expect(card.querySelector('h3')?.textContent).toBe('No opportunities yet');
  });

  test('topbar scope filter: explains the scope instead of onboarding', () => {
    const card = buildFirstRunEmptyState({ hasAccounts: false, scopeFiltered: true, onRefresh: jest.fn() });
    expect(card.querySelector('h3')?.textContent).toBe('No opportunities for the selected provider or account');
    expect(card.textContent).toContain('All Providers and All Accounts');
    expect(buttons(card)).toEqual(['Refresh recommendations']);
  });

  test('freshness line fills in asynchronously and escapes the error text', async () => {
    const card = buildFirstRunEmptyState({
      hasAccounts: true,
      scopeFiltered: false,
      loadFreshness: () => Promise.resolve({ last_collected_at: null, last_collection_error: '<img src=x>' }),
    });
    const line = card.querySelector<HTMLElement>('.first-run-freshness')!;
    expect(line.hidden).toBe(true);
    await flush();
    expect(line.hidden).toBe(false);
    expect(line.textContent).toContain('<img src=x>');
    expect(card.querySelector('img')).toBeNull();
  });

  test('freshness failure leaves the line hidden', async () => {
    const card = buildFirstRunEmptyState({
      hasAccounts: true,
      scopeFiltered: false,
      loadFreshness: () => Promise.reject(new Error('down')),
    });
    await flush();
    expect(card.querySelector<HTMLElement>('.first-run-freshness')!.hidden).toBe(true);
  });
});
