/**
 * Issue #180: Home and Opportunities both show "Potential Monthly Savings"
 * for the same recommendations, so the two figures must agree. Opportunities
 * honors Settings -> Enabled Providers (#463); Home used to sum every
 * provider, so a tenant with a provider switched off saw two different
 * numbers. When the Opportunities card is narrowed by column filters, the
 * scope difference must be labeled on the card itself.
 */
import { test, expect, type Page } from '@playwright/test';
import { mockApi, seedAuth, RECS } from './fixtures/recs';

// Every fixture row is its own cell with a single variant, so each page-level
// range collapses to the plain sum of the enabled rows.
const AWS_TOTAL = RECS.filter((r) => r.provider === 'aws').reduce((s, r) => s + r.savings, 0);
const AWS_COUNT = RECS.filter((r) => r.provider === 'aws').length;
const AWS_EC2_COUNT = RECS.filter((r) => r.provider === 'aws' && r.service === 'ec2').length;
const usd = (n: number): string => `$${n.toLocaleString('en-US')}`;

async function setup(page: Page): Promise<void> {
  await seedAuth(page);
  await mockApi(page);
  // Azure and GCP are switched off in Settings.
  await page.route('**/api/config', (route) =>
    route.fulfill({ json: { global: { enabled_providers: ['aws'] }, services: {} } }),
  );
  await page.route('**/api/dashboard/upcoming', (route) => route.fulfill({ json: { purchases: [] } }));
  await page.route('**/api/dashboard/summary**', (route) =>
    route.fulfill({
      json: {
        potential_monthly_savings: 0, total_recommendations: RECS.length, active_commitments: 0,
        committed_monthly: 0, current_coverage: 0, target_coverage: 80, ytd_savings: 0, by_service: {},
      },
    }),
  );
}

function opportunitiesSavingsCard(page: Page) {
  return page.locator('#recommendations-summary .card').filter({ hasText: 'Potential Monthly Savings' });
}

test('Home and Opportunities agree on Potential Monthly Savings when a provider is disabled', async ({ page }) => {
  await setup(page);

  await page.goto('/home');
  const homeTile = page.locator('#summary [data-kpi="savings"]');
  await expect(homeTile.locator('.kpi-tile-value')).toHaveText(usd(AWS_TOTAL));
  await expect(homeTile.locator('.kpi-tile-detail')).toHaveText(`${AWS_COUNT} recommendations`);

  await page.goto('/opportunities');
  const card = opportunitiesSavingsCard(page);
  await expect(card.locator('.value.savings')).toHaveText(usd(AWS_TOTAL));
  // Unfiltered: same scope as Home, so no scope label.
  await expect(card.getByTestId('savings-scope')).toHaveCount(0);
});

test('a column-filtered Opportunities card labels its narrower scope', async ({ page }) => {
  await page.addInitScript(() => {
    localStorage.setItem('cudly.recs.columnFilters.v1', JSON.stringify({
      schemaVersion: 1,
      filters: { service: { kind: 'set', values: ['ec2'] } },
    }));
  });
  await setup(page);

  await page.goto('/opportunities');
  await expect(opportunitiesSavingsCard(page).getByTestId('savings-scope'))
    .toHaveText(`Filtered: ${AWS_EC2_COUNT} of ${AWS_COUNT} recommendations`);
});

test('the card stays labeled when the service filter is sent to the server', async ({ page }) => {
  // The persisted filter is seeded after the first fetch, so only a later
  // load (here: leaving and returning to Opportunities) sends ?service=ec2.
  // The server then returns only ec2 rows and the counts match, but the card
  // still covers less than Home's figure.
  await page.addInitScript(() => {
    localStorage.setItem('cudly.recs.columnFilters.v1', JSON.stringify({
      schemaVersion: 1,
      filters: { service: { kind: 'set', values: ['ec2'] } },
    }));
  });
  await setup(page);

  await page.goto('/opportunities');
  const scope = opportunitiesSavingsCard(page).getByTestId('savings-scope');
  await expect(scope).toHaveText(`Filtered: ${AWS_EC2_COUNT} of ${AWS_COUNT} recommendations`);

  await page.locator('#home-tab-btn').click();
  const pushed = page.waitForRequest((req) =>
    req.url().includes('/api/recommendations') && new URL(req.url()).searchParams.get('service') === 'ec2');
  await page.locator('#opportunities-tab-btn').click();
  await pushed;
  await expect(scope).toHaveText(`Filtered: ${AWS_EC2_COUNT} recommendations (Service: ec2)`);
});
