import { test, expect, type Page } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

// Issue #756: with no accounts and no recommendations, Opportunities and Home
// show an onboarding card pointing at Admin > Accounts instead of "No rows
// match these filters." over an empty 15-column table.

async function firstRun(page: Page): Promise<void> {
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/accounts**', (route) => route.fulfill({ json: [] }));
  await page.route('**/api/recommendations*', (route) =>
    route.fulfill({ json: { recommendations: [], summary: {}, regions: [] } }));
  await page.route('**/api/recommendations/freshness', (route) =>
    route.fulfill({ json: { last_collected_at: new Date().toISOString(), last_collection_error: null } }));
  await page.route('**/api/dashboard/upcoming', (route) => route.fulfill({ json: { purchases: [] } }));
  await page.route('**/api/dashboard/summary?*', (route) => route.fulfill({ json: {
    total_recommendations: 0, active_commitments: 0, committed_monthly: 0, ytd_savings: 0, by_service: {},
  } }));
}

test('Opportunities first run: onboarding card links to Admin > Accounts', async ({ page }) => {
  await firstRun(page);
  await page.goto('/opportunities');
  const card = page.locator('#recommendations-list .first-run-empty-state');
  await expect(card).toBeVisible();
  await expect(card.locator('h3')).toHaveText('No cloud accounts connected yet');
  await expect(page.locator('#recommendations-list table')).toHaveCount(0);
  await expect(page.getByText('No rows match these filters.')).toHaveCount(0);
  await expect(page.locator('#recommendations-summary')).toBeHidden();
  await expect(page.locator('#recommendations-action-box')).toBeHidden();

  await card.getByRole('button', { name: 'Connect a cloud account' }).click();
  await expect(page).toHaveURL(/\/admin\/accounts$/);
  await expect(page.locator('#accounts-section')).toBeVisible();
});

test('Home first run: onboarding card above the KPI tiles', async ({ page }) => {
  await firstRun(page);
  await page.goto('/home');
  const card = page.locator('#home-onboarding .first-run-empty-state');
  await expect(card).toBeVisible();
  await expect(card.getByRole('button', { name: 'Connect a cloud account' })).toBeVisible();
});

test('filtered to zero keeps the table hint and offers Clear filters', async ({ page }) => {
  await seedAuth(page);
  await mockApi(page);
  await page.addInitScript(() => {
    localStorage.setItem('cudly.recs.columnFilters.v1', JSON.stringify({
      schemaVersion: 1, filters: { count: { kind: 'expr', expr: '>100000' } },
    }));
  });
  await page.goto('/opportunities');
  const hint = page.locator('#recommendations-list tbody td.empty');
  await expect(hint).toContainText('No rows match these filters.');
  await expect(page.locator('.first-run-empty-state')).toHaveCount(0);
  await hint.getByRole('button', { name: 'Clear filters' }).click();
  await expect(page.locator('#recommendations-list tbody td.empty')).toHaveCount(0);
  await expect(page.locator('#recommendations-list tbody tr').first()).toBeVisible();
});
