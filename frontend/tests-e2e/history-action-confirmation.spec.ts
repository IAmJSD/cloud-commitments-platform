import { test, expect, type Page } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

const ID = '11111111-1111-1111-1111-111111111111';
const actions = ['approve', 'cancel', 'retry', 'revoke', 'marketplace-sell', 'marketplace-cancel'] as const;
type Action = typeof actions[number];

const DETAILS = {
  execution_id: ID, status: 'pending', total_upfront_cost: 1200, estimated_savings: 10,
  recommendations: [{ id: 'r1', provider: 'aws', service: 'ec2', region: 'us-east-1', resource_type: 'm5.large',
    count: 1, term: 1, payment: 'all-upfront', upfront_cost: 1200, monthly_cost: 0, savings: 10 }],
};

function gate(): { promise: Promise<void>; release(): void } {
  let release!: () => void;
  const promise = new Promise<void>(resolve => { release = resolve; });
  return { promise, release };
}

async function historyFixture(page: Page, action: Action) {
  await seedAuth(page);
  await mockApi(page);
  const row = {
    purchase_id: ID, timestamp: new Date().toISOString(), provider: action === 'revoke' ? 'azure' : 'aws',
    service: 'ec2', resource_type: 'm5.large', region: 'us-east-1', count: 1, term: 1,
    upfront_cost: 1200, monthly_cost: 50, estimated_savings: 10, offering_class: 'standard',
    status: action === 'retry' ? 'failed' : ['approve', 'cancel'].includes(action) ? 'pending' : 'completed',
    account_id: 'acct-001', created_by_user_id: 'user-smoke', retry_attempt_n: 5,
    revocation_window_closes_at: new Date(Date.now() + 86400000).toISOString(),
    listing_state: action === 'marketplace-cancel' ? 'active' : '',
  };
  const posts: { url: string; body: string | null }[] = [];
  let failNext = false;
  await page.route('**/api/auth/me/permissions', route => route.fulfill({ json: { permissions:
    ['admin', 'approve-any', 'retry-any', 'cancel-any', 'revoke-any', 'sell-any'].map(verb => ({ action: verb, resource: verb === 'admin' ? '*' : 'purchases' })),
  } }));
  await page.route('**/api/info/deployment', route => route.fulfill({ json: {} }));
  await page.route('**/api/history**', route => route.fulfill({ json: { summary: {}, purchases: [row] } }));
  await page.route('**/api/purchases/**', async route => {
    if (route.request().method() === 'POST') {
      posts.push({ url: route.request().url(), body: route.request().postData() });
      const failed = failNext;
      failNext = false;
      if (!failed) { row.status = 'cancelled'; row.listing_state = ''; }
      await route.fulfill({ status: failed ? 500 : 200, json: failed ? { error: 'synthetic mutation failure' } : { status: 'pending', email_sent: true, execution_id: 'new-execution' } });
    } else {
      await route.fulfill({ json: route.request().url().includes('/calculate')
        ? { refund_amount: 100, refund_currency: 'USD' }
        : DETAILS });
    }
  });
  return { row, posts, failNextMutation: () => { failNext = true; } };
}

for (const action of actions) {
  test(`${action}: asynchronous confirmation, dismissal, API failure and explicit retry`, async ({ page }) => {
    const fixture = await historyFixture(page, action);
    const pageErrors: string[] = [];
    page.on('pageerror', error => pageErrors.push(error.message));
    await page.goto('/purchases/history');
    const btn = page.locator(`#history-list .history-${action}-btn`);
    await btn.click();
    await expect(page.locator('.modal-confirm-backdrop')).toHaveCount(1);
    await expect(btn).toBeDisabled();
    // DOM re-entry exercises the handler even though the modal blocks pointer clicks.
    await btn.dispatchEvent('click');
    await expect(page.locator('.modal-confirm-backdrop')).toHaveCount(1);
    await page.keyboard.press('Escape');
    await expect(btn).toBeEnabled();
    await expect(btn).toBeFocused();
    expect(fixture.posts).toHaveLength(0);
    for (const dismiss of ['.modal-confirm-close', '.modal-confirm-actions .btn-secondary', '.modal-confirm-backdrop']) {
      await btn.click();
      await page.locator(dismiss).click({ position: { x: 5, y: 5 } });
      await expect(btn).toBeEnabled();
      await expect(btn).toBeFocused();
      expect(fixture.posts).toHaveLength(0);
    }
    fixture.failNextMutation();
    await btn.click();
    await expect(page.locator('.modal-confirm-backdrop')).toHaveCount(1);
    await page.locator('.modal-confirm-actions button').last().click();
    await expect.poll(() => fixture.posts.length).toBe(1);
    await expect(btn).toBeEnabled();
    await btn.click();
    await expect(page.locator('.modal-confirm-backdrop')).toHaveCount(1);
    await page.locator('.modal-confirm-actions button').last().click();
    await expect.poll(() => fixture.posts.length).toBe(2);
    await expect(btn).toHaveCount(0);
    expect(fixture.posts.every(post => post.url.includes(ID))).toBe(true);
    if (action === 'retry') expect(fixture.posts[1]!.url).toContain('force=true');
    if (action === 'revoke') expect(JSON.parse(fixture.posts[1]!.body!)).toMatchObject({ expected_refund_amount: 100, expected_refund_currency: 'USD' });
    expect(pageErrors).toEqual([]);
  });
}

