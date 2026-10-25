/**
 * Scoping for the Home "Upcoming Scheduled Purchases" widget (issue #713).
 *
 * GET /api/dashboard/upcoming takes no filter parameters: it is scoped by
 * the session only, so without this the widget kept listing every
 * provider's purchases while the rest of Home followed the topbar chips.
 * Each row carries its plan's provider, so the provider chip is applied
 * here. Rows carry no account, so the account chip cannot be applied
 * client-side; the widget says so instead of silently disagreeing with the
 * rest of the page. Both go away once the endpoint accepts the chip state.
 */

import type { UpcomingPurchase } from '../types';

/** Shown above the rows when account chips are active but cannot apply. */
export const UPCOMING_ACCOUNT_SCOPE_NOTE =
  'The account filter does not apply here: showing upcoming purchases for all accounts you can access.';

function isProviderFilter(provider: string): boolean {
  return provider !== '' && provider.toLowerCase() !== 'all';
}

/**
 * Keep only the rows matching the provider chip. '' / 'all' keep every row.
 * The row's provider is the plan's first service provider (see
 * upcomingFromExecution in internal/api/handler_dashboard.go).
 */
export function scopeUpcomingToProvider(
  purchases: readonly UpcomingPurchase[],
  provider: string,
): UpcomingPurchase[] {
  if (!isProviderFilter(provider)) return [...purchases];
  const want = provider.toLowerCase();
  return purchases.filter(p => (p.provider || '').toLowerCase() === want);
}

/** Empty-state text, naming the provider filter when one hid every row. */
export function upcomingEmptyMessage(provider: string): string {
  return isProviderFilter(provider)
    ? `No upcoming scheduled purchases for ${provider.toUpperCase()}`
    : 'No upcoming scheduled purchases';
}
