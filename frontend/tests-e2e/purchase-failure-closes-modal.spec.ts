import { test, expect, type Page } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

// Issue #1694: after a failed execute that a purchase may have followed, the
// review modal must close so one more click cannot start a second attempt.
// Runs the real recommendations module, whose selection getter returns a new
// array on every call.
const row = {
  id: 'refused-ec2', provider: 'aws', service: 'ec2', cloud_account_id: 'acct-001',
  resource_type: 'm5.large', region: 'us-east-1', payment: 'no-upfront', term: 1, count: 1,
  upfront_cost: 0, monthly_cost: 40, savings: 20, on_demand_monthly: 60, effective_savings_pct: 33,
};

async function executeWith(page: Page, status: number, body: Record<string, unknown>) {
  await mockApi(page);
  await seedAuth(page);
  await page.route(/\/api\/recommendations(?:\?.*)?$/, route => route.fulfill({ json: {
    recommendations: [row], summary: { total_recommendations: 1, total_upfront_cost: 0, potential_monthly_savings: 20, avg_payback_months: 0 }, regions: [row.region],
  } }));
  await page.route('**/api/accounts/*/service-overrides', route => route.fulfill({ json: [] }));
  await page.route('**/api/purchases/execute', route => route.fulfill({ status, json: body }));
  await page.goto('/opportunities');
  const selected = page.locator(`tr.recommendation-row[data-rec-id="${row.id}"]`);
  await selected.getByRole('checkbox', { name: /^Select / }).check();
  await page.locator('#bulk-purchase-btn').click();
  await page.locator('#execute-mode-direct').check();
  await page.locator('#execute-purchase-btn').click();
  const confirm = page.locator('.confirm-dialog, [role="alertdialog"], [role="dialog"]').filter({ hasText: 'bypasses the approval step' }).last();
  await confirm.getByRole('button').filter({ hasNotText: /cancel/i }).last().click();
}

for (const [name, status, body] of [
  ['a recorded failure (502 with execution_id)', 502, { error: 'execution x failed', execution_id: 'exec-1', status: 'failed' }],
  ['a refusal recorded as an execution (403 with execution_id)', 403, { error: 'execution x failed', execution_id: 'exec-2', status: 'failed' }],
  ['an audit-loss 500 without execution_id', 500, { error: 'execution x ran but its final status could not be saved' }],
  ['a 409 after the claim', 409, { error: 'pricing changed' }],
] as const) {
  test(`${name} closes the review modal`, async ({ page }) => {
    await executeWith(page, status, body);
    await expect(page.locator('.toast').filter({ hasText: 'Failed to execute purchase' })).toBeVisible();
    await expect(page.locator('#purchase-modal')).toBeHidden();
  });
}

test('a 400 validation refusal keeps the review modal open', async ({ page }) => {
  await executeWith(page, 400, { error: 'capacity must be between 1 and 100' });
  await expect(page.locator('.toast').filter({ hasText: 'Failed to execute purchase' })).toBeVisible();
  await expect(page.locator('#purchase-modal')).toBeVisible();
});
