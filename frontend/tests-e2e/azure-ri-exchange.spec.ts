import { expect, test, type Page } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

// Azure RI exchange: select reservations, preview, then execute behind the
// max_payment_due + currency guardrail (issue #81). All Azure calls are mocked.
const SUB = '11111111-2222-3333-4444-555555555555';
const reservations = [
  { reservation_order_id: 'o1', reservation_id: '/providers/Microsoft.Capacity/reservationOrders/o1/reservations/r1', sku: 'Standard_D2s_v3', quantity: 4,
    region: 'eastus', term: 'P3Y', expiry_date: '2028-01-01T00:00:00Z', instance_flexibility: 'On', display_name: 'web-tier', billing_scope_id: `/subscriptions/${SUB}` },
  { reservation_order_id: 'o2', reservation_id: '/providers/Microsoft.Capacity/reservationOrders/o2/reservations/r2', sku: 'Standard_E4s_v3', quantity: 2,
    region: 'eastus', term: 'P1Y', expiry_date: '2027-03-01T00:00:00Z', instance_flexibility: 'On', display_name: 'batch', billing_scope_id: `/subscriptions/${SUB}` },
];
const preview = {
  offerings: [{ sku: 'Standard_D4s_v5', location: 'eastus', term: 'P3Y', quantity: 2, billing_currency_total: 900.5, currency_code: 'USD' }],
  preview: { session_id: 's1', net_payable: 120.25, net_payable_currency: 'USD', refunds_total: 780.25, purchases_total: 900.5 },
};

async function setup(page: Page, executeStatus = 200) {
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/auth/me/permissions', route => route.fulfill({ json: {
    permissions: [{ action: 'admin', resource: '*' }, { action: 'execute', resource: 'ri-exchange' }], is_admin: true,
  } }));
  const posts: { path: string; body: unknown }[] = [];
  await page.route('**/api/ri-exchange/**', async route => {
    const path = new URL(route.request().url()).pathname;
    if (route.request().method() === 'POST') {
      posts.push({ path, body: route.request().postDataJSON() });
      if (path.endsWith('/compatible-offerings')) return route.fulfill({ json: preview });
      if (executeStatus !== 200) {
        return route.fulfill({ status: executeStatus, json: { error: 'quoted net payable 160.00 USD exceeds max_payment_due 150.00 USD' } });
      }
      return route.fulfill({ json: { session_id: 's2', status: 'Succeeded', net_payable: 118, net_payable_currency: 'USD', refunds_total: 780.25, purchases_total: 898.25 } });
    }
    if (path.endsWith('/azure-instances')) return route.fulfill({ json: { reservations } });
    if (path.endsWith('/history')) return route.fulfill({ json: { records: [] } });
    return route.fulfill({ json: {} });
  });
  await page.goto('/inventory/ri-exchange?provider=azure');
  await expect(page.locator('#ri-exchange-instances-list')).toContainText('web-tier');
  return posts;
}

async function previewFirst(page: Page) {
  await page.getByRole('checkbox', { name: 'Select web-tier for exchange' }).check();
  await page.locator('#azure-exchange-selected-btn').click();
  const modal = page.locator('#ri-exchange-modal');
  await modal.locator('.azure-exchange-target-sku').fill('Standard_D4s_v5');
  await modal.locator('.azure-exchange-target-qty').fill('2');
  await modal.getByRole('button', { name: 'Preview exchange' }).click();
  await expect(modal.locator('#azure-exchange-result')).toContainText('Net payable:USD 120.25');
  return modal;
}

test('previews and executes an Azure exchange with an explicit spending limit', async ({ page }) => {
  const posts = await setup(page);
  const modal = await previewFirst(page);
  expect(posts[0]).toEqual({ path: '/api/ri-exchange/azure-instances/compatible-offerings', body: {
    subscription_id: SUB,
    sources: [{ reservation_id: reservations[0]!.reservation_id, quantity: 4 }],
    targets: [{ sku: 'Standard_D4s_v5', location: 'eastus', term: 'P3Y', quantity: 2 }],
  } });

  await modal.getByRole('button', { name: 'Execute exchange' }).click();
  await expect(modal.locator('.azure-exchange-guard-error')).toContainText('maximum you are willing to pay');
  await modal.locator('#azure-exchange-max-payment').fill('150');
  await modal.locator('#azure-exchange-currency').fill('usd');
  await modal.getByRole('button', { name: 'Execute exchange' }).click();
  const confirm = page.locator('.confirm-dialog, [role="alertdialog"], [role="dialog"]').filter({ hasText: 'cannot be reversed' }).last();
  await confirm.getByRole('button', { name: 'Execute exchange' }).click();
  await expect(modal.locator('#azure-exchange-status')).toContainText('Exchange Succeeded. Net payable USD 118.00');
  expect(posts[1]!.path).toBe('/api/ri-exchange/azure-instances/exchange');
  expect(posts[1]!.body).toMatchObject({ subscription_id: SUB, max_payment_due: '150', currency: 'USD' });
});

test('explains a guardrail refusal instead of a generic error', async ({ page }) => {
  await setup(page, 422);
  const modal = await previewFirst(page);
  await modal.locator('#azure-exchange-max-payment').fill('150');
  await modal.locator('#azure-exchange-currency').fill('USD');
  await modal.getByRole('button', { name: 'Execute exchange' }).click();
  const confirm = page.locator('.confirm-dialog, [role="alertdialog"], [role="dialog"]').filter({ hasText: 'cannot be reversed' }).last();
  await confirm.getByRole('button', { name: 'Execute exchange' }).click();
  await expect(modal.locator('#azure-exchange-status')).toHaveText(
    'Exchange blocked by a safety check: quoted net payable 160.00 USD exceeds max_payment_due 150.00 USD. Raise the maximum payment or change the targets, then preview again.',
  );
});
