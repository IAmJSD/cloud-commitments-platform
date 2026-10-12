/**
 * Archera comparison section on the education page (issue #785).
 * Fixture-based: API responses are mocked with page.route using the Go DTO
 * golden file, so this proves the render path only. No live Archera calls.
 */
import { test, expect, type Page } from '@playwright/test';
import * as fs from 'fs';
import * as path from 'path';
import { mockApi, seedAuth } from './fixtures/recs';

const golden = JSON.parse(fs.readFileSync(
  path.resolve(__dirname, '../../internal/archera/testdata/comparison.golden.json'), 'utf8'));

async function setup(page: Page, statusBody: unknown, statusCode = 200) {
  const errors: string[] = [];
  const comparisonCalls: string[] = [];
  page.on('pageerror', e => errors.push(e.message));
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/insurance/status', r => r.fulfill({ status: statusCode, json: statusBody }));
  await page.route('**/api/insurance/comparison', r => {
    comparisonCalls.push(r.request().url());
    return r.fulfill({ json: golden });
  });
  return { errors, comparisonCalls };
}

test('not configured: no action and no comparison request', async ({ page }) => {
  const { errors, comparisonCalls } = await setup(page, { configured: false, missing: ['ARCHERA_ORG_ID'] });
  await page.goto('/archera-insurance');
  await expect(page.locator('.archera-page-inner h1')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Compare with Archera' })).toHaveCount(0);
  expect(comparisonCalls).toEqual([]);
  expect(errors).toEqual([]);
});

test('configured: compare, disclosures, refresh', async ({ page }) => {
  const { errors, comparisonCalls } = await setup(page, { configured: true, missing: [] });
  await page.goto('/archera-insurance');
  const button = page.getByRole('button', { name: 'Compare with Archera' });
  await button.scrollIntoViewIfNeeded();
  expect(comparisonCalls).toEqual([]);
  await button.click();
  await expect(page.getByRole('heading', { name: golden.title })).toBeVisible();
  await expect(page.getByText(golden.non_gating_disclosure)).toBeVisible();
  await expect(page.getByText(golden.sponsorship_disclosure)).toBeVisible();
  await expect(page.locator('.archera-fetched-at')).toContainText('Fetched at');
  expect(comparisonCalls).toHaveLength(1);
  const refresh = page.getByRole('button', { name: 'Refresh' });
  await expect(refresh).toBeFocused();
  await refresh.click();
  await expect.poll(() => comparisonCalls.length).toBe(2);
  expect(errors).toEqual([]);
});

test('hostile vendor string stays text and 429 shows the wait', async ({ page }) => {
  const { errors } = await setup(page, { configured: true, missing: [] });
  const hostile = JSON.parse(JSON.stringify(golden));
  hostile.rows[0].current.offer_id = '<img src=x onerror=window.__xss=1>';
  await page.route('**/api/insurance/comparison', r => r.fulfill({ json: hostile }));
  await page.goto('/archera-insurance');
  await page.getByRole('button', { name: 'Compare with Archera' }).click();
  await expect(page.getByText('<img src=x onerror=window.__xss=1>').first()).toBeVisible();
  expect(await page.evaluate(() => (window as unknown as { __xss?: number }).__xss)).toBeUndefined();

  await page.route('**/api/insurance/comparison', r => r.fulfill({
    status: 429, headers: { 'Retry-After': '30' },
    json: { error: 'Archera rate limit reached', retry_after_seconds: 30 },
  }));
  await page.getByRole('button', { name: 'Refresh' }).click();
  await expect(page.locator('.archera-comparison [aria-live="polite"]')).toContainText('Retry after 30 seconds');
  await expect(page.getByRole('button', { name: 'Refresh' })).toBeEnabled();
  expect(errors).toEqual([]);
});

test('mobile width: comparison does not widen the page', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 800 });
  await setup(page, { configured: true, missing: [] });
  await page.goto('/archera-insurance');
  await page.getByRole('button', { name: 'Compare with Archera' }).click();
  await expect(page.getByRole('heading', { name: golden.title })).toBeVisible();
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(overflow).toBeLessThanOrEqual(0);
});
