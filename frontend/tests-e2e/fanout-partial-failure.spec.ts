import { test, expect, type Page } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

// Issue #757: when some buckets of a bulk purchase fail, the modal stays open
// with per-bucket status, and "Retry N failed" resubmits only those buckets.

async function openFanOut(page: Page): Promise<void> {
  await page.route('**/api/accounts/*/service-overrides', (route) => route.fulfill({ json: [] }));
  await page.goto('/opportunities');
  for (const id of ['r01', 'r12', 'r16']) {
    await page.locator(`tr.recommendation-row[data-rec-id="${id}"]`)
      .getByRole('checkbox', { name: /^Select / }).check();
  }
  await page.locator('#bulk-purchase-btn').click();
  await expect(page.locator('#fanout-summary')).toBeVisible();
  await expect(page.locator('section.fanout-bucket')).toHaveCount(3);
}

async function send(page: Page, label: RegExp | string): Promise<void> {
  await page.locator('#execute-purchase-btn').filter({ hasText: label }).click();
  await page.locator('.modal-confirm-backdrop').getByRole('button', { name: 'Send all for approval' }).click();
}

test('partial failure keeps the modal open and retries only the failed bucket', async ({ page }) => {
  await seedAuth(page);
  await mockApi(page);
  const posted: string[] = [];
  let azureAttempts = 0;
  await page.route('**/api/purchases/execute', async (route) => {
    const body = JSON.parse(route.request().postData() ?? '{}') as { recommendations: Array<{ provider: string }> };
    const provider = body.recommendations[0]!.provider;
    posted.push(provider);
    if (provider === 'azure' && azureAttempts++ === 0) {
      await route.fulfill({ status: 502, json: { error: 'Azure reservation API unavailable' } });
      return;
    }
    await route.fulfill({ json: { execution_id: `exec-${provider}-${posted.length}`, status: 'pending', email_sent: true } });
  });

  await openFanOut(page);
  await send(page, 'Send for Approval');

  const modal = page.getByRole('dialog', { name: 'Review Purchase' });
  await expect(modal).toBeVisible();
  await expect(page.locator('#fanout-result')).toContainText('2 buckets submitted, 1 failed.');
  const failed = page.locator('section.fanout-bucket.fanout-bucket-failed');
  await expect(failed).toHaveCount(1);
  await expect(failed).toContainText('AZURE');
  await expect(failed.locator('.fanout-bucket-result')).toContainText('Failed:');
  await expect(page.locator('section.fanout-bucket.fanout-bucket-submitted')).toHaveCount(2);
  await expect(page.locator('section.fanout-bucket-submitted select').first()).toBeDisabled();
  await expect(page.locator('#execute-purchase-btn')).toHaveText('Retry 1 failed');
  expect([...posted].sort()).toEqual(['aws', 'azure', 'gcp']);

  await send(page, 'Retry 1 failed');
  await expect(modal).toBeHidden();
  expect(posted.slice(3)).toEqual(['azure']);
});
