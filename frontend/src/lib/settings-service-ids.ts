// Service ids for the Settings page's Azure/GCP per-service defaults and
// per-account overrides (#613).
//
// Recommendations carry the provider's common.ServiceType slug (compute,
// relational-db, nosql, cache, search), and both the scheduler
// (config.ResolveAccountConfigsForRecs) and the purchase modal look up
// service_configs / account_service_overrides rows by that exact
// (provider, service) pair. Settings used to write Azure/GCP rows under
// per-product ids (vm, sql, cosmosdb, redis, memorystore) that no
// recommendation carries, so those rows never applied.

/** Service ids the Settings page writes for Azure and GCP, per provider. */
export const SETTINGS_SERVICE_IDS: Readonly<Record<'azure' | 'gcp', readonly string[]>> = {
  azure: ['compute', 'relational-db', 'nosql', 'cache', 'search'],
  gcp: ['compute', 'relational-db', 'cache'],
};

/** Old Settings ids (`provider/service`) mapped to the id recommendations carry. */
export const LEGACY_SETTINGS_SERVICES: Readonly<Record<string, string>> = {
  'azure/vm': 'compute',
  'azure/sql': 'relational-db',
  'azure/cosmosdb': 'nosql',
  'azure/redis': 'cache',
  'gcp/sql': 'relational-db',
  'gcp/memorystore': 'cache',
};

/** The current id for a row stored under an old Settings id, or null when the id is not a legacy one. */
export function legacySettingsServiceTarget(provider: string, service: string): string | null {
  const key = `${provider}/${service}`;
  return Object.prototype.hasOwnProperty.call(LEGACY_SETTINGS_SERVICES, key) ? LEGACY_SETTINGS_SERVICES[key]! : null;
}

/**
 * Re-keys rows stored under an old Settings id onto the current id so the
 * matching card shows the stored values and the next Save writes them under
 * the id the backend matches. A row already stored under the current id wins
 * over its legacy twin, which is dropped. Other rows pass through unchanged.
 */
export function canonicalizeServiceConfigs<T extends { provider: string; service: string }>(rows: readonly T[]): T[] {
  const present = new Set(rows.map(r => `${r.provider}/${r.service}`));
  const out: T[] = [];
  for (const row of rows) {
    const target = legacySettingsServiceTarget(row.provider, row.service);
    if (target === null) {
      out.push(row);
    } else if (!present.has(`${row.provider}/${target}`)) {
      present.add(`${row.provider}/${target}`);
      out.push({ ...row, service: target });
    }
  }
  return out;
}
