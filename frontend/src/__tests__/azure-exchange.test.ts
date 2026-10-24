/**
 * Pure helpers for the Azure RI exchange flow (issue #81).
 */
import {
  azureTermLabel, describeAzureExchangeError, formatAzureAmount, resolveExchangeSubscription,
  subscriptionFromBillingScope, validateAzureExchangeForm, validateAzureGuardrails,
} from '../lib/azure-exchange';
import type { ExchangeableAzureRI } from '../api/types';

const SUB = '11111111-2222-3333-4444-555555555555';
const ri = (id: string, scope?: string): ExchangeableAzureRI => ({
  reservation_order_id: `o-${id}`, reservation_id: id, sku: 'Standard_D2s_v3', quantity: 2, instance_flexibility: 'On', billing_scope_id: scope,
});

describe('subscriptionFromBillingScope', () => {
  it.each([
    [`/subscriptions/${SUB}`, SUB],
    [`/SUBSCRIPTIONS/${SUB}/`, SUB],
    [`  /subscriptions/${SUB}  `, SUB],
    [`/subscriptions/${SUB}/resourceGroups/rg`, null],
    ['/providers/Microsoft.Billing/billingAccounts/1', null],
    ['', null],
    [undefined, null],
  ])('%p -> %p', (scope, expected) => {
    expect(subscriptionFromBillingScope(scope)).toBe(expected);
  });
});

describe('resolveExchangeSubscription', () => {
  it('returns the shared billing subscription', () => {
    expect(resolveExchangeSubscription([ri('a', `/subscriptions/${SUB}`), ri('b', `/subscriptions/${SUB.toUpperCase()}`)]))
      .toEqual({ subscriptionId: SUB });
  });
  it('refuses reservations billed to different subscriptions', () => {
    const r = resolveExchangeSubscription([ri('a', `/subscriptions/${SUB}`), ri('b', '/subscriptions/other')]);
    expect('error' in r && r.error).toMatch(/different subscriptions/);
  });
  it('refuses a reservation without a reported billing scope', () => {
    const r = resolveExchangeSubscription([ri('a')]);
    expect('error' in r && r.error).toMatch(/did not report which subscription pays for a/);
  });
  it('refuses an empty or oversized selection', () => {
    expect('error' in resolveExchangeSubscription([])).toBe(true);
    const many = Array.from({ length: 51 }, (_, i) => ri(String(i), `/subscriptions/${SUB}`));
    expect('error' in resolveExchangeSubscription(many)).toBe(true);
  });
});

describe('validateAzureExchangeForm', () => {
  const target = { sku: 'Standard_D4s_v5', location: 'eastus', term: 'P3Y', quantity: 1 };
  const source = { reservation_id: 'a', quantity: 2 };
  it('accepts a complete form', () => {
    expect(validateAzureExchangeForm([source], [2], [target])).toBeNull();
  });
  it.each([
    [[{ ...source, quantity: 3 }], [target], /Source 1: quantity must be a whole number from 1 to 2/],
    [[{ ...source, quantity: 0 }], [target], /Source 1/],
    [[{ ...source, quantity: 1.5 }], [target], /Source 1/],
    [[source], [], /at least one target/],
    [[source], [{ ...target, sku: ' ' }], /Target 1: enter a VM size/],
    [[source], [{ ...target, location: '' }], /Target 1: enter a region/],
    [[source], [{ ...target, term: 'P2Y' }], /Target 1: pick a term/],
    [[source], [target, { ...target, quantity: 0 }], /Target 2: quantity/],
  ])('rejects %#', (sources, targets, msg) => {
    expect(validateAzureExchangeForm(sources, [2], targets)).toMatch(msg);
  });
});

describe('validateAzureGuardrails', () => {
  it.each([['1250.00', 'USD'], ['0', 'eur'], [' 10 ', ' GBP ']])('accepts %p %p', (max, cur) => {
    expect(validateAzureGuardrails(max, cur)).toBeNull();
  });
  it.each([['', 'USD'], ['-5', 'USD'], ['1/3', 'USD'], ['1e3', 'USD'], ['10', ''], ['10', 'US'], ['10', 'US$']])('rejects %p %p', (max, cur) => {
    expect(validateAzureGuardrails(max, cur)).not.toBeNull();
  });
});

describe('formatting', () => {
  it('never shows a missing amount as zero', () => {
    expect(formatAzureAmount(null, 'USD')).toBe('not reported');
    expect(formatAzureAmount(undefined, 'USD')).toBe('not reported');
    expect(formatAzureAmount(0, 'USD')).toBe('USD 0.00');
    expect(formatAzureAmount(-12.5, 'EUR')).toBe('EUR -12.50');
  });
  it('labels terms', () => {
    expect(azureTermLabel('P3Y')).toBe('3 years');
    expect(azureTermLabel('P7Y')).toBe('P7Y');
    expect(azureTermLabel(undefined)).toBe('—');
    expect(azureTermLabel('toString')).toBe('toString');
  });
});

describe('describeAzureExchangeError', () => {
  const e = (status: number, message: string) => Object.assign(new Error(message), { status });
  it.each([
    ['quoted net payable 120.00 USD exceeds max_payment_due 100.00 USD', /safety check: quoted net payable .* Raise the maximum payment/],
    ['quoted currency "EUR" does not match requested currency "USD"', /Set the currency to the one Azure quoted/],
    ['Azure did not return a net payable amount; refusing to execute', /Azure gave no price/],
    ['Azure rejected this exchange: expired reservation', /meet Azure's exchange policy/],
  ])('explains the 422 guardrail %p', (msg, expected) => {
    expect(describeAzureExchangeError(e(422, msg), 'execute')).toMatch(expected);
  });
  it('explains permission, scope and request errors', () => {
    expect(describeAzureExchangeError(e(403, 'direct RI exchange execution is disabled while 4-eyes approval mode is on; use the approval flow'), 'execute'))
      .toBe('Exchange not executed: you are not allowed to do this. direct RI exchange execution is disabled while 4-eyes approval mode is on; use the approval flow');
    expect(describeAzureExchangeError(e(404, 'not found'), 'preview')).toMatch(/^Could not preview the exchange: no Azure account/);
    expect(describeAzureExchangeError(e(400, 'targets[0].sku is required'), 'preview')).toMatch(/rejected\. targets\[0\]\.sku is required/);
    expect(describeAzureExchangeError(e(500, 'exchange execution failed and may already have been submitted to Azure'), 'execute'))
      .toMatch(/may already have been submitted/);
    expect(describeAzureExchangeError(undefined, 'preview')).toBe('Could not preview the exchange: Unknown error');
  });
});
