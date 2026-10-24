/**
 * Page-level horizontal scroll guard (issue #703).
 *
 * A wide table, a fieldset's min-content width, a hidden-but-laid-out
 * tooltip or a non-wrapping toolbar each push the document wider than the
 * viewport, so the whole page scrolls sideways. jsdom has no layout, so this
 * runs in Chromium and compares the document's scroll width to the viewport.
 */

import { test, expect } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

const GROUPS = ['Administrators', 'Purchasers', 'Read-only finance auditors', 'Platform engineering leads'].map((name, i) => ({
  id: `00000000-0000-5000-8000-00000000000${i + 1}`,
  name,
  description: `${name}: a deliberately long description so the group card header has to share its row with the action buttons`,
  permissions: [{ action: i === 0 ? 'admin' : 'view', resource: i === 0 ? '*' : 'recommendations' }],
}));
const USERS = [1, 2, 3].map((n) => ({
  id: `user-${n}`,
  email: `long.address.for.user.number.${n}@subdomain.example-company.com`,
  groups: GROUPS.map((g) => g.id),
  mfa_enabled: n % 2 === 0,
  created_at: '2026-07-01T00:00:00Z',
}));

const PAGES = [
  '/home',
  '/opportunities',
  '/plans',
  '/purchases',
  '/inventory',
  '/inventory/trends',
  '/admin/general',
  '/admin/purchasing',
  '/admin/accounts',
  '/admin/users',
];
const WIDTHS = [1280, 768, 390];

for (const width of WIDTHS) {
  for (const path of PAGES) {
    test(`${path} does not scroll horizontally at ${width}px`, async ({ page }) => {
      await page.setViewportSize({ width, height: 900 });
      await seedAuth(page);
      await mockApi(page);
      // Populated users and groups: empty lists hide the group cards, the
      // permission matrix and the long user rows that overflow on phones.
      await page.route('**/api/users', (route) =>
        route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ users: USERS }) }),
      );
      await page.route('**/api/groups', (route) =>
        route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ groups: GROUPS }) }),
      );
      await page.goto(path);
      await page.waitForLoadState('networkidle');
      const overflow = await page.evaluate(
        () => document.documentElement.scrollWidth - window.innerWidth,
      );
      expect(overflow).toBeLessThanOrEqual(0);
    });
  }
}
