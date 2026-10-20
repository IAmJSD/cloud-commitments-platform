import { test, expect } from '@playwright/test';

test.use({ hasTouch: true });
import { mockApi, seedAuth } from './fixtures/recs';

for (const [width, height] of [[1440, 900], [1280, 900], [1280, 768], [1024, 900], [390, 900], [320, 900]] as const) test.describe(`viewport ${width}x${height}`, () => {
  test.use({ isMobile: width <= 390 });
  test(`local Create/Edit preview at ${width}x${height} (mock API, no writes)`, async ({ page, isMobile }, testInfo) => {
    test.setTimeout(90_000);
    await page.setViewportSize({ width, height });
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
      await page.emulateMedia({ reducedMotion: mode === 'Create' && width > 390 ? 'no-preference' : 'reduce' });
      await page.locator(mode === 'Create' ? '#new-plan-btn' : '[data-action="edit-plan"][data-id="draft-plan"]').click();
      const parent = page.locator('#plan-modal');
      await expect(parent).toBeVisible();
      await expect(page.locator('#preview-plan-schedule-btn')).toHaveCount(0);
      await expect(page.locator('#plan-schedule-preview')).toHaveCount(0);
      await expect(parent.locator('#plan-name')).toBeFocused();
      const advanced = parent.locator('#plan-advanced-settings'), summary = advanced.locator(':scope > summary');
      await expect(advanced).not.toHaveAttribute('open', '');
      await summary.focus(); await summary.press('Enter'); await expect(advanced).toHaveAttribute('open', '');
      await expect(parent.locator('#plan-description')).toBeDisabled();
      await expect(advanced).toContainText('Descriptions are unavailable and are not saved');
      const payment = await parent.locator('#plan-payment').inputValue();
      await advanced.scrollIntoViewIfNeeded(); await page.screenshot({ path: testInfo.outputPath(mode.toLowerCase() + '-advanced.png'), fullPage: true });
      await summary.press('Space'); await expect(advanced).not.toHaveAttribute('open', '');
      expect(await parent.locator('#plan-payment').inputValue()).toBe(payment);

      if (mode === 'Create') {
        await page.locator('#plan-name').fill('Capacity ladder');
        await parent.locator('label.ramp-option').filter({ hasText: 'Weekly 25%' }).click();
        const panel = parent.locator('#plan-editor-panel'), live = panel.locator('[data-editor="summary"]');
        await expect(panel).toBeVisible(); await expect(panel).toContainText('They are not saved or scheduled');
        await expect(panel).not.toContainText('Custom step dates can be set once saving them is supported');
        await expect(live).toHaveText(/^Target \d+% in 4 steps, \d{4}-\d{2}-\d{2} to \d{4}-\d{2}-\d{2} \(UTC\)$/);
        await expect(panel.locator('[data-preview-step]')).toHaveCount(4);
        await parent.locator('#plan-coverage').fill('');
        await expect(live).toContainText('Target coverage must be a whole number');
        await expect(panel.locator('.schedule-preview-scroll')).toBeHidden(); await expect(panel.locator('.editor-target')).toBeHidden();
        await page.screenshot({ path: testInfo.outputPath('create-invalid.png'), fullPage: true });
        await parent.locator('#plan-coverage').fill('80');
        await expect(panel.locator('.schedule-preview-scroll')).toBeVisible(); await expect(panel.locator('.editor-target')).toBeVisible();
        await expect(panel.locator('svg [role="button"]')).toHaveCount(0);
        await expect(panel.locator('[data-editor="controls"]')).toBeHidden();
        await expect(panel.locator('.schedule-preview-scroll')).toHaveAttribute('role', 'region');
        await parent.locator('#plan-account-search').fill('AWS');
        await parent.locator('.account-suggestion-item').first().click(); await expect(parent.locator('button[type="submit"]')).toBeEnabled();
        await summary.click();
        await parent.locator('#plan-notify-days').fill('1.5'); await summary.click();
        await parent.locator('button[type="submit"]').click();
        await expect(advanced).toHaveAttribute('open', '');
        await expect(parent.locator('#plan-notify-days')).toBeFocused();
        expect(await parent.locator('#plan-notify-days').evaluate(element => element.validity.stepMismatch)).toBe(true);
        await parent.locator('#plan-notify-days').fill('3'); await summary.click();
        await parent.locator('#plan-name').fill(''); await parent.locator('button[type="submit"]').click();
        await expect(advanced).not.toHaveAttribute('open', '');
        await expect(parent.locator('#plan-name')).toBeFocused(); await parent.locator('#plan-name').fill('Capacity ladder');
        const labels = await panel.locator('[data-preview-step]').evaluateAll(elements => elements.map(element => element.getAttribute('aria-label')));
        const point = panel.locator('[data-preview-step="1"]'); await point.scrollIntoViewIfNeeded();
        const pointBox = (await point.boundingBox())!;
        await page.mouse.move(pointBox.x + pointBox.width / 2, pointBox.y + pointBox.height / 2); await page.mouse.down();
        await page.mouse.move(pointBox.x + pointBox.width / 2 + 60, pointBox.y + pointBox.height / 2); await page.mouse.up();
        expect(await panel.locator('[data-preview-step]').evaluateAll(elements => elements.map(element => element.getAttribute('aria-label')))).toEqual(labels);
        expect(await parent.locator('#plan-form').evaluate(element => element.inert)).toBe(false);
        expect(await parent.evaluate(element => element.scrollWidth <= element.clientWidth)).toBe(true);
        await parent.locator('.plan-editor-layout').evaluate(element => { element.scrollTop = 0; });
        await page.screenshot({ path: testInfo.outputPath('create-inline-graph.png'), fullPage: true });
        await panel.locator('.editor-draft-notice').scrollIntoViewIfNeeded(); await page.screenshot({ path: testInfo.outputPath('create-inline-notice.png'), fullPage: true });
        await parent.locator('#close-plan-modal-btn').scrollIntoViewIfNeeded();
        const footer = (await parent.locator('.modal-buttons').boundingBox())!;
        expect(footer.y + footer.height).toBeLessThanOrEqual(page.viewportSize()!.height);
        await page.screenshot({ path: testInfo.outputPath('create-inline-actions.png'), fullPage: true });
        expect(await panel.evaluate(element => !!(element.previousElementSibling as HTMLElement | null)?.matches('#plan-form'))).toBe(true);
        await summary.focus(); await page.keyboard.press('Tab');
        await expect(panel.locator('.schedule-preview-scroll')).toBeFocused(); await page.keyboard.press('Tab');
        await expect(parent.locator('#close-plan-modal-btn')).toBeFocused();
        await page.keyboard.press('Shift+Tab'); await expect(panel.locator('.schedule-preview-scroll')).toBeFocused();
        await page.keyboard.press('Shift+Tab'); await expect(summary).toBeFocused();
        await summary.click(); await expect(advanced).toHaveAttribute('open', '');
        await parent.locator('#close-plan-modal-btn').click(); await expect(parent).toBeHidden();
        await page.locator('#new-plan-btn').click(); await parent.locator('#plan-name').fill('Capacity ladder');
        await expect(advanced).not.toHaveAttribute('open', '');
        await parent.locator('label.ramp-option').filter({ hasText: 'Weekly 25%' }).click();
        await expect(live).toHaveText(/^Target \d+% in 4 steps, \d{4}-\d{2}-\d{2} to \d{4}-\d{2}-\d{2} \(UTC\)$/);
        await expect(panel.locator('[data-editor="controls"]')).toBeHidden();

      } else {
        const panel = parent.locator('#plan-editor-panel');
        await expect(panel).toBeVisible();
        await expect(panel.locator('[data-preview-step]')).toHaveCount(4);
        await expect(panel.locator('.editor-draft-notice')).toContainText('restarts from today for a plan already in progress; executed purchases are not shown');
        await expect(panel.locator('[data-editor="controls"]')).toBeHidden();
        await page.screenshot({ path: testInfo.outputPath('edit-inline-graph.png'), fullPage: true });
      }
      const layoutForm = (await parent.locator('#plan-form').boundingBox())!, layoutPanel = (await parent.locator('#plan-editor-panel').boundingBox())!;
      if (width > 960) {
        expect(layoutForm.width).toBeGreaterThanOrEqual(360); expect(layoutPanel.x).toBeGreaterThanOrEqual(layoutForm.x + layoutForm.width);
        expect(layoutPanel.width).toBeGreaterThan(layoutForm.width);
        expect(await parent.locator('#plan-editor-panel').evaluate(element => getComputedStyle(element).position)).toBe('sticky');
      } else expect(layoutPanel.y).toBeGreaterThanOrEqual(layoutForm.y + layoutForm.height - 1);
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth)).toBe(true);
      if (width > 960) {
        const reach = await parent.evaluate(() => {
          const scroller = document.querySelector('.plan-editor-layout')!, sticky = document.getElementById('plan-editor-panel')!;
          sticky.scrollTop = sticky.scrollHeight;
          const bounds = scroller.getBoundingClientRect(), last = Array.from(sticky.querySelectorAll('.help-text')).pop()!.getBoundingClientRect(), own = sticky.getBoundingClientRect();
          return { panelFits: own.height <= bounds.height + 1, lastVisible: last.top >= bounds.top - 1 && last.bottom <= bounds.bottom + 1 };
        });
        expect(reach).toEqual({ panelFits: true, lastVisible: true });
      }
      await parent.locator('#close-plan-modal-btn').scrollIntoViewIfNeeded();
      const cancel = (await parent.locator('#close-plan-modal-btn').boundingBox())!;
      expect(cancel.x).toBeGreaterThanOrEqual(0); expect(cancel.x + cancel.width).toBeLessThanOrEqual(width);
      const footerBox = (await parent.locator('.modal-buttons').boundingBox())!;
      expect(footerBox.y + footerBox.height).toBeLessThanOrEqual(page.viewportSize()!.height);
      await page.locator('#close-plan-modal-btn').click();
    }
    await page.locator('#new-plan-btn').click();
    const dense = page.locator('#plan-editor-panel'), scroll = dense.locator('.schedule-preview-scroll');
    await page.locator('#plan-modal label.ramp-option').filter({ hasText: 'Define your own schedule' }).click();
    await page.locator('#ramp-step-percent').fill('1');
    await expect(dense.locator('[data-preview-step]')).toHaveCount(100);
    await scroll.scrollIntoViewIfNeeded();
    expect((await dense.locator('svg').boundingBox())!.width).toBeGreaterThanOrEqual(4896);
    expect(await scroll.evaluate(element => element.scrollWidth > element.clientWidth)).toBe(true);
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth)).toBe(true);
    expect(await page.locator('#plan-modal').evaluate(element => element.scrollWidth <= element.clientWidth)).toBe(true);
    expect(await page.locator('#plan-form').evaluate(element => element.scrollWidth <= element.clientWidth)).toBe(true);
    await scroll.focus();
    await page.keyboard.press('ArrowRight');
    await expect.poll(() => scroll.evaluate(element => element.scrollLeft)).toBeGreaterThan(0);
    await scroll.evaluate(element => { element.scrollLeft = 0; });
    if (width <= 390) {
      const box = (await scroll.boundingBox())!, svgBox = (await dense.locator('svg').boundingBox())!;
      const y = svgBox.y + svgBox.height / 2, left = box.x + 40, right = box.x + box.width - 40;
      const beforeScroll = await scroll.evaluate(element => element.scrollLeft);
      const labels = await dense.locator('[data-preview-step]').evaluateAll(elements => elements.map(element => element.getAttribute('aria-label')));
      const session = await page.context().newCDPSession(page);
      try {
        await session.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x: right, y, id: 1 }] });
        for (let move = 1; move <= 12; move++) { await page.evaluate(() => new Promise(requestAnimationFrame)); await session.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x: right + (left - right) * move / 12, y, id: 1 }] }); }
        await session.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
        await page.evaluate(() => new Promise<void>(resolve => requestAnimationFrame(() => requestAnimationFrame(() => resolve()))));
        await expect.poll(() => scroll.evaluate(element => element.scrollLeft)).toBeGreaterThan(beforeScroll);
        expect(await dense.locator('[data-preview-step]').evaluateAll(elements => elements.map(element => element.getAttribute('aria-label')))).toEqual(labels);
      } finally { await session.detach(); }
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth)).toBe(true);
    }
    expect(writes).toEqual([]);
    expect(errors).toEqual([]);
    await testInfo.attach('production-preview-receipt', { body: JSON.stringify({ width, isMobile, scripts, writes, errors }), contentType: 'application/json' });
  });
});
