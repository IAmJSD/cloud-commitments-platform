import { expect, test, type Page } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

const ri = '11111111-1111-1111-1111-111111111111';
const otherRI = '55555555-5555-5555-5555-555555555555';
const offering = '22222222-2222-2222-2222-222222222222';
const secondOffering = '44444444-4444-4444-4444-444444444444';
const quotedPayment = '123.456789012345678901';
const browserErrors = new WeakMap<Page, string[]>();

test.afterEach(({ page }) => { expect(browserErrors.get(page)).toEqual([]); });

async function setup(page: Page) {
  await seedAuth(page);
  await mockApi(page);
  const history = {
    id: '33333333-3333-3333-3333-333333333333', account_id: 'acct-001',
    source_ri_ids: [ri], source_instance_type: 'm5.large', source_count: 2,
    target_offering_id: offering, target_instance_type: 'm5.xlarge', target_count: 1,
    payment_due: '123.456789', status: 'pending', mode: 'manual', region: 'us-east-1',
    created_at: new Date().toISOString(), updated_at: new Date().toISOString(),
  };
  const quote = {
    IsValidExchange: true, CurrencyCode: 'USD', PaymentDueRaw: quotedPayment,
    Region: 'us-east-1', SourceHourlyPriceRaw: '0.10', TargetHourlyPriceRaw: '0.08',
  };
  const requests: { path: string; body: unknown }[] = [];
  let exchanged = false;
  const control = { quoteDelay: Promise.resolve(), executeDelay: Promise.resolve(), failQuote: false, failExecute: false, failApprove: false, quoteCalls: 0 };
  const errors: string[] = [];
  browserErrors.set(page, errors);
  page.on('pageerror', error => errors.push(error.message));
  page.on('requestfailed', request => errors.push(`${request.method()} ${request.url()}: ${request.failure()?.errorText}`));
  page.on('console', message => {
    if (message.type() === 'error' && !message.text().includes('the server responded with a status of 500')) errors.push(message.text());
  });
  page.on('response', response => {
    if (response.status() < 400) return;
    const path = new URL(response.url()).pathname;
    const expected = response.status() === 500 && (
      (path === '/api/ri-exchange/quote' && control.failQuote)
      || (path === '/api/ri-exchange/execute' && control.failExecute)
      || (path === `/api/ri-exchange/approve/${history.id}` && control.failApprove)
    );
    if (!expected) errors.push(`${response.status()} ${path}`);
  });
  await page.route('**/api/auth/me/permissions', route => route.fulfill({ json: {
    permissions: [{ action: 'admin', resource: '*' }, { action: 'execute', resource: 'ri-exchange' }],
  } }));
  await page.route('**/api/ri-exchange/**', async route => {
    const path = new URL(route.request().url()).pathname;
    const end = path.split('/').pop()!;
    if (route.request().method() === 'POST') {
      if (end === 'quote') {
        control.quoteCalls++;
        await control.quoteDelay;
        await route.fulfill({ status: control.failQuote ? 500 : 200, json: control.failQuote ? { error: 'quote refused' } : quote });
        return;
      }
      requests.push({ path, body: route.request().postDataJSON() });
      const failed = end === 'execute' ? control.failExecute : control.failApprove;
      if (end === 'execute') await control.executeDelay;
      if (!failed) {
        history.status = 'completed';
        exchanged = true;
      }
      await route.fulfill({ status: failed ? 500 : 200, json: failed ? { error: 'exchange refused' } : { exchange_id: 'synthetic-exchange', quote } });
      return;
    }
    const responses: Record<string, unknown> = {
      instances: { instances: [ri, otherRI].filter(id => id !== ri || !exchanged).map(id => ({ reserved_instance_id: id, instance_type: 'm5.large', instance_count: 2,
        region: 'us-east-1', state: 'active', offering_class: 'convertible', offering_type: 'No Upfront',
        product_description: 'Linux/UNIX', start: '2026-01-01T00:00:00Z', end: '2027-01-01T00:00:00Z' })) },
      utilization: { utilization: [] }, 'reshape-recommendations': { recommendations: [] }, history: { records: [history] },
      'target-offerings': { offerings: [offering, secondOffering].map(offering_id => ({ offering_id, instance_type: 'm5.xlarge', offering_type: 'No Upfront' })) },
    };
    await route.fulfill({ json: responses[end] ?? {} });
  });
  await page.goto('/inventory/ri-exchange');
  return { history, quote, requests, control, errors };
}

