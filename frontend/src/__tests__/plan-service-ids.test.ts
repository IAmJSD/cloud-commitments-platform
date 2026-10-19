import * as fs from 'fs';
import * as path from 'path';
import { CANONICAL_PLAN_SERVICES, LEGACY_PLAN_SERVICES, isSavingsPlanService, resolvePlanService } from '../lib/plan-service-ids';

test.each([
  ['azure', 'vm', 'compute'], ['azure', 'sql', 'relational-db'], ['azure', 'redis', 'cache'],
  ['gcp', 'cloudsql', 'relational-db'], ['gcp', 'memorystore', 'cache'],
])('treats legacy %s/%s as the same service as canonical %s', (provider, service, canonical) => {
  expect(resolvePlanService(provider, service)).toEqual({ state: 'legacy', canonical });
});

test('maps exactly the five legacy slugs', () => {
  expect(Object.keys(LEGACY_PLAN_SERVICES).sort()).toEqual(['azure/redis', 'azure/sql', 'azure/vm', 'gcp/cloudsql', 'gcp/memorystore']);
});

test('keeps every canonical slug canonical, per provider only', () => {
  for (const [provider, services] of Object.entries(CANONICAL_PLAN_SERVICES)) {
    for (const service of services) expect(resolvePlanService(provider, service)).toEqual({ state: 'canonical', canonical: service });
  }
  expect(resolvePlanService('aws', 'search')).toEqual({ state: 'unsupported', canonical: null });
  expect(resolvePlanService('gcp', 'search')).toEqual({ state: 'unsupported', canonical: null });
});

test.each([
  ['oracle', 'compute'], ['aws', 'vm'], ['azure', 'unknown-opaque'], ['aws', 'EC2'], ['Azure', 'compute'], ['azure', 'VM'], ['azure', ' vm'],
  ['aws', 'vm/'], ['constructor', 'compute'], ['aws', 'toString'], ['', ''],
])('never guesses or normalizes %s/%s', (provider, service) => {
  expect(resolvePlanService(provider, service)).toEqual({ state: 'unsupported', canonical: null });
});

test('identifies exactly the four Savings Plans slugs', () => {
  const flagged = Object.values(CANONICAL_PLAN_SERVICES).flat().filter(isSavingsPlanService);
  expect(flagged).toEqual(['savings-plans-compute', 'savings-plans-ec2instance', 'savings-plans-sagemaker', 'savings-plans-database']);
  expect(isSavingsPlanService('ec2')).toBe(false);
  expect(isSavingsPlanService('savings-plans-unknown')).toBe(false);
});

test('canonical set equals the #plan-service options in index.html per provider', () => {
  const html = fs.readFileSync(path.join(__dirname, '..', 'index.html'), 'utf8');
  const body = new DOMParser().parseFromString(html, 'text/html').body;
  const offered: Record<string, string[]> = {};
  body.querySelectorAll<HTMLOptGroupElement>('#plan-service > optgroup').forEach(group => {
    const provider = group.dataset['provider']!;
    (offered[provider] ??= []).push(...Array.from(group.querySelectorAll('option')).map(option => option.value));
  });
  expect(offered).toEqual(CANONICAL_PLAN_SERVICES);
});
