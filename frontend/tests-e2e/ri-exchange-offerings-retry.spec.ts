import { expect, test } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

// Issue #740: a failed target-offerings load used to say "type a UUID" with
// no field to type into. It now shows an error with a Retry button.
const ri = '11111111-1111-1111-1111-111111111111';
const offering = '22222222-2222-2222-2222-222222222222';

test('a failed offerings load offers Retry and recovers the picker', async ({ page }) => {
  await seedAuth(page);
  await mockApi(page);
  let failOfferings = true;
  await page.route('**/api/auth/me/permissions', route => route.fulfill({ json: {
    permissions: [{ action: 'admin', resource: '*' }, { action: 'execute', resource: 'ri-exchange' }],
  } }));
  await page.route('**/api/ri-exchange/**', async route => {
    const end = new URL(route.request().url()).pathname.split('/').pop()!;
    if (end === 'target-offerings') {
      await route.fulfill(failOfferings
        ? { status: 500, json: { error: 'offerings unavailable' } }
        : { json: { offerings: [{ offering_id: offering, instance_type: 'm5.xlarge', offering_type: 'No Upfront' }] } });
      return;
    }
    const responses: Record<string, unknown> = {
      instances: { instances: [{ reserved_instance_id: ri, instance_type: 'm5.large', instance_count: 2,
        region: 'us-east-1', state: 'active', offering_class: 'convertible', offering_type: 'No Upfront',
        product_description: 'Linux/UNIX', start: '2026-01-01T00:00:00Z', end: '2027-01-01T00:00:00Z' }] },
      utilization: { utilization: [] }, 'reshape-recommendations': { recommendations: [] }, history: { records: [] },
    };
    await route.fulfill({ json: responses[end] ?? {} });
  });
  await page.goto('/inventory/ri-exchange');
  await page.locator(`[data-action="quote-ri"][data-ri-id="${ri}"]`).click();

  const modal = page.locator('#ri-exchange-modal');
  await expect(modal.getByRole('alert')).toContainText('Could not load target offerings');
  await expect(modal).not.toContainText('UUID');
  await expect(modal.locator('.modal-exchange-target-select option').first()).toHaveText('Could not load offerings');

  failOfferings = false;
  await modal.getByRole('button', { name: 'Retry', exact: true }).click();
  await expect(modal.locator('.exchange-offerings-error')).toBeHidden();
  await modal.locator('.modal-exchange-target-select').selectOption(offering);
  await expect(modal.locator('.modal-exchange-target')).toHaveValue(offering);
});
