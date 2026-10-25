import { test, expect, type Page } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

// Issue #18: the Plans table's column-filter popover now runs on the shared
// lib/column-filter-popover.ts. Exercise its lifecycle in a real browser.

function planned(id: string, provider: 'aws' | 'azure', count: number) {
  return {
    id, plan_id: 'plan-1', plan_name: 'Ramp', scheduled_date: '2026-11-01T00:00:00Z',
    provider, service: 'ec2', resource_type: 'm5.large', region: 'us-east-1', count, term: 1,
    payment: 'all-upfront', estimated_savings: 10, upfront_cost: 100, has_recommendations: true,
    status: 'pending', step_number: 1, total_steps: 2, created_by_user_id: 'user-smoke',
  };
}

async function openPlans(page: Page): Promise<void> {
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/purchases/planned', route => route.fulfill({ json: { purchases: [
    planned('pp-1', 'aws', 1), planned('pp-2', 'azure', 5),
  ] } }));
  await page.goto('/plans');
  await expect(page.locator('#planned-purchases-list tbody tr')).toHaveCount(2);
}

const rows = (page: Page) => page.locator('#planned-purchases-list tbody tr');
const trigger = (page: Page, column: string) =>
  page.locator(`#planned-purchases-list th .column-filter-btn[data-column="${column}"]`);

test('categorical filter narrows rows, stays open across re-render, Escape restores focus', async ({ page }) => {
  await openPlans(page);
  await trigger(page, 'provider').click();
  const dialog = page.getByRole('dialog', { name: 'Filter Provider' });
  await expect(dialog).toBeVisible();

  await dialog.getByRole('checkbox', { name: 'AZURE' }).uncheck();
  await expect(rows(page)).toHaveCount(1);
  await expect(dialog).toBeVisible();
  await expect(trigger(page, 'provider')).toHaveAttribute('aria-expanded', 'true');

  await page.keyboard.press('Escape');
  await expect(dialog).toHaveCount(0);
  await expect(trigger(page, 'provider')).toBeFocused();
  await expect(trigger(page, 'provider')).toHaveAttribute('aria-expanded', 'false');
});

test('numeric filter shows inline errors, commits valid expressions, and closes on outside click', async ({ page }) => {
  await openPlans(page);
  await trigger(page, 'count').click();
  const dialog = page.getByRole('dialog', { name: 'Filter Count' });
  const input = dialog.getByLabel('Expression');
  await input.fill('>>');
  await input.press('Enter');
  await expect(dialog.locator('.column-filter-error')).not.toBeEmpty();
  await expect(rows(page)).toHaveCount(2);

  await input.fill('>2');
  await input.press('Enter');
  await expect(rows(page)).toHaveCount(1);
  await expect(dialog.locator('.column-filter-error')).toBeEmpty();

  await page.locator('#planned-purchases-list').click({ position: { x: 2, y: 2 } });
  await expect(dialog).toHaveCount(0);
});
