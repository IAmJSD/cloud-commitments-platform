import { serviceLabel } from '../lib/service-labels';

// Issues #1649 / #1683: one display name per service slug for every surface.
describe('serviceLabel', () => {
  test.each([
    ['ec2', 'EC2'],
    ['rds', 'RDS'],
    ['relational-db', 'Relational DB'],
    ['data-warehouse', 'Data Warehouse'],
    ['compute', 'Compute'],
    ['savings-plans-compute', 'Compute Savings Plans'],
    ['savingsplans', 'Savings Plans'],
  ])('%s -> %s', (slug, label) => {
    expect(serviceLabel(slug)).toBe(label);
  });

  test('compact form abbreviates Savings Plans', () => {
    expect(serviceLabel('savings-plans-ec2instance', true)).toBe('EC2 Instance SP');
    expect(serviceLabel('rds', true)).toBe('RDS');
  });

  test('unknown slugs pass through unchanged', () => {
    expect(serviceLabel('some-new-service')).toBe('some-new-service');
  });
});
