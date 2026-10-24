/**
 * Readable bodies for the RI exchange money confirms and quote summary.
 *
 * An exchange is irreversible, so every place that asks the user to commit
 * to one (Execute after a quote, Approve from history) names what is given
 * up and what is received by instance type first, keeps the exact offering
 * id next to it for verification, and shows the quoted price change.
 */

import type { ExchangeQuoteSummary, RIExchangeHistoryRecord } from './api';

// Hours in an average month; only used for the approximate monthly delta.
const HOURS_PER_MONTH = 730;

export interface ExchangeConfirmParts {
  quote: ExchangeQuoteSummary;
  sources: string[];
  targets: string[];
}

// hourlyPriceDelta describes the quoted hourly price change as
// "+0.0250 USD/hr, about +18.25 USD/month", or returns null when the quote
// lacks either hourly price, so callers never show a made-up delta.
export function hourlyPriceDelta(quote: ExchangeQuoteSummary): string | null {
  const src = Number(quote.SourceHourlyPriceRaw);
  const tgt = Number(quote.TargetHourlyPriceRaw);
  if (!quote.SourceHourlyPriceRaw || !quote.TargetHourlyPriceRaw || !Number.isFinite(src) || !Number.isFinite(tgt)) {
    return null;
  }
  const delta = tgt - src;
  const sign = delta >= 0 ? '+' : '-';
  const abs = Math.abs(delta);
  const cur = quote.CurrencyCode;
  return `${sign}${abs.toFixed(4)} ${cur}/hr, about ${sign}${(abs * HOURS_PER_MONTH).toFixed(2)} ${cur}/month`;
}

function confirmBody(lines: Array<[string, string]>, warning: string): HTMLElement {
  const root = document.createElement('div');
  for (const [label, value] of lines) {
    const p = document.createElement('p');
    const strong = document.createElement('strong');
    strong.textContent = value;
    p.append(`${label}: `, strong);
    root.appendChild(p);
  }
  const warn = document.createElement('p');
  warn.textContent = warning;
  root.appendChild(warn);
  return root;
}

export function buildExchangeConfirmBody({ quote, sources, targets }: ExchangeConfirmParts): HTMLElement {
  const lines: Array<[string, string]> = [
    ['Pay now', `${quote.CurrencyCode} ${quote.PaymentDueRaw}`],
    ['Exchange', sources.join(', ')],
    ['For', targets.join(', ')],
    ['Region', quote.Region],
  ];
  const delta = hourlyPriceDelta(quote);
  if (delta) {
    const src = Number(quote.SourceHourlyPriceRaw).toFixed(4);
    const tgt = Number(quote.TargetHourlyPriceRaw).toFixed(4);
    lines.push(['Hourly price change', `${quote.CurrencyCode} ${src} → ${tgt} (${delta})`]);
  }
  return confirmBody(lines, 'This exchange executes immediately and cannot be reversed.');
}

// buildApproveConfirmBody describes a pending history record. The history
// API stores payment_due in USD (the execute cap is max_payment_due_usd),
// and carries the instance types, so the confirm leads with those rather
// than the target offering UUID.
export function buildApproveConfirmBody(record: RIExchangeHistoryRecord): HTMLElement {
  const ids = record.source_ri_ids.join(', ');
  const source = record.source_instance_type
    ? `${record.source_count} × ${record.source_instance_type} (${ids})`
    : ids;
  const target = record.target_instance_type
    ? `${record.target_count} × ${record.target_instance_type} (offering ${record.target_offering_id})`
    : `${record.target_count} × offering ${record.target_offering_id}`;
  return confirmBody(
    [
      ['Previously quoted payment', `USD ${record.payment_due}`],
      ['Exchange', source],
      ['For', target],
      ['Region', record.region],
    ],
    'Approving executes immediately using a fresh quote, within the configured per-exchange '
      + 'and remaining daily spending limits. This exchange cannot be reversed.',
  );
}
