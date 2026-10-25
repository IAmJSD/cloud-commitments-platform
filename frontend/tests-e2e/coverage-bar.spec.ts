/**
 * Coverage-bar rendering smoke for Inventory & Coverage -> Coverage (issue #1777).
 *
 * The reported failure is "the Coverage bar column renders empty". The markup
 * was always there: buildServiceRow() appends
 * `div.coverage-bar > div.coverage-bar-fill` with an inline `width: <pct>%`.
 * What was missing were the CSS rules for those two classes, so both divs
 * resolved to height 0 with no background and the cell looked blank.
 *
 * That is invisible to jsdom, which does not resolve stylesheets into layout:
 * the jest suite asserts on the DOM structure and stays green while the user
 * sees nothing. The assertions below therefore run against the real bundle in
 * Chromium and measure what the browser actually paints -- box height and
 * background colour -- not what the DOM contains.
 */

import { test, expect, type Page, type Locator } from '@playwright/test';
import { mockApi, seedAuth, COVERAGE } from './fixtures/recs';

/** The AWS section is the only one the fixture gives services to. */
const AWS_TABLE = '.coverage-provider-card:has(h3:text-is("AWS")) .coverage-service-table';

interface PaintedBox {
  width: number;
  height: number;
  background: string;
}

/** What Chromium actually paints for an element. */
async function painted(locator: Locator): Promise<PaintedBox> {
  return locator.evaluate((el) => {
    const rect = el.getBoundingClientRect();
    return {
      width: rect.width,
      height: rect.height,
      background: getComputedStyle(el).backgroundColor,
    };
  });
}

/**
 * A colour the user can see. Chromium reports every resolved background as
 * `rgb(...)` or `rgba(...)`, and an unset one as `rgba(0, 0, 0, 0)`, so the
 * check reduces to "opaque, or alpha above zero".
 */
function isVisibleColour(background: string): boolean {
  const rgba = /^rgba\(\s*[\d.]+\s*,\s*[\d.]+\s*,\s*[\d.]+\s*,\s*([\d.]+)\s*\)$/.exec(background);
  if (rgba) return Number(rgba[1]) > 0;
  return background.startsWith('rgb(');
}

async function openCoverage(page: Page): Promise<void> {
  await seedAuth(page);
  await mockApi(page);
  await page.goto('/inventory/coverage');
  await expect(page.locator(`${AWS_TABLE} tbody tr`)).toHaveCount(
    COVERAGE.providers[0]!.services!.length,
  );
}

/**
 * Rows carrying a numeric coverage_pct, in fixture order. The specs below
 * loop over this, so an empty list would let them pass without asserting
 * anything -- pinned here once rather than in each test.
 */
const NUMERIC_ROWS = COVERAGE.providers[0]!.services!.filter(
  (s): s is typeof s & { coverage_pct: number } => s.coverage_pct !== null,
);

test('the fixture supplies rows to assert on', () => {
  expect(NUMERIC_ROWS.map((s) => s.coverage_pct)).toEqual([100, 62.5, 0]);
});

test('every row with a coverage percentage paints a visible bar', async ({ page }) => {
  await openCoverage(page);

  for (const [index, service] of NUMERIC_ROWS.entries()) {
    const cell = page.locator(`${AWS_TABLE} tbody tr`).nth(index).locator('.coverage-bar-cell');
    const track = cell.locator('.coverage-bar');
    const fill = track.locator('.coverage-bar-fill');

    await expect(track, `${service.service}: track present`).toHaveCount(1);

    const trackBox = await painted(track);
    const fillBox = await painted(fill);

    // The defect: both divs existed but collapsed to height 0, so the cell
    // was blank regardless of the percentage.
    expect(trackBox.height, `${service.service}: track height`).toBeGreaterThan(0);
    expect(trackBox.width, `${service.service}: track width`).toBeGreaterThan(0);
    expect(fillBox.height, `${service.service}: fill height`).toBeGreaterThan(0);

    // A zero-height box is the obvious failure; a transparent one is the
    // same outcome for the user.
    expect(
      isVisibleColour(trackBox.background),
      `${service.service}: track background ${trackBox.background}`,
    ).toBe(true);
    expect(
      isVisibleColour(fillBox.background),
      `${service.service}: fill background ${fillBox.background}`,
    ).toBe(true);
  }
});

