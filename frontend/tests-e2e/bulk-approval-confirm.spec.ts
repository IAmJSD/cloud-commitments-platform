import { test, expect } from '@playwright/test';
import { mockApi, RECS, seedAuth } from './fixtures/recs';

const usd = (n: number): string => `$${n.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`;

// Issue #707: the bulk (fan-out) approval confirm stated only the number of
// requests. It now shows the combined upfront, monthly and term totals and
// one line per commitment, like the single-purchase confirm.
test('bulk approval confirm states the totals across every bucket', async ({ page }) => {
  const api = await mockApi(page);
  await seedAuth(page);
  await page.route('**/api/accounts/*/service-overrides', route => route.fulfill({ json: [] }));
  await page.goto('/opportunities');
  const ids = ['r01', 'r08', 'r12'];
  for (const id of ids) {
    await page.locator(`tr.recommendation-row[data-rec-id="${id}"]`)
      .getByRole('checkbox', { name: /^Select / }).check();
  }
  await page.locator('#bulk-purchase-btn').click();
  await expect(page.locator('#fanout-summary')).toBeVisible();
  await page.locator('#execute-purchase-btn').click();

  const picked = RECS.filter(rec => ids.includes(rec.id));
  const upfront = picked.reduce((sum, rec) => sum + rec.upfront_cost, 0);
  const monthly = picked.reduce((sum, rec) => sum + rec.monthly_cost, 0);
  const termTotal = picked.reduce((sum, rec) => sum + rec.upfront_cost + rec.monthly_cost * 12 * rec.term, 0);

  const dialog = page.getByRole('dialog', { name: 'Send 3 bulk purchases for approval?' });
  await expect(dialog).toBeVisible();
  await expect(dialog.locator('.commitment-summary-band')).toContainText(`Upfront on approval${usd(upfront)}`);
  await expect(dialog.locator('.commitment-summary-band')).toContainText(`Monthly${usd(monthly)}`);
  await expect(dialog.locator('.commitment-summary-band')).toContainText(`Total over term${usd(termTotal)}`);
  await expect(dialog.locator('.commitment-summary-lines li')).toHaveCount(3);
  await expect(dialog.getByRole('button', { name: `Send for Approval (${usd(upfront)} upfront on approval)` })).toBeVisible();

  await dialog.getByRole('button', { name: 'Cancel' }).click();
  await expect(dialog).toHaveCount(0);
  expect(api.calls.filter(call => call.method === 'POST' && call.url.includes('/purchases/execute'))).toEqual([]);
});
