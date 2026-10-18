export const CANONICAL_PLAN_SERVICES: Readonly<Record<'aws' | 'azure' | 'gcp', readonly string[]>> = {
  aws: ['ec2', 'rds', 'elasticache', 'opensearch', 'memorydb', 'redshift',
    'savings-plans-compute', 'savings-plans-ec2instance', 'savings-plans-sagemaker', 'savings-plans-database'],
  azure: ['compute', 'relational-db', 'cache', 'search'],
  gcp: ['compute', 'relational-db', 'cache'],
};

export const LEGACY_PLAN_SERVICES: Readonly<Record<string, string>> = {
  'azure/vm': 'compute',
  'azure/sql': 'relational-db',
  'azure/redis': 'cache',
  'gcp/cloudsql': 'relational-db',
  'gcp/memorystore': 'cache',
};

export interface PlanServiceResolution {
  readonly state: 'canonical' | 'legacy' | 'unsupported';
  readonly canonical: string | null;
}

const own = <T>(record: Readonly<Record<string, T>>, key: string): T | undefined =>
  Object.prototype.hasOwnProperty.call(record, key) ? record[key] : undefined;

export const isSavingsPlanService = (service: string): boolean =>
  CANONICAL_PLAN_SERVICES.aws.includes(service) && service.startsWith('savings-plans-');

export const resolvePlanService = (provider: string, service: string): PlanServiceResolution => {
  if (own(CANONICAL_PLAN_SERVICES, provider)?.includes(service)) return { state: 'canonical', canonical: service };
  const legacy = own(LEGACY_PLAN_SERVICES, `${provider}/${service}`);
  return legacy === undefined ? { state: 'unsupported', canonical: null } : { state: 'legacy', canonical: legacy };
};
