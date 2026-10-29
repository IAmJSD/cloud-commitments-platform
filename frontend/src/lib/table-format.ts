/**
 * Display formatters shared by the data tables (Opportunities, approval
 * queue, purchase history, planned purchases, inventory, RI exchange).
 *
 * The tables used to show the same value four ways: payment as the raw
 * slug ("all-upfront"), "all upfront" or "All Upfront", and term as "1y",
 * "1yr" or "1 Year". These helpers give every table cell (and the column
 * filter labels that must match it) one compact spelling. Dialogs and plan
 * cards keep the long formatTerm() ("1 Year") from utils.ts.
 */

import { ownValue } from './own-property';

const PAYMENT_LABELS: Record<string, string> = {
  'all-upfront': 'All Upfront',
  'partial-upfront': 'Partial Upfront',
  'no-upfront': 'No Upfront',
  upfront: 'Upfront',
  monthly: 'Monthly',
};

/**
 * Human label for a payment option. Accepts the API slugs ("no-upfront"),
 * underscores ("no_upfront") and AWS's own spelling ("No Upfront"); any
 * other value is returned unchanged so nothing is hidden.
 */
export function formatPaymentOption(payment: string | null | undefined): string {
  if (!payment) return '';
  const key = payment.trim().toLowerCase().replace(/[\s_]+/g, '-');
  return ownValue(PAYMENT_LABELS, key) ?? payment;
}

/** Compact term for table cells: "1 yr", "3 yr". Empty for a missing term. */
export function formatTermShort(years: number | null | undefined): string {
  if (years === null || years === undefined || !Number.isFinite(years)) return '';
  return `${Math.round(years)} yr`;
}

/**
 * Currency for an amount the API sends as a decimal string (AWS returns RI
 * exchange payments as "184.220000"). Two decimals with separators:
 * "$184.22", "$1,210.50". A value that is not a number is shown as sent.
 */
export function formatDecimalStringCurrency(amount: string): string {
  const n = Number(amount);
  if (amount.trim() === '' || !Number.isFinite(n)) return amount;
  return n.toLocaleString('en-US', {
    style: 'currency',
    currency: 'USD',
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  });
}

/** Azure reservation terms come as ISO 8601 durations ("P1Y", "P3Y"). */
export function formatIsoTermShort(term: string | null | undefined): string {
  if (!term) return '';
  const m = /^P(\d+)Y$/i.exec(term.trim());
  return m ? formatTermShort(Number(m[1])) : term;
}