test('actual double-click and second projection cannot duplicate pending approval details', async ({ page }) => {
  const fixture = await historyFixture(page, 'approve');
  const details = gate();
  let detailGets = 0;
  await page.route(`**/api/purchases/${ID}`, async route => {
    detailGets++;
    await details.promise;
    await route.fulfill({ json: DETAILS });
  });
  await page.goto('/purchases/history');
  const btn = page.locator('#history-list .history-approve-btn');
  await btn.dblclick();
  await expect(btn).toBeDisabled();
  await expect(page.locator('#history-list .history-cancel-btn')).toBeDisabled();
  await page.locator('#purchases-approval-queue .history-approve-btn').click();
  details.release();
  await expect(page.locator('.modal-confirm-backdrop')).toHaveCount(1);
  expect(detailGets).toBe(1);
  await page.locator('.modal-confirm-actions button').last().click();
  await expect.poll(() => fixture.posts.length).toBe(1);
});

test('approve is not offered when the purchase details cannot be loaded (issue #247)', async ({ page }) => {
  const fixture = await historyFixture(page, 'approve');
  let detailGets = 0;
  await page.route(`**/api/purchases/${ID}`, route => {
    detailGets++;
    return detailGets === 1
      ? route.fulfill({ status: 500, json: { error: 'internal error' } })
      : route.fulfill({ json: DETAILS });
  });
  await page.goto('/purchases/history');
  const btn = page.locator('#history-list .history-approve-btn');
  await btn.click();
  await expect(page.getByRole('alert')).toContainText('cannot be approved without showing the amount');
  await expect(page.locator('.modal-confirm-backdrop')).toHaveCount(0);
  await expect(btn).toBeEnabled();
  await btn.click();
  await expect(page.locator('.modal-confirm-body')).toContainText('$1,200');
  await page.locator('.modal-confirm-actions button').last().click();
  await expect.poll(() => fixture.posts.length).toBe(1);
});

for (const [status, wording] of [[403, 'view:purchases'], [404, 'outside your account access'], [410, 'approval link has expired']] as const) {
  test(`approval link with details ${status} shows permanent wording without retry advice (issue #529)`, async ({ page }) => {
    const fixture = await historyFixture(page, 'approve');
    await page.route(`**/api/purchases/approve/${ID}/details`, route => route.fulfill({ status, json: { error: 'denied by server' } }));
    await page.goto(`/purchases/approve/${ID}?token=tok`);
    const alert = page.getByRole('alert');
    await expect(alert).toContainText('cannot be approved without showing the amount');
    await expect(alert).toContainText(wording);
    await expect(alert).toContainText('denied by server');
    await expect(alert).not.toContainText(/try again|retry/i);
    await expect(page.locator('.modal-confirm-backdrop')).toHaveCount(0);
    expect(fixture.posts).toHaveLength(0);
  });
}

