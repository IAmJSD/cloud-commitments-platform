import { test, expect } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

// Issue #742: the toast stack moves only when its default bottom-right corner
// would cover an open dialog's button row. A short .modal-wide dialog is
// centred with its buttons mid-screen, so an error toast stays in the corner
// instead of moving onto the dialog's content.
const OVERRIDES = [
  { id: 'o1', account_id: 'acct-001', provider: 'aws', service: 'ec2', enabled: true, term: 3, payment: 'no-upfront', coverage: 70 },
];

test('an error toast stays off a short centred .modal-wide dialog at 1280x720', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 720 });
  await seedAuth(page); await mockApi(page);
  await page.route('**/api/accounts/*/service-overrides', route => route.fulfill({ json: OVERRIDES }));
  await page.route('**/api/accounts/*/service-overrides/*/*', route => route.request().method() === 'DELETE'
    ? route.fulfill({ status: 500, json: { error: 'database unavailable' } })
    : route.fallback());
  await page.goto('/admin/accounts');
  await page.locator('#aws-accounts-list button', { hasText: 'Overrides' }).first().click();
  const dialog = page.locator('#account-overrides-modal .modal-content');
  await expect(dialog.locator('table.overrides-table tbody tr')).toHaveCount(1);

  await dialog.getByRole('button', { name: 'Delete' }).click();
  await page.getByRole('dialog', { name: 'Delete override?' }).getByRole('button', { name: 'Delete override' }).click();
  const toast = page.getByRole('alert').filter({ hasText: 'Failed to delete override' });
  await expect(toast).toBeVisible();
  await expect(page.getByRole('dialog', { name: 'Delete override?' })).toHaveCount(0);

  // Let the layout pass queued by the confirm closing run.
  await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  const t = (await toast.boundingBox())!;
  const d = (await dialog.boundingBox())!;
  const overlaps = t.x < d.x + d.width && d.x < t.x + t.width && t.y < d.y + d.height && d.y < t.y + t.height;
  expect(overlaps, `toast ${JSON.stringify(t)} covers the dialog ${JSON.stringify(d)}`).toBe(false);
  // The dialog's own button still works with the toast up.
  await page.locator('#close-account-overrides-modal-btn').click({ timeout: 5000 });
  await expect(page.locator('#account-overrides-modal')).toBeHidden();
});