test('bar fill width tracks the coverage percentage, including 0% and 100%', async ({ page }) => {
  await openCoverage(page);

  for (const [index, service] of NUMERIC_ROWS.entries()) {
    const row = page.locator(`${AWS_TABLE} tbody tr`).nth(index);
    const track = row.locator('.coverage-bar');
    const fill = track.locator('.coverage-bar-fill');

    const trackBox = await painted(track);
    const fillBox = await painted(fill);
    const ratio = (fillBox.width / trackBox.width) * 100;

    // Precision 0 is a half-point tolerance, enough to absorb sub-pixel
    // rounding on the fractional row (62.5%) without letting a wrong bar pass.
    expect(ratio, `${service.service}: fill ratio`).toBeCloseTo(service.coverage_pct, 0);

    // The numeric column and the bar must agree.
    await expect(row.locator('td').nth(3)).toHaveText(`${service.coverage_pct.toFixed(1)}%`);
  }
});

test('a row with no coverage figure renders no bar rather than an empty 0% bar', async ({ page }) => {
  await openCoverage(page);

  const absentIndex = COVERAGE.providers[0]!.services!.findIndex((s) => s.coverage_pct === null);
  expect(absentIndex, 'fixture carries a null-coverage row').toBeGreaterThanOrEqual(0);

  const row = page.locator(`${AWS_TABLE} tbody tr`).nth(absentIndex);
  await expect(row.locator('td').nth(3)).toHaveText('N/A');

  // Absent is not zero: no track, so the row cannot be read as "0% covered".
  await expect(row.locator('.coverage-bar')).toHaveCount(0);

  // The cell still says something, so it is distinguishable from the defect
  // this spec pins (a cell that is blank because the CSS is missing).
  const placeholder = row.locator('.coverage-bar-cell .coverage-bar-absent');
  await expect(placeholder).toHaveCount(1);
  await expect(placeholder).toHaveText('N/A');
});

/**
 * Issue #726: the fill was success-green at every value, so 0% read like
 * 100%. With a known target (the /api/config default here) the fill is
 * neutral on track, red far below target, and a tick marks the target.
 */
test('the bar colour follows distance from the coverage target, with a target tick', async ({ page }) => {
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/config', (route) => route.fulfill({ json: { global: { default_coverage: 80 }, services: [] } }));
  await page.goto('/inventory/coverage');
  const rows = page.locator(`${AWS_TABLE} tbody tr`);
  await expect(rows).toHaveCount(COVERAGE.providers[0]!.services!.length);

  const fills: string[] = [];
  for (const [index, service] of NUMERIC_ROWS.entries()) {
    const track = rows.nth(index).locator('.coverage-bar');
    fills.push((await painted(track.locator('.coverage-bar-fill'))).background);

    // The tick sits at 80% of the track and stands taller than it.
    const trackBox = (await track.boundingBox())!;
    const tickBox = (await track.locator('.coverage-bar-target').boundingBox())!;
    const tickCentre = ((tickBox.x + tickBox.width / 2 - trackBox.x) / trackBox.width) * 100;
    expect(tickCentre, `${service.service}: tick position`).toBeCloseTo(80, 0);
    expect(tickBox.height, `${service.service}: tick height`).toBeGreaterThan(trackBox.height);
    await expect(track).toHaveAttribute('aria-label', new RegExp(`target 80%`));
  }

  const [full, mid, zero] = fills;
  // 100% and 62.5% (0.78 of target) are on track and share the neutral fill;
  // 0% is far below and must look different.
  expect(mid).toBe(full);
  expect(zero).not.toBe(full);
  // None of them is the old success green (--cudly-success #15803d).
  expect(fills).not.toContain('rgb(21, 128, 61)');
});
