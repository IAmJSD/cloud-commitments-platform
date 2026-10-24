// Pure helpers for the Azure RI exchange flow (issue #81): request shaping,
// input validation and user-facing error text for
// POST /api/ri-exchange/azure-instances/{compatible-offerings,exchange}.

import type { ExchangeableAzureRI } from '../api/types';

/** Terms the backend accepts (armreservations.PossibleReservationTermValues). */
export const AZURE_EXCHANGE_TERMS: readonly string[] = ['P1Y', 'P3Y', 'P5Y'];

const TERM_LABELS: Readonly<Record<string, string>> = { P1Y: '1 year', P3Y: '3 years', P5Y: '5 years' };

/** "P3Y" -> "3 years"; unknown terms are shown as Azure sent them. */
export function azureTermLabel(term: string | undefined): string {
  if (!term) return '—';
  return Object.prototype.hasOwnProperty.call(TERM_LABELS, term) ? TERM_LABELS[term]! : term;
}

/** Backend cap on sources and on targets per request (maxAzureExchangeItems). */
export const MAX_AZURE_EXCHANGE_ITEMS = 50;

/** A non-negative plain decimal; the backend parses max_payment_due as an exact rational. */
export const AZURE_MAX_PAYMENT_PATTERN = /^\d+(?:\.\d+)?$/;

/** ISO 4217 currency code. */
export const AZURE_CURRENCY_PATTERN = /^[A-Za-z]{3}$/;

/** The subscription GUID in a billing scope such as "/subscriptions/{id}". */
export function subscriptionFromBillingScope(scope: string | undefined): string | null {
  const m = /^\/subscriptions\/([^/\s]+)\/?$/i.exec((scope ?? '').trim());
  return m ? m[1]! : null;
}

/**
 * The subscription an exchange of these reservations must be requested
 * under. The backend only lets a subscription hand back reservations it is
 * billed for, so every source must report the same billing subscription.
 */
export function resolveExchangeSubscription(sources: readonly ExchangeableAzureRI[]): { subscriptionId: string } | { error: string } {
  if (sources.length === 0) return { error: 'Select at least one reservation to exchange.' };
  if (sources.length > MAX_AZURE_EXCHANGE_ITEMS) {
    return { error: `Select at most ${MAX_AZURE_EXCHANGE_ITEMS} reservations per exchange.` };
  }
  let subscriptionId: string | null = null;
  for (const r of sources) {
    const sub = subscriptionFromBillingScope(r.billing_scope_id);
    if (!sub) {
      return { error: `Azure did not report which subscription pays for ${r.display_name || r.reservation_id}, so it cannot be exchanged here.` };
    }
    if (subscriptionId !== null && sub.toLowerCase() !== subscriptionId.toLowerCase()) {
      return { error: 'The selected reservations are billed to different subscriptions. Exchange them separately, one subscription at a time.' };
    }
    subscriptionId ??= sub;
  }
  return { subscriptionId: subscriptionId! };
}

export interface AzureExchangeSourceInput {
  reservation_id: string;
  quantity: number;
}

export interface AzureExchangeTargetInput {
  sku: string;
  location: string;
  term: string;
  quantity: number;
}

/** Validates the form values the way the backend does, so the user sees the problem before a round trip. */
export function validateAzureExchangeForm(sources: readonly AzureExchangeSourceInput[], sourceMax: readonly number[], targets: readonly AzureExchangeTargetInput[]): string | null {
  for (let i = 0; i < sources.length; i++) {
    const q = sources[i]!.quantity;
    if (!Number.isInteger(q) || q < 1 || q > (sourceMax[i] ?? q)) {
      return `Source ${i + 1}: quantity must be a whole number from 1 to ${sourceMax[i]}.`;
    }
  }
  if (targets.length === 0) return 'Add at least one target.';
  if (targets.length > MAX_AZURE_EXCHANGE_ITEMS) return `Add at most ${MAX_AZURE_EXCHANGE_ITEMS} targets.`;
  for (let i = 0; i < targets.length; i++) {
    const t = targets[i]!;
    if (!t.sku.trim()) return `Target ${i + 1}: enter a VM size (SKU).`;
    if (!t.location.trim()) return `Target ${i + 1}: enter a region.`;
    if (!AZURE_EXCHANGE_TERMS.includes(t.term)) return `Target ${i + 1}: pick a term.`;
    if (!Number.isInteger(t.quantity) || t.quantity < 1) return `Target ${i + 1}: quantity must be a whole number of at least 1.`;
  }
  return null;
}

/** Validates the mandatory execute guardrails. */
export function validateAzureGuardrails(maxPaymentDue: string, currency: string): string | null {
  if (!AZURE_MAX_PAYMENT_PATTERN.test(maxPaymentDue.trim())) {
    return 'Enter the maximum you are willing to pay as a plain number, for example 1250.00.';
  }
  if (!AZURE_CURRENCY_PATTERN.test(currency.trim())) return 'Enter the three-letter currency code, for example USD.';
  return null;
}

/** Formats a nullable amount; a missing amount is shown as such, never as 0. */
export function formatAzureAmount(amount: number | null | undefined, currency: string | undefined): string {
  if (amount === null || amount === undefined || !Number.isFinite(amount)) return 'not reported';
  const code = currency ? `${currency} ` : '';
  return `${code}${amount.toFixed(2)}`;
}

interface ApiErrorLike {
  message?: string;
  status?: number;
}

// Hints for the 422 guardrail refusals checkAzureExchangeMoneyGuardrails
// returns, keyed on its fixed message text.
const GUARDRAIL_HINTS: ReadonlyArray<[RegExp, string]> = [
  [/exceeds max_payment_due/, 'Raise the maximum payment or change the targets, then preview again.'],
  [/does not match requested currency/, 'Set the currency to the one Azure quoted.'],
  [/did not return a net payable/, 'Azure gave no price for this exchange. Try again later.'],
  [/^Azure rejected this exchange/, 'Change the sources or targets so they meet Azure\'s exchange policy.'],
];

/** User-facing text for a failed preview or execute call. */
export function describeAzureExchangeError(error: unknown, phase: 'preview' | 'execute'): string {
  const err = (error ?? {}) as ApiErrorLike;
  const msg = err.message || 'Unknown error';
  const what = phase === 'preview' ? 'Could not preview the exchange' : 'Exchange not executed';
  switch (err.status) {
    case 422: {
      const hint = GUARDRAIL_HINTS.find(([re]) => re.test(msg))?.[1];
      return `Exchange blocked by a safety check: ${msg}.${hint ? ` ${hint}` : ''}`;
    }
    case 403:
      return `${what}: you are not allowed to do this. ${msg}`;
    case 404:
      return `${what}: no Azure account you can access is registered for this subscription.`;
    case 400:
      return `${what}: the request was rejected. ${msg}`;
    default:
      return `${what}: ${msg}`;
  }
}