async function openQuote(page: Page) {
  await page.locator(`[data-action="quote-ri"][data-ri-id="${ri}"]`).click();
  await page.locator('.modal-exchange-target-select').selectOption(offering);
  await page.getByRole('button', { name: 'Get Quote', exact: true }).click();
  await expect(page.locator('#modal-exchange-result')).toContainText('Valid Exchange');
  await expect(page.locator('#modal-exchange-result')).toContainText('Hourly Price Change:-0.0200 USD/hr, about -14.60 USD/month');
}

const execute = (page: Page) => page.locator('#ri-exchange-modal').getByRole('button', { name: 'Execute Exchange', exact: true });
const dialog = (page: Page) => page.locator('.modal-confirm-backdrop');
const confirm = (page: Page) => dialog(page).locator('.btn-destructive');

for (const multiple of [false, true]) {
  test(`Execute confirms exact ${multiple ? 'multiple' : 'single'} targets and raw payment`, async ({ page }) => {
    const state = await setup(page);
    await openQuote(page);
    if (multiple) {
      await page.getByRole('button', { name: '+ Add target', exact: true }).click();
      await expect(execute(page)).toBeHidden();
      await page.locator('.modal-exchange-target-select').nth(1).selectOption(secondOffering);
      await page.locator('.modal-exchange-count').nth(1).fill('3');
      await page.getByRole('button', { name: 'Get Quote', exact: true }).click();
    }
    await execute(page).click();
    await expect(dialog(page)).toContainText(`USD ${quotedPayment}`);
    await expect(dialog(page)).toContainText(`2 × m5.xlarge (No Upfront, offering ${offering})`);
    await expect(dialog(page)).toContainText(ri);
    await expect(dialog(page)).toContainText('us-east-1');
    if (multiple) await expect(dialog(page)).toContainText(`3 × m5.xlarge (No Upfront, offering ${secondOffering})`);
    expect(state.requests).toHaveLength(0);
    // Destructive dialogs open on the safe action, so a reflex Enter cancels.
    await expect(dialog(page).getByRole('button', { name: 'Cancel', exact: true })).toBeFocused();
    await page.keyboard.press('Tab');
    await expect(confirm(page)).toBeFocused();
    await confirm(page).click();
    await expect(page.locator('#modal-exchange-result')).toContainText('Exchange completed. ID: synthetic-exchange');
    await expect(page.locator('.toast--success')).toContainText('Exchange ID: synthetic-exchange');
    expect(state.requests).toEqual([{ path: '/api/ri-exchange/execute', body: {
      ri_ids: [ri], ...(multiple ? { targets: [{ offering_id: offering, count: 2 }, { offering_id: secondOffering, count: 3 }] } : { target_offering_id: offering, target_count: 2 }),
      max_payment_due_usd: quotedPayment, region: 'us-east-1',
    } }]);
    expect(state.errors).toEqual([]);
  });
}

for (const action of ['Execute', 'Approve']) {
  for (const dismissal of ['Cancel', 'Escape', 'Close', 'backdrop']) {
    test(`${action} ${dismissal} refuses mutation and permits a later confirmation`, async ({ page }) => {
      const state = await setup(page);
      if (action === 'Execute') await openQuote(page);
      const trigger = action === 'Execute' ? execute(page) : page.locator('.riexchange-approve-btn');
      await trigger.click();
      await expect(dialog(page)).toHaveCount(1);
      if (action === 'Approve') {
        await expect(dialog(page)).toContainText('Previously quoted payment: USD 123.456789');
        await expect(dialog(page)).toContainText('fresh quote');
        await expect(dialog(page)).toContainText(`2 × m5.large (${ri})`);
        await expect(dialog(page)).toContainText(`1 × m5.xlarge (offering ${offering})`);
      }
      if (dismissal === 'Escape') await page.keyboard.press('Escape');
      else if (dismissal === 'backdrop') await dialog(page).click({ position: { x: 2, y: 2 } });
      else await dialog(page).getByRole('button', { name: dismissal, exact: true }).click();
      await expect(dialog(page)).toHaveCount(0);
      expect(state.requests).toHaveLength(0);
      await expect(trigger).toBeEnabled();
      await expect(trigger).toBeFocused();
      await trigger.click();
      await confirm(page).click();
      await expect.poll(() => state.requests.length).toBe(1);
      if (action === 'Approve') {
        expect(state.requests[0]).toEqual({ path: `/api/ri-exchange/approve/${state.history.id}`, body: null });
        await expect(page.locator('#ri-exchange-history-list')).toContainText('Completed');
      }
      expect(state.errors).toEqual([]);
    });
  }
  test(`${action} failure requires a fresh confirmation before retry`, async ({ page }) => {
    const state = await setup(page);
    if (action === 'Execute') await openQuote(page);
    state.control.failExecute = state.control.failApprove = true;
    const trigger = action === 'Execute' ? execute(page) : page.locator('.riexchange-approve-btn');
    await trigger.click();
    await confirm(page).click();
    await expect(trigger).toBeEnabled();
    await expect(page.getByText(action === 'Execute' ? /Exchange failed:/ : /Failed to approve exchange:/)).toBeVisible();
    state.control.failExecute = state.control.failApprove = false;
    await trigger.click();
    expect(state.requests).toHaveLength(1);
    await confirm(page).click();
    await expect.poll(() => state.requests.length).toBe(2);
    expect(state.errors).toEqual([]);
  });
}

