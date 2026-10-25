/**
 * Issue #1903: which priced variant a single-bucket purchase modal submits.
 *
 * Legacy rows whose payment is missing or unrecognized must resolve to a
 * complete priced sibling (or be excluded) before submission, and the
 * purchase identity (platform, tenancy, AZ, engine, plan type, ...) limits
 * which variants the Term/Payment selects may swap to. These tests drive
 * app.ts and recommendations.ts and assert that the displayed variant
 * matches the body passed to api.executePurchase. Shared mocks, fixtures
 * and the per-test reset live in purchase-modal-harness.ts.
 */

// The harness registers the module mocks, so it must be imported first.
import {
  buildRows,
  deferred,
  flush,
  setupPurchaseModalTest,
} from './purchase-modal-harness';
import { handleExecutePurchase } from '../app';
import * as api from '../api';
import * as state from '../state';
import { openModal } from '../modal';
import {
  openPurchaseModal,
  getPurchaseModalRecommendations,
  clearPurchaseModalRecommendations,
  getFanOutBuckets,
  loadRecommendations,
  seedGlobalDefaults,
} from '../recommendations';
import { formatCurrency } from '../utils';
import type { LocalRecommendation } from '../types';

beforeEach(setupPurchaseModalTest);

describe('Issue #1903: purchase modal re-prices on Term/Payment change', () => {
  test.each(['active', 'closed', 'executed'])('legacy payment delayed open cannot restore rows after newer modal is %s', async (action) => {
    const firstFetch = deferred<Awaited<ReturnType<typeof api.listAccountServiceOverrides>>>();
    (api.listAccountServiceOverrides as jest.Mock)
      .mockReturnValueOnce(firstFetch.promise)
      .mockResolvedValueOnce([]);
    const first = { ...buildRows()[0]!, id: 'first', resource_type: 'c5.large' };
    const second = { ...buildRows()[1]!, id: 'second', resource_type: 'm6i.large' };
    (state.getRecommendations as jest.Mock).mockReturnValue([first, second]);

    const pendingFirst = openPurchaseModal([first]);
    await openPurchaseModal([second]);
    expect(getPurchaseModalRecommendations()).toEqual([second]);
    if (action === 'closed') {
      (document.getElementById('close-purchase-modal-btn') as HTMLButtonElement).click();
      // closePurchaseModal and modal.ts are mocked in this file, so run the
      // onClose the real closeModal would (the close button no longer clears
      // purchase state itself, issue #414).
      const opens = (openModal as jest.Mock).mock.calls;
      (opens[opens.length - 1]![1] as { onClose: () => void }).onClose();
    }
    if (action === 'executed') await handleExecutePurchase();
    const rendered = document.getElementById('purchase-details')!.innerHTML;
    const openCount = (openModal as jest.Mock).mock.calls.length;

    firstFetch.resolve([]);
    await pendingFirst;

    expect(getPurchaseModalRecommendations()).toEqual(action === 'active' ? [second] : []);
    expect(document.getElementById('purchase-details')!.innerHTML).toBe(rendered);
    expect(openModal).toHaveBeenCalledTimes(openCount);
    await handleExecutePurchase();
    if (action === 'closed') {
      expect(api.executePurchase).not.toHaveBeenCalled();
    } else {
      expect(api.executePurchase).toHaveBeenCalledTimes(1);
      expect(api.executePurchase).toHaveBeenCalledWith([expect.objectContaining(second)], 100, undefined);
    }
  });

  test.each([undefined, '', 'unrecognized'])('legacy payment %j resolves a complete priced variant before submission', async (payment) => {
    const details = { platform: 'Linux/UNIX', tenancy: 'default', scope: 'Region' };
    const legacy = { ...buildRows()[0]!, id: 'legacy', payment, upfront_cost: 17, monthly_cost: 29, details };
    const priced = { ...buildRows()[0]!, count: 4, details: { ...details, vcpu: 2 } };
    (state.getRecommendations as jest.Mock).mockReturnValue([legacy, priced]);

    await openPurchaseModal([legacy]);

    const row = document.querySelector<HTMLTableRowElement>('.purchase-modal-table tbody tr')!;
    expect(row.cells[4]!.textContent).toBe('4');
    expect(row.cells[5]!.textContent).toBe(formatCurrency(priced.upfront_cost));
    expect(row.cells[6]!.textContent).toBe(formatCurrency(priced.monthly_cost!));
    expect(getPurchaseModalRecommendations()).toEqual([expect.objectContaining(priced)]);
    await handleExecutePurchase();
    expect(api.executePurchase).toHaveBeenCalledWith([expect.objectContaining(priced)], 100, undefined);
  });

  test.each([true, false])('legacy payment uses configured preference only when priced (available: %s)', async (available) => {
    const legacy = { ...buildRows()[0]!, id: 'legacy', payment: '' };
    const all = buildRows()[0]!;
    const partial = buildRows()[1]!;
    seedGlobalDefaults(3, 'partial-upfront');
    (state.getRecommendations as jest.Mock).mockReturnValue(available ? [legacy, all, partial] : [legacy, all]);

    await openPurchaseModal([legacy]);

    const expected = available ? partial : all;
    expect(getPurchaseModalRecommendations()).toEqual([expect.objectContaining(expected)]);
    expect(document.querySelector<HTMLSelectElement>('.purchase-row-payment')!.value).toBe(expected.payment);
  });

  test.each([false, true])('legacy payment resolves with account override fetch failure: %s', async (failed) => {
    const legacy = { ...buildRows()[0]!, id: 'legacy', payment: '' };
    (state.getRecommendations as jest.Mock).mockReturnValue([legacy, ...buildRows()]);
    if (failed) {
      (api.listAccountServiceOverrides as jest.Mock).mockRejectedValue(new Error('offline'));
    } else {
      (api.listAccountServiceOverrides as jest.Mock).mockResolvedValue([
        { id: 'ovr', account_id: 'a1', provider: 'aws', service: 'ec2', payment: 'partial-upfront' },
      ]);
    }

    await openPurchaseModal([legacy]);

    expect(getPurchaseModalRecommendations()[0]).toMatchObject(buildRows()[failed ? 0 : 1]!);
    expect(document.querySelector('.purchase-row-payment-source') !== null).toBe(!failed);
  });

  test.each([false, true])('legacy payment excludes an unavailable row (zero at capacity: %s)', async (zero) => {
    const legacy = { ...buildRows()[0]!, id: 'legacy', payment: '' };
    const priced = { ...buildRows()[0]!, count: 1 };
    (state.getRecommendations as jest.Mock).mockReturnValue(zero ? [legacy, priced] : [legacy]);

    await openPurchaseModal([legacy], zero ? 50 : 100);

    const notice = document.querySelector('.purchase-modal-unavailable');
    expect(notice?.getAttribute('role')).toBe('alert');
    for (const label of ['a1', 'ec2', 'm5.large', 'us-east-1', 'excluded']) expect(notice?.textContent).toContain(label);
    expect(getPurchaseModalRecommendations()).toEqual([]);
    expect(document.querySelectorAll('.purchase-modal-table tbody tr')).toHaveLength(0);
    expect((document.getElementById('execute-purchase-btn') as HTMLButtonElement).disabled).toBe(true);
    await handleExecutePurchase();
    expect(api.executePurchase).not.toHaveBeenCalled();

    clearPurchaseModalRecommendations();
    await openPurchaseModal([priced]);
    expect(document.querySelector('.purchase-modal-unavailable')).toBeNull();
    expect(getPurchaseModalRecommendations()).toEqual([expect.objectContaining(priced)]);
  });

  test('legacy payment skips a zero-unit preferred variant and scales the viable fallback once', async () => {
    const legacy = { ...buildRows()[0]!, id: 'legacy', payment: '', count: 1, recommended_count: 2 };
    const all = { ...buildRows()[0]!, count: 1 };
    const partial = buildRows()[1]!;
    (state.getRecommendations as jest.Mock).mockReturnValue([legacy, all, partial]);

    await openPurchaseModal([legacy], 50);

    expect(getPurchaseModalRecommendations()).toEqual([expect.objectContaining({
      ...partial, count: 1, recommended_count: 2, upfront_cost: 9000, monthly_cost: 150, savings: 425,
    })]);
  });

  test('legacy payment mixed bulk selection excludes unpriced rows from totals, selection and POST', async () => {
    const legacy = { ...buildRows()[0]!, id: 'legacy', payment: '' };
    const unavailable = { ...legacy, id: 'unavailable', resource_type: 'm6i.large' };
    const priced = buildRows()[0]!;
    const rows = [legacy, unavailable, priced];
    (api.getRecommendations as jest.Mock).mockResolvedValue({ summary: {}, recommendations: rows, regions: [] });
    (state.getRecommendations as jest.Mock).mockReturnValue(rows);
    (state.getVisibleRecommendations as jest.Mock).mockReturnValue(rows);
    (state.getSelectedRecommendationIDs as jest.Mock).mockReturnValue(new Set([legacy.id, unavailable.id]));

    await loadRecommendations();
    (document.getElementById('bulk-purchase-btn') as HTMLButtonElement).click();
    await flush();

    expect(getFanOutBuckets()).toBeNull();
    expect(document.querySelector('.purchase-modal-unavailable')?.textContent).toContain('m6i.large');
    expect(document.querySelectorAll('.purchase-modal-table tbody tr')).toHaveLength(1);
    expect(document.getElementById('purchase-modal-totals-row')?.textContent).toContain(formatCurrency(priced.upfront_cost));
    const selectAll = document.getElementById('purchase-modal-select-all') as HTMLInputElement;
    selectAll.click();
    expect(getPurchaseModalRecommendations()).toEqual([]);
    selectAll.click();
    expect(getPurchaseModalRecommendations()).toEqual([expect.objectContaining(priced)]);
    (document.getElementById('execute-mode-direct') as HTMLInputElement).click();
    expect(document.querySelector('.direct-execute-warning')?.textContent).toContain('36,000.00');
    await handleExecutePurchase();
    expect(api.executePurchase).toHaveBeenCalledWith([expect.objectContaining(priced)], 100, 'direct');
  });

  test('legacy payment normalization preserves a valid Azure upfront price', async () => {
    const rec: LocalRecommendation = { ...buildRows()[0]!, provider: 'azure', service: 'compute', resource_type: 'Standard_D2s_v3', region: 'eastus', payment: 'upfront' };
    (state.getRecommendations as jest.Mock).mockReturnValue([rec]);

    await openPurchaseModal([rec]);

    expect(getPurchaseModalRecommendations()).toEqual([{ ...rec, payment: 'all-upfront' }]);
    await handleExecutePurchase();
    expect(api.executePurchase).toHaveBeenCalledWith([expect.objectContaining({ ...rec, payment: 'all-upfront' })], 100, undefined);
  });

  test.each<[string, string, string, Record<string, unknown>, unknown]>([
    ['ec2', 'platform', 'm5.large', { instance_type: 'm5.large', platform: 'Linux/UNIX', tenancy: 'default', scope: 'Region' }, 'Windows'],
    ['ec2', 'tenancy', 'm5.large', { instance_type: 'm5.large', platform: 'Linux/UNIX', tenancy: 'default', scope: 'Region' }, 'dedicated'],
    ['ec2', 'availability_zone', 'm5.large', { instance_type: 'm5.large', platform: 'Linux/UNIX', tenancy: 'default', scope: 'Availability Zone', availability_zone: 'us-east-1a' }, 'us-east-1b'],
    ['compute', 'availability_zone', 'm5.large', { instance_type: 'm5.large', platform: 'Linux/UNIX', tenancy: 'default', scope: 'Availability Zone', availability_zone: 'us-east-1a' }, 'us-east-1b'],
    ['ec2', 'scope', 'm5.large', { instance_type: 'm5.large', platform: 'Linux/UNIX', tenancy: 'default', scope: 'Region' }, 'Availability Zone'],
    ['compute', 'platform', 'm5.large', { instance_type: 'm5.large', platform: 'Linux/UNIX', tenancy: 'default', scope: 'Region' }, 'Windows'],
    ['rds', 'az_config', 'db.r5.large', { engine: 'postgres', az_config: 'single-az' }, 'multi-az'],
    ['relational-db', 'az_config', 'db.r5.large', { engine: 'postgres', az_config: 'single-az' }, 'multi-az'],
    ['rds', 'engine', 'db.r5.large', { engine: 'postgres', az_config: 'single-az' }, 'mysql'],
    ['elasticache', 'engine', 'cache.r6g.large', { engine: 'redis', node_type: 'cache.r6g.large' }, 'memcached'],
    ['cache', 'engine', 'cache.r6g.large', { engine: 'redis', node_type: 'cache.r6g.large' }, 'memcached'],
    ['savingsplans', 'plan_type', '', { plan_type: 'Compute', hourly_commitment: 1 }, 'SageMaker'],
    ['savings-plans-ec2instance', 'instance_family', '', { plan_type: 'EC2Instance', instance_family: 'm5', region: 'us-east-1', hourly_commitment: 1 }, 'm6i'],
    ['savings-plans-ec2instance', 'region', '', { plan_type: 'EC2Instance', instance_family: 'm5', region: 'us-east-1', hourly_commitment: 1 }, 'us-west-2'],
  ])('purchase identity excludes %s variants with different %s', async (service, field, resourceType, details, otherValue) => {
    const savingsPlan = service === 'savingsplans' || service.startsWith('savings-plans');
    const original = { ...buildRows()[0]!, service, resource_type: resourceType, count: savingsPlan ? 1 : 2, region: savingsPlan ? '' : 'us-east-1', details };
    const other = { ...original, id: 'other-identity', term: 1, upfront_cost: 12000, details: { ...details, [field]: otherValue } };
    (state.getRecommendations as jest.Mock).mockReturnValue([other, original]);

    await openPurchaseModal([original]);

    const termSelect = document.querySelector<HTMLSelectElement>('.purchase-row-term')!;
    expect(Array.from(termSelect.options, (option) => option.value)).toEqual(['3']);
    expect(getPurchaseModalRecommendations()).toEqual([original]);
    (document.getElementById('execute-purchase-btn') as HTMLButtonElement).click();
    await flush();
    expect(api.executePurchase).toHaveBeenCalledTimes(1);
    expect((api.executePurchase as jest.Mock).mock.calls[0]![0]).toEqual([
      expect.objectContaining({ id: original.id, term: 3, details, upfront_cost: original.upfront_cost }),
    ]);
  });

  test.each([false, true])('purchase identity selects the matching priced EC2 variant (reverse order: %s)', async (reverse) => {
    const details = { instance_type: 'm5.large', platform: 'Linux/UNIX', tenancy: 'default', scope: 'Region' };
    const original = { ...buildRows()[0]!, details };
    const dedicated = { ...original, id: 'dedicated-1-all', term: 1, upfront_cost: 12000, details: { ...details, tenancy: 'dedicated' } };
    const matching = { ...original, id: 'default-1-no', term: 1, payment: 'no-upfront', upfront_cost: 0, monthly_cost: 800, savings: 500 };
    const loaded = [original, dedicated, matching];
    (state.getRecommendations as jest.Mock).mockReturnValue(reverse ? loaded.reverse() : loaded);

    await openPurchaseModal([original]);
    const termSelect = document.querySelector<HTMLSelectElement>('.purchase-row-term')!;
    termSelect.value = '1';
    termSelect.dispatchEvent(new Event('change'));

    const row = document.querySelector<HTMLTableRowElement>('.purchase-modal-table tbody tr')!;
    expect(row.cells[5]!.textContent).toBe(formatCurrency(0));
    expect(row.cells[6]!.textContent).toBe(formatCurrency(800));
    expect(document.querySelector<HTMLSelectElement>('.purchase-row-payment')!.value).toBe('no-upfront');
    expect(getPurchaseModalRecommendations()[0]).toMatchObject(matching);
    (document.getElementById('execute-purchase-btn') as HTMLButtonElement).click();
    await flush();
    expect(api.executePurchase).toHaveBeenCalledTimes(1);
    expect((api.executePurchase as jest.Mock).mock.calls[0]![0]).toEqual([
      expect.objectContaining(matching),
    ]);
  });

  test('purchase identity keeps a regional EC2 rec (no AZ) matching its own variants', async () => {
    const details = { instance_type: 'm5.large', platform: 'Linux/UNIX', tenancy: 'default', scope: 'Region' };
    const original = { ...buildRows()[0]!, details };
    const sibling = { ...original, id: 'regional-1-all', term: 1, upfront_cost: 12000 };
    (state.getRecommendations as jest.Mock).mockReturnValue([sibling, original]);

    await openPurchaseModal([original]);

    expect(Array.from(document.querySelector<HTMLSelectElement>('.purchase-row-term')!.options, (option) => option.value).sort())
      .toEqual(['1', '3']);
  });

  test.each([undefined, null, [], 'invalid', { platform: 1 }, {}])('purchase identity does not match populated EC2 details to %j', async (otherDetails) => {
    const original = { ...buildRows()[0]!, details: { platform: 'Linux/UNIX', tenancy: 'default', scope: 'Region' } };
    const other = { ...original, id: 'missing-identity', term: 1, details: otherDetails };
    (state.getRecommendations as jest.Mock).mockReturnValue([other, original]);

    await openPurchaseModal([original]);

    expect(Array.from(document.querySelector<HTMLSelectElement>('.purchase-row-term')!.options, (option) => option.value))
      .toEqual(['3']);
    expect(getPurchaseModalRecommendations()[0]).toEqual(original);
  });

  test('purchase identity allows Savings Plans prices and offering IDs to change', async () => {
    const original = {
      ...buildRows()[0]!, service: 'savings-plans-ec2instance', resource_type: '', region: '', count: 1,
      details: { plan_type: 'EC2Instance', instance_family: 'm5', region: 'us-east-1', hourly_commitment: 1, offering_id: 'offering-3-all', coverage: '50.0%' },
    };
    const matching = {
      ...original, id: 'sp-1-no', term: 1, payment: 'no-upfront', upfront_cost: 0, monthly_cost: 1460, savings: 400,
      details: { ...original.details, hourly_commitment: 2, offering_id: 'offering-1-no', coverage: '40.0%' },
    };
    (state.getRecommendations as jest.Mock).mockReturnValue([original, matching]);

    await openPurchaseModal([original]);
    const termSelect = document.querySelector<HTMLSelectElement>('.purchase-row-term')!;
    termSelect.value = '1';
    termSelect.dispatchEvent(new Event('change'));

    expect(getPurchaseModalRecommendations()[0]).toMatchObject(matching);
    expect(document.querySelector<HTMLTableRowElement>('.purchase-modal-table tbody tr')!.cells[6]!.textContent)
      .toBe(formatCurrency(1460));
    (document.getElementById('execute-purchase-btn') as HTMLButtonElement).click();
    await flush();
    expect(api.executePurchase).toHaveBeenCalledTimes(1);
    expect((api.executePurchase as jest.Mock).mock.calls[0]![0]).toEqual([
      expect.objectContaining(matching),
    ]);
  });
});
