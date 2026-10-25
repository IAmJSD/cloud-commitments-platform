import { test, expect, type Page } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

// Issue #281: the Approval Queue and Purchase History tables render filter
// buttons with the same bare column ids. Opening one table's Provider filter
// and then the other's must swap popovers (not just close), and Escape must
// return focus to the button that opened the popover.

// Tall enough that both tables are on screen: clicking a button below the
// fold scrolls, and the scroll-close handler would mask the collision.
test.use({ viewport: { width: 1400, height: 1700 } });

function row(id: string, status: string, provider: string) {
  return {
    purchase_id: id, timestamp: new Date().toISOString(), provider,
    service: 'ec2', resource_type: 'm5.large', region: 'us-east-1', count: 1, term: 1,
    upfront_cost: 1200, monthly_cost: 50, estimated_savings: 10, offering_class: 'standard',
    status, account_id: 'acct-001', created_by_user_id: 'user-smoke',
  };
}

async function setup(page: Page): Promise<void> {
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/history**', route => route.fulfill({ json: { summary: {}, purchases: [
    row('11111111-1111-1111-1111-111111111111', 'pending', 'aws'),
    row('22222222-2222-2222-2222-222222222222', 'completed', 'azure'),
  ] } }));
  await page.goto('/purchases/history');
}

const queueBtn = (page: Page) =>
  page.locator('#purchases-approval-queue .history-column-filter-btn[data-column="provider"]');
const historyBtn = (page: Page) =>
  page.locator('#history-list .history-column-filter-btn[data-column="provider"]');

test('opening the other table\'s filter on the same column swaps the popover', async ({ page }) => {
  await setup(page);
  await queueBtn(page).click();
  await expect(page.locator('.column-filter-popover')).toHaveCount(1);
  await expect(queueBtn(page)).toHaveAttribute('aria-expanded', 'true');

  await historyBtn(page).click();
  await expect(page.locator('.column-filter-popover')).toHaveCount(1);
  await expect(historyBtn(page)).toHaveAttribute('aria-expanded', 'true');
  await expect(queueBtn(page)).toHaveAttribute('aria-expanded', 'false');
});

test('Escape returns focus to the button that opened the popover', async ({ page }) => {
  await setup(page);
  await historyBtn(page).click();
  await expect(page.locator('.column-filter-popover')).toHaveCount(1);
  await page.keyboard.press('Escape');
  await expect(page.locator('.column-filter-popover')).toHaveCount(0);
  await expect(historyBtn(page)).toBeFocused();
  await expect(historyBtn(page)).toHaveAttribute('aria-expanded', 'false');
  await expect(queueBtn(page)).toHaveAttribute('aria-expanded', 'false');
});
