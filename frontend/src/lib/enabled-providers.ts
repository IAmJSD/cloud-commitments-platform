/**
 * Settings -> General -> Enabled Providers scoping for recommendation lists
 * (issues #463 and #180).
 *
 * The backend returns every collected recommendation irrespective of the
 * `enabled_providers` preference, so each screen that sums recommendations
 * must apply the same client-side filter. Home and Opportunities both use
 * this helper so their "Potential Monthly Savings" figures are computed
 * from the same set of rows and cannot drift apart.
 *
 * Permissive default: an empty or absent list (older configs, or a tenant
 * that never touched the toggles) means "no filter", matching the Settings
 * load path, rather than hiding every recommendation.
 */

/**
 * Keep only the recommendations whose provider is enabled. Returns a copy of
 * the input when `enabledProviders` is empty, null or undefined.
 */
export function filterByEnabledProviders<T extends { provider: string }>(
  recs: readonly T[],
  enabledProviders: readonly string[] | null | undefined,
): T[] {
  if (!enabledProviders || enabledProviders.length === 0) return [...recs];
  return recs.filter((r) => enabledProviders.includes(r.provider));
}
