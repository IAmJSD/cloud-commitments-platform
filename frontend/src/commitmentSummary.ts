/**
 * Shared "what exactly will be bought" block for money-moving confirm
 * dialogs (Execute now, Run now, RI exchange). Pure DOM builder plus the
 * totals helper, so every confirm states the same three numbers: what is
 * charged today, what recurs monthly, and the total over the term.
 */

import { formatCurrency, formatTerm } from './utils';

export interface CommitmentLine {
  service: string;
  provider?: string;
  region?: string;
  resourceType: string;
  count: number;
  /** Term in years. */
  term: number;
  payment: string;
  upfront: number;
  /** null when the provider did not report a recurring breakdown. */
  monthly: number | null;
  /** Resolved display name of the account, when known. */
  account?: string;
}

export interface CommitmentTotals {
  upfront: number;
  /** null when any line has no reported monthly cost. */
  monthly: number | null;
  /** upfront + monthly x 12 x term; null when monthly is null. */
  termTotal: number | null;
}

const MAX_LISTED_LINES = 5;

export function commitmentTotals(lines: readonly CommitmentLine[]): CommitmentTotals {
  let upfront = 0;
  let monthly: number | null = 0;
  let termTotal: number | null = 0;
  for (const l of lines) {
    upfront += l.upfront;
    if (l.monthly === null || monthly === null || termTotal === null) {
      monthly = null;
      termTotal = null;
      continue;
    }
    monthly += l.monthly;
    termTotal += l.upfront + l.monthly * 12 * l.term;
  }
  return { upfront, monthly, termTotal };
}

/** Label of the purchase modal's primary button; one source for every place that sets it. */
export function executeButtonLabel(direct: boolean): string {
  return direct ? 'Execute Purchase Now' : 'Send for Approval';
}

/**
 * Confirm-dialog button: the same verb as the modal button plus the upfront
 * amount, so the highest-stakes click states the money involved.
 */
export function executeConfirmLabel(direct: boolean, upfront: number): string {
  const amount = formatCurrency(upfront, '$', 2);
  return `${executeButtonLabel(direct)} (${amount} ${direct ? 'charged today' : 'upfront on approval'})`;
}

export function paymentLabel(payment: string): string {
  switch (payment.replace(/_/g, '-')) {
    case 'upfront': // Azure spelling of all-upfront
    case 'all-upfront': return 'All upfront';
    case 'partial-upfront': return 'Partial upfront';
    case 'no-upfront': return 'No upfront';
    case 'monthly': return 'Monthly';
    default: return payment;
  }
}

function stat(label: string, value: string): HTMLElement {
  const el = document.createElement('div');
  el.className = 'commitment-summary-stat';
  const l = document.createElement('span');
  l.className = 'commitment-summary-label';
  l.textContent = label;
  const v = document.createElement('strong');
  v.className = 'commitment-summary-value';
  v.textContent = value;
  el.append(l, v);
  return el;
}

// A partial or no-upfront commitment bills monthly; saying only "not
// reported" would read as "no monthly fee".
function monthlyUnknownText(lines: readonly CommitmentLine[]): string {
  const bills = lines.some(l => paymentLabel(l.payment) !== 'All upfront');
  return bills ? 'Not reported (monthly fees apply)' : 'Not reported';
}

function describe(l: CommitmentLine): string {
  const where = [l.account, l.region].filter(Boolean).join(', ');
  const what = `${l.count} × ${l.service} ${l.resourceType}`;
  const terms = `${formatTerm(l.term)}, ${paymentLabel(l.payment)}`;
  return [what, terms, where].filter(Boolean).join(' · ');
}

export interface SummaryOptions {
  /** Label for the upfront stat; 'Upfront on approval' when nothing is charged yet. */
  upfrontLabel?: string;
}

export function buildCommitmentSummary(lines: readonly CommitmentLine[], opts: SummaryOptions = {}): HTMLElement {
  const totals = commitmentTotals(lines);
  const root = document.createElement('div');
  root.className = 'commitment-summary';

  const band = document.createElement('div');
  band.className = 'commitment-summary-band';
  band.append(
    stat(opts.upfrontLabel ?? 'Charged today', formatCurrency(totals.upfront, '$', 2)),
    stat('Monthly', totals.monthly === null ? monthlyUnknownText(lines) : formatCurrency(totals.monthly, '$', 2)),
    stat('Total over term', totals.termTotal === null ? 'Not available' : formatCurrency(totals.termTotal, '$', 2)),
  );
  root.appendChild(band);

  const list = document.createElement('ul');
  list.className = 'commitment-summary-lines';
  for (const l of lines.slice(0, MAX_LISTED_LINES)) {
    const li = document.createElement('li');
    li.textContent = describe(l);
    list.appendChild(li);
  }
  if (lines.length > MAX_LISTED_LINES) {
    const li = document.createElement('li');
    li.textContent = `and ${lines.length - MAX_LISTED_LINES} more`;
    list.appendChild(li);
  }
  root.appendChild(list);
  return root;
}

/** Intro sentence followed by the summary block, as one dialog body. */
export function withIntro(intro: string, summary: HTMLElement): HTMLElement {
  const root = document.createElement('div');
  const p = document.createElement('p');
  p.textContent = intro;
  root.append(p, summary);
  return root;
}