for (const edit of ['count', 'revert', 'offering', 'add', 'remove']) {
  test(`late quote cannot restore authorization after ${edit}`, async ({ page }) => {
    const state = await setup(page);
    await openQuote(page);
    if (edit === 'remove') await page.getByRole('button', { name: '+ Add target', exact: true }).click();
    if (edit === 'remove') await page.locator('.modal-exchange-target-select').nth(1).selectOption(secondOffering);
    let release!: () => void;
    state.control.quoteDelay = new Promise(resolve => { release = resolve; });
    await page.getByRole('button', { name: 'Get Quote', exact: true }).click();
    await expect.poll(() => state.control.quoteCalls).toBe(2);
    if (edit === 'count' || edit === 'revert') await page.locator('.modal-exchange-count').first().fill('3');
    if (edit === 'revert') await page.locator('.modal-exchange-count').first().fill('2');
    if (edit === 'offering') await page.locator('.modal-exchange-target-select').first().selectOption(secondOffering);
    if (edit === 'add') await page.getByRole('button', { name: '+ Add target', exact: true }).click();
    if (edit === 'remove') await page.getByRole('button', { name: 'Remove target' }).last().click();
    release();
    await expect(page.getByRole('button', { name: 'Get Quote', exact: true })).toBeEnabled();
    await expect(execute(page)).toBeHidden();
    expect(state.requests).toHaveLength(0);
    if (edit === 'add') await page.locator('.modal-exchange-target-select').nth(1).selectOption(secondOffering);
    await page.getByRole('button', { name: 'Get Quote', exact: true }).click();
    await execute(page).click();
    await confirm(page).click();
    await expect.poll(() => state.requests.length).toBe(1);
  });
}

for (const reopenBeforeResponse of [false, true]) {
  test(`old execution refreshes inventory and history without closing a reopened modal (pending response: ${reopenBeforeResponse})`, async ({ page }) => {
    const state = await setup(page);
    await openQuote(page);
    let release!: () => void;
    state.control.executeDelay = new Promise(resolve => { release = resolve; });
    await execute(page).click();
    await confirm(page).dblclick();
    await expect.poll(() => state.requests.length).toBe(1);
    await expect(execute(page)).toBeDisabled();
    if (!reopenBeforeResponse) {
      release();
      await expect(page.locator('#modal-exchange-result')).toContainText('Exchange completed');
    }
    // A completed exchange keeps its receipt open and offers Close instead of Cancel.
    await page.locator('#ri-exchange-modal').getByRole('button', { name: reopenBeforeResponse ? 'Cancel' : 'Close', exact: true }).click();
    await page.locator(`[data-action="quote-ri"][data-ri-id="${otherRI}"]`).click();
    if (reopenBeforeResponse) release();
    await expect(page.locator('#ri-exchange-history-list')).toContainText('Completed');
    await expect(page.locator(`[data-action="quote-ri"][data-ri-id="${ri}"]`)).toHaveCount(0);
    await page.waitForTimeout(2200);
    await expect(page.locator('#ri-exchange-modal')).toBeVisible();
    expect(state.requests).toHaveLength(1);
    expect(state.errors).toEqual([]);
  });
}