test('canceling an independent row does not steal focus from another confirmation', async ({ page }) => {
  const fixture = await historyFixture(page, 'approve');
  const details = gate();
  await page.route('**/api/history**', route => route.fulfill({ json: { summary: {}, purchases: [fixture.row, { ...fixture.row, purchase_id: 'other-execution' }] } }));
  await page.route('**/api/purchases/*', async route => {
    await details.promise;
    await route.fulfill({ json: DETAILS });
  });
  await page.goto('/purchases/history');
  const buttons = page.locator('#history-list .history-approve-btn');
  await buttons.nth(0).click();
  await buttons.nth(1).click();
  details.release();
  await expect(page.locator('.modal-confirm-backdrop')).toHaveCount(2);
  await page.locator('.modal-confirm-close').last().click();
  await expect(page.locator('.modal-confirm-backdrop')).toHaveCount(1);
  await expect(page.locator('.modal-confirm-actions .btn-primary')).toBeFocused();
  await page.locator('.modal-confirm-close').click();
  await expect(page.locator('.modal-confirm-backdrop')).toHaveCount(0);
  expect(fixture.posts).toHaveLength(0);
});

test('success survives failed refresh, cached redraw and old GET; fresh same-ID revoke remains usable', async ({ page }) => {
  const fixture = await historyFixture(page, 'approve');
  const oldGet = gate();
  let holdNext = false;
  let held = false;
  let failHistory = false;
  await page.route('**/api/history**', async route => {
    const snapshot = { ...fixture.row };
    if (holdNext) {
      holdNext = false;
      held = true;
      await oldGet.promise;
      await route.fulfill({ json: { summary: {}, purchases: [snapshot] } });
    } else if (failHistory) {
      await route.fulfill({ status: 500, json: { error: 'synthetic history failure' } });
    } else await route.fulfill({ json: { summary: {}, purchases: [fixture.row] } });
  });
  await page.goto('/purchases/history');
  await expect(page.locator('.history-approve-btn')).toHaveCount(2);
  holdNext = true;
  await page.getByRole('button', { name: 'Load History', exact: true }).click();
  await expect.poll(() => held).toBe(true);
  await page.locator('#history-amortize-checkbox').check();
  await page.locator('#history-list .history-approve-btn').click();
  failHistory = true;
  await expect(page.locator('.modal-confirm-backdrop')).toHaveCount(1);
  await page.locator('.modal-confirm-actions button').last().click();
  await expect(page.locator('#history-list .error')).toBeVisible();
  const oldResponse = page.waitForResponse(response => response.url().includes('/api/history') && response.status() === 200);
  oldGet.release();
  await (await oldResponse).finished();
  await page.evaluate(() => new Promise<void>(resolve => requestAnimationFrame(() => resolve())));
  await page.locator('#history-amortize-checkbox').uncheck();
  await expect(page.locator('.history-approve-btn')).toHaveCount(0);
  expect(fixture.posts).toHaveLength(1);
  failHistory = false;
  fixture.row.provider = 'azure';
  fixture.row.status = 'scheduled';
  await page.getByRole('button', { name: 'Load History', exact: true }).click();
  await page.locator('#history-list .history-revoke-btn').click();
  await expect(page.locator('.modal-confirm-backdrop')).toHaveCount(1);
  await page.locator('.modal-confirm-actions button').last().click();
  await expect.poll(() => fixture.posts.length).toBe(2);
  expect(fixture.posts[1]!.url).toContain(`/purchases/${ID}/revoke`);
});

test('completed revoke owns its row before refund quote and recovers from quote failure', async ({ page }) => {
  const fixture = await historyFixture(page, 'revoke');
  const quote = gate();
  let quoteGets = 0;
  await page.route(`**/api/purchases/${ID}/revoke/calculate`, async route => {
    quoteGets++;
    if (quoteGets === 1) {
      await quote.promise;
      await route.fulfill({ status: 500, json: { error: 'synthetic quote failure' } });
    } else await route.fulfill({ json: { refund_amount: 100, refund_currency: 'USD' } });
  });
  await page.goto('/purchases/history');
  const btn = page.locator('#history-list .history-revoke-btn');
  await btn.dblclick();
  await expect(btn).toBeDisabled();
  quote.release();
  await expect(btn).toBeEnabled();
  expect(quoteGets).toBe(1);
  expect(fixture.posts).toHaveLength(0);
  await btn.click();
  await expect(page.locator('.modal-confirm-body')).toContainText('100.00 USD');
  await page.locator('.modal-confirm-actions button').last().click();
  await expect.poll(() => fixture.posts.length).toBe(1);
});
