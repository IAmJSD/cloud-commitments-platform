// Display names for service slugs, shared by every page that shows a service
// (issues #1649, #1683). Slugs shared across providers (compute, cache,
// relational-db) get a provider-neutral name; the Provider column next to them
// carries the provider. Unknown slugs pass through unchanged. A Go test
// (internal/config/service_labels_frontend_test.go) checks every library
// ServiceType has an entry here.
const SERVICE_LABELS: Readonly<Record<string, string>> = {
  ec2: 'EC2',
  rds: 'RDS',
  elasticache: 'ElastiCache',
  opensearch: 'OpenSearch',
  memorydb: 'MemoryDB',
  redshift: 'Redshift',
  'savings-plans-compute': 'Compute Savings Plans',
  'savings-plans-ec2instance': 'EC2 Instance Savings Plans',
  'savings-plans-sagemaker': 'SageMaker Savings Plans',
  'savings-plans-database': 'Database Savings Plans',
  savingsplans: 'Savings Plans',
  compute: 'Compute',
  'relational-db': 'Relational DB',
  cache: 'Cache',
  search: 'Search',
  nosql: 'NoSQL',
  'data-warehouse': 'Data Warehouse',
  storage: 'Storage',
  commitments: 'Commitments',
  other: 'Other',
};

/** Display name for a service slug; `compact` abbreviates "Savings Plans" to "SP". */
export function serviceLabel(slug: string, compact = false): string {
  const label = SERVICE_LABELS[slug] ?? slug;
  return compact ? label.replace(/ Savings Plans$/, ' SP') : label;
}