test('quote errors and close during quote leave a new session usable', async ({ page }) => {
  const state = await setup(page);
  state.control.failQuote = true;
  await page.locator(`[data-action="quote-ri"][data-ri-id="${ri}"]`).click();
  await page.locator('.modal-exchange-target-select').selectOption(offering);
  await page.getByRole('button', { name: 'Get Quote', exact: true }).click();
  await expect(page.locator('#modal-exchange-result')).toContainText('Quote failed:');
  state.control.failQuote = false;
  let release!: () => void;
  state.control.quoteDelay = new Promise(resolve => { release = resolve; });
  await page.getByRole('button', { name: 'Get Quote', exact: true }).click();
  await expect.poll(() => state.control.quoteCalls).toBe(2);
  await page.locator('#ri-exchange-modal').getByRole('button', { name: 'Cancel', exact: true }).click();
  await page.locator(`[data-action="quote-ri"][data-ri-id="${ri}"]`).click();
  release();
  await expect(execute(page)).toBeHidden();
  await page.locator('.modal-exchange-target-select').selectOption(offering);
  await page.getByRole('button', { name: 'Get Quote', exact: true }).click();
  await execute(page).click();
  await confirm(page).click();
  await expect.poll(() => state.requests.length).toBe(1);
});

test('zero remains explicit and absent money or currency cannot authorize', async ({ page }) => {
  const state = await setup(page);
  state.quote.PaymentDueRaw = '';
  await openQuote(page);
  await execute(page).click();
  await expect(page.locator('#modal-exchange-result')).toContainText('Cannot confirm');
  state.quote.PaymentDueRaw = 'NaN';
  await page.getByRole('button', { name: 'Get Quote', exact: true }).click();
  await execute(page).click();
  await expect(page.locator('#modal-exchange-result')).toContainText('Cannot confirm');
  state.quote.PaymentDueRaw = '0';
  state.quote.CurrencyCode = '';
  await page.getByRole('button', { name: 'Get Quote', exact: true }).click();
  await execute(page).click();
  await expect(page.locator('#modal-exchange-result')).toContainText('Cannot confirm');
  expect(state.requests).toHaveLength(0);
  state.quote.CurrencyCode = 'USD';
  await page.getByRole('button', { name: 'Get Quote', exact: true }).click();
  await execute(page).click();
  await expect(dialog(page)).toContainText('USD 0');
  await confirm(page).click();
  await expect.poll(() => state.requests.length).toBe(1);
});

test('approval handles missing and zero record amounts without inventing money', async ({ page }) => {
  const state = await setup(page);
  state.history.payment_due = '';
  await page.locator('#ri-exchange-refresh-btn').click();
  await page.locator('.riexchange-approve-btn').click();
  await expect(page.getByText('Cannot approve exchange without a previously quoted payment.')).toBeVisible();
  expect(state.requests).toHaveLength(0);
  state.history.payment_due = 'NaN';
  await page.locator('#ri-exchange-refresh-btn').click();
  await page.locator('.riexchange-approve-btn').click();
  await expect(dialog(page)).toHaveCount(0);
  expect(state.requests).toHaveLength(0);
  state.history.payment_due = '0.000000';
  state.history.target_instance_type = '<img src=x onerror=alert(1)>';
  await page.locator('#ri-exchange-refresh-btn').click();
  await page.locator('.riexchange-approve-btn').click();
  await expect(dialog(page)).toContainText('Previously quoted payment: USD 0.000000');
  await expect(dialog(page)).toContainText(state.history.target_instance_type);
  await expect(dialog(page).locator('img')).toHaveCount(0);
  await confirm(page).click();
  await expect.poll(() => state.requests.length).toBe(1);
});

test('a completed exchange keeps its ID on screen and links to the history', async ({ page }) => {
  const state = await setup(page);
  await openQuote(page);
  await execute(page).click();
  await confirm(page).click();
  const result = page.locator('#modal-exchange-result');
  await expect(result).toHaveAttribute('role', 'status');
  await expect(result).toContainText('Exchange completed. ID: synthetic-exchange');
  await page.waitForTimeout(2500);
  await expect(page.locator('#ri-exchange-modal')).toBeVisible();
  await expect(page.locator('#ri-exchange-modal').getByRole('button', { name: 'Close', exact: true })).toBeFocused();
  await page.locator('.toast--success').getByRole('button', { name: 'View history', exact: true }).click();
  await expect(page.locator('#ri-exchange-modal')).toBeHidden();
  await expect(page.locator('#ri-exchange-history-list')).toBeFocused();
  expect(state.errors).toEqual([]);
});
