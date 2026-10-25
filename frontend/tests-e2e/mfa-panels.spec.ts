/**
 * MFA setup panels (issue #745). They were styled with style="" attributes,
 * which the page CSP (`style-src 'self'`) blocks, so in a real browser the
 * panel had no border, the codes were proportional text and the recovery
 * codes were a bulleted list. jsdom ignores CSP, so this runs in Chromium.
 */
import { test, expect, type Page } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

const SECRET = 'JBSWY3DPEHPK3PXP';

async function startEnrollment(page: Page): Promise<void> {
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/auth/me', (r) =>
    r.fulfill({ json: { id: 'user-smoke', email: 'smoke@example.com', groups: [], mfa_enabled: false } }));
  await page.route('**/api/auth/mfa/setup', (r) =>
    r.fulfill({ json: { secret: SECRET, provisioning_uri: `otpauth://totp/CUDly:smoke@example.com?secret=${SECRET}` } }));
  await page.route('**/api/auth/mfa/enable', (r) =>
    r.fulfill({ json: { recovery_codes: ['aaaa-bbbb-cccc', 'dddd-eeee-ffff'] } }));
  await page.goto('/home');
  await page.locator('#user-email-display').click();
  await page.locator('#mfa-enable-btn').click();
  await page.locator('#mfa-enroll-password').fill('correct horse battery');
  await page.locator('#mfa-enroll-continue').click();
  await expect(page.locator('#mfa-qr img')).toBeVisible();
}

const style = (page: Page, selector: string) =>
  page.locator(selector).first().evaluate((el) => {
    const cs = getComputedStyle(el);
    return { border: cs.borderTopWidth, font: cs.fontFamily, spacing: cs.letterSpacing, list: cs.listStyleType };
  });

test('the QR step renders as a bordered panel with monospace secret and code input', async ({ page }) => {
  await startEnrollment(page);
  expect((await style(page, '#mfa-flow-host .mfa-panel')).border).toBe('1px');
  expect((await style(page, '.mfa-secret')).font).toMatch(/monospace/);
  const code = await style(page, '#mfa-enroll-code');
  expect(code.font).toMatch(/monospace/);
  expect(code.spacing).not.toBe('normal');
});

test('recovery codes render as an emphasized panel with an unbulleted monospace list', async ({ page }) => {
  await startEnrollment(page);
  await page.locator('#mfa-enroll-code').fill('123456');
  await page.locator('#mfa-qr-verify').click();
  await expect(page.locator('.recovery-codes li')).toHaveCount(2);
  expect((await style(page, '.mfa-panel-emphasis')).border).toBe('2px');
  const list = await style(page, '.recovery-codes');
  expect(list.list).toBe('none');
  expect(list.font).toMatch(/monospace/);
});
