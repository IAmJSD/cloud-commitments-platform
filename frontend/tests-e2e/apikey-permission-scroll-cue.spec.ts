import { test, expect, type Page } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

const RESOURCES = [
  'recommendations', 'plans', 'purchases', 'accounts', 'users', 'groups', 'api-keys',
  'config', 'history', 'ladder', 'coverage', 'exchanges', 'reports', 'budgets', 'audit',
];

async function openCreateKey(page: Page): Promise<void> {
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/users*', route => route.fulfill({ json: { users: [] } }));
  await page.route('**/api/groups*', route => route.fulfill({ json: { groups: [] } }));
  await page.route('**/api/api-keys/usage-stats', route => route.fulfill({ json: {
    total_active: 0, total_requests_window: 0, total_requests_lifetime: 0, lifetime_partial: false, top_keys: [],
  } }));
  await page.route('**/api/api-keys', route => route.fulfill({ json: { api_keys: [] } }));
  await page.route('**/api/auth/me/permissions', route => route.fulfill({ json: {
    permissions: RESOURCES.map(resource => ({ action: 'view', resource })), is_admin: true,
  } }));
  await page.goto('/admin/users');
  await page.locator('#create-apikey-btn').click();
  await expect(page.locator('#apikey-permissions-list input[type="checkbox"]')).toHaveCount(RESOURCES.length);
}

// The permission box is a fixed-height scroll area. If its height lands exactly
// on a row boundary, overlay scrollbars give no cue that more rows exist, so a
// user can miss later permissions. Some row must be cut by the bottom edge.
for (const viewport of [{ width: 1440, height: 900 }, { width: 390, height: 844 }]) {
  test(`API key permission list shows a partly clipped row at ${viewport.width}x${viewport.height}`, async ({ page }) => {
    await page.setViewportSize(viewport);
    await openCreateKey(page);

    const m = await page.evaluate(() => {
      const list = document.getElementById('apikey-permissions-list')!;
      list.scrollTop = 0;
      const box = list.getBoundingClientRect();
      const edge = box.bottom - parseFloat(getComputedStyle(list).borderBottomWidth);
      const rows = Array.from(list.querySelectorAll('.apikey-permission-option')).map(r => r.getBoundingClientRect());
      return {
        overflows: list.scrollHeight > list.clientHeight,
        clipped: rows.filter(r => r.top < edge - 1 && r.bottom > edge + 1).length,
        visible: rows.filter(r => r.bottom <= edge + 1).length,
      };
    });
    expect(m.overflows).toBe(true);
    expect(m.visible).toBeGreaterThan(0);
    expect(m.clipped).toBeGreaterThanOrEqual(1);
  });
}
