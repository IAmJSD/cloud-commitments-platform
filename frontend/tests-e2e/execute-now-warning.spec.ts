import { test, expect } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

for (const sample of [
  { provider: 'aws', service: 'ec2', cloud_account_id: 'acct-001', resource_type: 'm5.large', region: 'us-east-1', payment: 'all-upfront', upfront_cost: 1200 },
  { provider: 'azure', service: 'compute', cloud_account_id: 'acct-100', resource_type: 'Standard_D2_v3', region: 'eastus', payment: 'all-upfront', upfront_cost: 2400 },
  { provider: 'gcp', service: 'gce', cloud_account_id: 'acct-200', resource_type: 'n2-standard-2', region: 'us-central1', payment: 'monthly', upfront_cost: 0 },
]) {
  test(`${sample.provider} Execute Now warns without promising cancellation`, async ({ page }) => {
    const errors: string[] = [];
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error') errors.push(message.text()); });
    page.on('response', response => { if (response.status() >= 400) errors.push(`${response.status()} ${response.url()}`); });
    const api = await mockApi(page);
    await seedAuth(page);
    const row = { ...sample, id: `warning-${sample.provider}`, term: 1, count: 1, monthly_cost: sample.upfront_cost ? 0 : 240, savings: 100, on_demand_monthly: 340, effective_savings_pct: 29 };
    await page.route(/\/api\/recommendations(?:\?.*)?$/, route => route.fulfill({ json: {
      recommendations: [row], summary: { total_recommendations: 1, total_upfront_cost: row.upfront_cost, potential_monthly_savings: 100, avg_payback_months: 6 }, regions: [row.region],
    } }));
    await page.route('**/api/accounts/*/service-overrides', route => route.fulfill({ json: [] }));
    await page.goto('/opportunities');
    const selected = page.locator(`tr.recommendation-row[data-rec-id="${row.id}"]`);
    await expect(selected).toBeVisible();
    await selected.getByRole('checkbox', { name: /^Select / }).check();
    await page.locator('#bulk-purchase-btn').click();
    await expect(page.locator('#purchase-details')).toContainText(row.resource_type);
    const warning = page.locator('#purchase-details .direct-execute-warning');
    await expect(warning).toBeHidden();
    await page.locator('#execute-mode-direct').check();
    await expect(warning).toBeVisible();
    await expect(warning).toHaveText(`Warning: This will charge $${row.upfront_cost.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 })} upfront immediately. This bypasses the approval step.`);
    await expect(warning).not.toContainText(/AWS|cancell|24 hours/);
    await expect(page.locator('#execute-purchase-btn')).toHaveText('Execute Purchase Now');
    await page.getByRole('checkbox', { name: 'Include row 1' }).uncheck();
    await expect(warning).toHaveText('Warning: This will charge $0.00 upfront immediately. This bypasses the approval step.');
    await expect(page.locator('#execute-purchase-btn')).toBeDisabled();
    await page.getByRole('checkbox', { name: 'Include row 1' }).check();
    await expect(warning).toContainText(`$${row.upfront_cost.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 })} upfront immediately.`);
    await expect(page.locator('#execute-purchase-btn')).toBeEnabled();
    if (sample.provider === 'gcp') {
      await page.setViewportSize({ width: 390, height: 844 });
      await expect(warning).toBeInViewport();
      const box = await warning.boundingBox();
      expect(box).not.toBeNull();
      expect(box!.x).toBeGreaterThanOrEqual(0);
      expect(box!.x + box!.width).toBeLessThanOrEqual(390);
    }
    await page.locator('#execute-mode-approval').check();
    await expect(warning).toBeHidden();
    await expect(page.locator('#execute-purchase-btn')).toHaveText('Send for Approval');
    await page.locator('#execute-mode-direct').check();
    await page.locator('#close-purchase-modal-btn').click();
    await page.locator('#bulk-purchase-btn').click();
    await expect(page.locator('#execute-mode-approval')).toBeChecked();
    await expect(warning).toBeHidden();
    expect(api.calls.filter(call => call.method === 'POST' && call.url.includes('/purchases/execute'))).toEqual([]);
    expect(errors).toEqual([]);
  });
}

test('mixed providers use fan-out without cancellation reassurance', async ({ page }) => {
  const api = await mockApi(page);
  await seedAuth(page);
  await page.route('**/api/accounts/*/service-overrides', route => route.fulfill({ json: [] }));
  await page.goto('/opportunities');
  for (const id of ['r01', 'r12']) {
    await page.locator(`tr.recommendation-row[data-rec-id="${id}"]`)
      .getByRole('checkbox', { name: /^Select / }).check();
  }
  await page.locator('#bulk-purchase-btn').click();
  await expect(page.locator('#fanout-summary')).toBeVisible();
  await expect(page.locator('#purchase-details')).not.toContainText(/cancellation|24 hours/);
  await expect(page.locator('#execute-mode-direct')).toHaveCount(0);
  expect(api.calls.filter(call => call.method === 'POST' && call.url.includes('/purchases/execute'))).toEqual([]);
});
