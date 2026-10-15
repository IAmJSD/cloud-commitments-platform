import { test, expect } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

for (const width of [1440, 1280, 390, 320]) {
  test(`local Create/Edit preview at ${width}px (mock API, no writes)`, async ({ page }, testInfo) => {
    await page.setViewportSize({ width, height: 900 });
    await page.emulateMedia({ reducedMotion: 'reduce' });
    const errors: string[] = [], writes: string[] = [];
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error') errors.push(message.text()); });
    page.on('requestfailed', request => errors.push(request.url()));
    page.on('response', response => { if (response.status() >= 400) errors.push(`${response.status()} ${response.url()}`); });
    page.on('request', request => { if (request.url().includes('/api/') && request.method() !== 'GET') writes.push(request.url()); });
    await page.route('**/*', route => new URL(route.request().url()).hostname === '127.0.0.1' ? route.fallback() : route.abort());
    await seedAuth(page);
    await mockApi(page);
    const plan = { id: 'draft-plan', name: 'Existing plan', enabled: true, auto_purchase: false, notification_days_before: 3,
      services: { ec2: { provider: 'aws', service: 'ec2', enabled: true, term: 1, payment: 'no-upfront', coverage: 80 } },
      ramp_schedule: { type: 'weekly', current_step: 1, total_steps: 4, step_interval_days: 7, percent_per_step: 25 } };
    await page.route('**/api/plans**', route => route.fulfill({ json: new URL(route.request().url()).pathname.endsWith('/accounts') ? []
      : new URL(route.request().url()).pathname.endsWith('/draft-plan') ? plan : { plans: [plan] } }));
    await page.route('**/api/purchases/planned', route => route.fulfill({ json: { purchases: [] } }));
    await page.route('**/api/commitment-options**', route => route.fulfill({ json: { status: 'ok', aws: {} } }));
    await page.goto('/plans');
    const scripts = await page.locator('script[src]').evaluateAll(elements => elements.map(element => (element as HTMLScriptElement).src));
    expect(scripts.some(source => /app\.[a-f0-9]+\.js/.test(source))).toBe(true);
    for (const mode of ['Create', 'Edit']) {
      await page.locator(mode === 'Create' ? '#new-plan-btn' : '[data-action="edit-plan"][data-id="draft-plan"]').click();
      const parent = page.locator('#plan-modal');
      await expect(parent).toBeVisible();
      if (mode === 'Create') {
        await page.locator('#plan-name').fill('Capacity ladder');
        await parent.locator('label.ramp-option').filter({ hasText: 'Weekly 25%' }).click();
      }
      const before = await parent.locator('input, select, textarea').evaluateAll(elements => elements.map(element => (element as HTMLInputElement).value));
      const trigger = page.locator('#preview-plan-schedule-btn');
      await trigger.click();
      const preview = page.locator('#plan-schedule-preview');
      await expect(preview).toBeVisible();
      await expect(preview).toContainText('These dates are not saved or scheduled');
      await expect(preview).toContainText('Saved purchases and history are not shown');
      await expect(preview).toContainText('Amount unavailable');
      expect(await preview.locator('header').evaluate(element => getComputedStyle(element).display)).toBe('block');
      await expect(preview.locator('[data-preview-step]')).toHaveCount(4);
      expect(await parent.evaluate(element => element.inert)).toBe(true);
      expect(await preview.locator('details').evaluate(element => element.open)).toBe(false);
      await preview.getByRole('button', { name: 'Close preview' }).focus();
      await page.keyboard.press('Tab');
      await expect(preview.locator('.schedule-preview-scroll')).toBeFocused();
      await page.keyboard.press('Shift+Tab');
      await expect(preview.getByRole('button', { name: 'Close preview' })).toBeFocused();
      const point = preview.locator('[data-preview-step="2"]');
      await point.locator('.preview-hit').click();
      await expect(preview.locator('#preview-selected-step')).toHaveText('Step 3');
      await point.press('ArrowRight');
      await expect(preview.locator('[data-preview-step="3"]')).toBeFocused();
      await page.keyboard.press(' ');
      await page.keyboard.press('Enter');
      await expect(preview.locator('#preview-selected-target')).toHaveText('Planned target: 80%');
      expect(await preview.evaluate(element => element.scrollWidth <= element.clientWidth)).toBe(true);
      await page.screenshot({ path: testInfo.outputPath(`${mode.toLowerCase()}-preview.png`), fullPage: true });
      await page.keyboard.press('Escape');
      await expect(preview).toHaveCount(0);
      await expect(trigger).toBeFocused();
      await expect(parent).toBeVisible();
      expect(await parent.locator('input, select, textarea').evaluateAll(elements => elements.map(element => (element as HTMLInputElement).value))).toEqual(before);
      await page.locator('#close-plan-modal-btn').click();
    }
    await page.locator('#new-plan-btn').click();
    await page.locator('#plan-modal label.ramp-option').filter({ hasText: 'Define your own schedule' }).click();
    await page.locator('#ramp-step-percent').fill('1');
    await page.locator('#preview-plan-schedule-btn').click();
    const dense = page.locator('#plan-schedule-preview');
    await expect(dense.locator('[data-preview-step]')).toHaveCount(100);
    expect(await dense.evaluate(element => element.scrollWidth <= element.clientWidth)).toBe(true);
    await dense.locator('[data-preview-step="0"]').focus();
    await page.keyboard.press('ArrowRight');
    await expect(dense.locator('#preview-selected-step')).toHaveText('Step 2');
    await page.keyboard.press('Escape');
    expect(writes).toEqual([]);
    expect(errors).toEqual([]);
    await testInfo.attach('production-preview-receipt', { body: JSON.stringify({ width, scripts, writes, errors }), contentType: 'application/json' });
  });
}
