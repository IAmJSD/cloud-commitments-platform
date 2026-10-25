import { test, expect, type Page } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

// Issue #734: the account delete confirms name the account, the cascade
// confirm lists the pending purchases it would cancel, and a finished
// delete ends with a success toast naming the account.

const EXEC_1 = '11111111-1111-1111-1111-111111111111';
const EXEC_2 = '22222222-2222-2222-2222-222222222222';
const DETAILS: Record<string, unknown> = {
  [EXEC_1]: { execution_id: EXEC_1, status: 'pending', plan_name: 'Weekly EC2', step_number: 2, scheduled_date: '2026-11-02',
    total_upfront_cost: 1200, estimated_savings: 40, recommendations: [{}, {}] },
  [EXEC_2]: { execution_id: EXEC_2, status: 'notified', plan_name: 'RDS ramp', step_number: 1, scheduled_date: '2026-11-09',
    total_upfront_cost: 300, estimated_savings: 12, recommendations: [{}] },
};

async function accountsPage(page: Page, pending: boolean): Promise<{ deletes: string[] }> {
  await seedAuth(page);
  await mockApi(page);
  const deletes: string[] = [];
  let cancelled = 0;
  await page.route('**/api/accounts/**', async (route) => {
    if (route.request().method() !== 'DELETE') return route.fallback();
    deletes.push(route.request().url());
    if (pending && cancelled < 2) {
      return route.fulfill({ status: 409, json: {
        error: 'cannot delete account: 2 pending purchase(s) must be canceled first',
        pending_count: 2, pending_execution_ids: [EXEC_1, EXEC_2], reason: 'pending_executions',
      } });
    }
    return route.fulfill({ status: 204, body: '' });
  });
  await page.route('**/api/purchases/**', async (route) => {
    const url = route.request().url();
    if (route.request().method() === 'POST' && url.includes('/purchases/cancel/')) {
      cancelled += 1;
      return route.fulfill({ json: {} });
    }
    const id = url.split('/').pop() ?? '';
    return DETAILS[id] ? route.fulfill({ json: DETAILS[id] }) : route.fulfill({ status: 404, json: { error: 'not found' } });
  });
  await page.goto('/admin/accounts');
  await page.waitForLoadState('networkidle');
  return { deletes };
}

test('delete confirm names the account and a plain delete toasts success', async ({ page }) => {
  const { deletes } = await accountsPage(page, false);
  await page.getByRole('group', { name: 'AWS Accounts' }).getByRole('button', { name: 'Delete AWS Prod (acct-001) (111111111111)' }).click();

  const dialog = page.getByRole('dialog');
  await expect(dialog.getByRole('heading')).toHaveText('Delete account "AWS Prod (acct-001)" (111111111111)?');
  await dialog.getByRole('button', { name: 'Delete account' }).click();

  await expect(page.getByText('Deleted account "AWS Prod (acct-001)" (111111111111).')).toBeVisible();
  expect(deletes).toHaveLength(1);
});

test('cascade confirm lists the pending purchases before cancelling them', async ({ page }) => {
  const { deletes } = await accountsPage(page, true);
  await page.getByRole('group', { name: 'AWS Accounts' }).getByRole('button', { name: 'Delete AWS Prod (acct-001) (111111111111)' }).click();
  await page.getByRole('dialog').getByRole('button', { name: 'Delete account' }).click();

  const dialog = page.getByRole('dialog');
  await expect(dialog.getByRole('heading')).toHaveText('Cancel pending purchases and delete "AWS Prod (acct-001)" (111111111111)?');
  await expect(dialog.getByRole('listitem')).toHaveText([
    'Weekly EC2, step 2 · 2 commitments · scheduled Nov 2, 2026 · $1,200 upfront',
    'RDS ramp, step 1 · 1 commitment · scheduled Nov 9, 2026 · $300 upfront',
  ]);
  await expect(dialog).toContainText('Total upfront cost cancelled: $1,500');

  await dialog.getByRole('button', { name: 'Cancel All 2 Pending + Delete' }).click();
  await expect(page.getByText('Cancelled 2 pending purchases and deleted account "AWS Prod (acct-001)" (111111111111).')).toBeVisible();
  expect(deletes).toHaveLength(2);
});
