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

/** The priced-recommendation fields a commitment line is built from. */
export interface PricedCommitment {
  service: string;
  provider?: string;
  region?: string;
  resource_type: string;
  count: number;
  term: number;
  payment?: string;
  upfront_cost: number;
  monthly_cost?: number | null;
}

/**
 * Map a priced recommendation onto a commitment line. `payment` overrides
 * the rec's own (the bulk modal prices a whole bucket at one payment).
 */
export function commitmentLine(r: PricedCommitment, opts: { payment?: string; account?: string } = {}): CommitmentLine {
  return {
    service: r.service,
    ...(r.provider ? { provider: r.provider } : {}),
    ...(r.region ? { region: r.region } : {}),
    resourceType: r.resource_type,
    count: r.count,
    term: r.term,
    payment: opts.payment ?? r.payment ?? '',
    upfront: r.upfront_cost,
    monthly: r.monthly_cost ?? null,
    ...(opts.account ? { account: opts.account } : {}),
  };
}

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

/** True when any line is partial or no upfront, so it bills monthly. */
export function billsMonthly(lines: readonly CommitmentLine[]): boolean {
  return lines.some(l => paymentLabel(l.payment) !== 'All upfront');
}

// A partial or no-upfront commitment bills monthly; saying only "not
// reported" would read as "no monthly fee".
function monthlyUnknownText(lines: readonly CommitmentLine[]): string {
  return billsMonthly(lines) ? 'Not reported (monthly fees apply)' : 'Not reported';
}

export interface CommitmentTotalsText {
  upfront: string;
  monthly: string;
  termTotal: string;
}

/**
 * The three totals as display strings, so every screen words an unknown
 * monthly cost the same way instead of printing a partial sum or $0.
 */
export function formatCommitmentTotals(lines: readonly CommitmentLine[], digits = 2): CommitmentTotalsText {
  const totals = commitmentTotals(lines);
  return {
    upfront: formatCurrency(totals.upfront, '$', digits),
    monthly: totals.monthly === null ? monthlyUnknownText(lines) : formatCurrency(totals.monthly, '$', digits),
    termTotal: totals.termTotal === null ? 'Not available' : formatCurrency(totals.termTotal, '$', digits),
  };
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

/** Just the Upfront / Monthly / Total over term band, for screens that already list the lines. */
export function buildCommitmentBand(lines: readonly CommitmentLine[], opts: SummaryOptions = {}): HTMLElement {
  const text = formatCommitmentTotals(lines);
  const band = document.createElement('div');
  band.className = 'commitment-summary-band';
  band.append(
    stat(opts.upfrontLabel ?? 'Charged today', text.upfront),
    stat('Monthly', text.monthly),
    stat('Total over term', text.termTotal),
  );
  return band;
}

export function buildCommitmentSummary(lines: readonly CommitmentLine[], opts: SummaryOptions = {}): HTMLElement {
  const root = document.createElement('div');
  root.className = 'commitment-summary';
  root.appendChild(buildCommitmentBand(lines, opts));

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
