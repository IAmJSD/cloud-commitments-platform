/**
 * Archera plan comparison section (issue #785), shown on the Archera
 * education page. Read-only; fetched only on an explicit click.
 *
 * Rules enforced here:
 *   - Every string from the server (vendor text included) is set with
 *     textContent; there is no innerHTML.
 *   - Money is rendered verbatim as the exact decimal string the server
 *     sent. null renders as "Unknown", never 0. There is no currency
 *     symbol and no numeric parsing.
 *   - Titles, notes and both disclosures come from the response verbatim.
 *   - No polling, no timers, no client cache: each click is one request.
 */

import { getInsuranceComparison, getInsuranceStatus } from './api/insurance';
import type {
  ArcheraComparison,
  ArcheraFinancials,
  ArcheraOffer,
  ArcheraTotals,
} from './api/types';

const UNKNOWN = 'Unknown';
const NOT_GIVEN = 'Not given';
const NOT_KNOWN_SUPPORTED = 'Not known to be supported by Archera';

function el<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  text?: string,
  className?: string,
): HTMLElementTagNameMap[K] {
  const node = document.createElement(tag);
  if (text !== undefined) node.textContent = text;
  if (className) node.className = className;
  return node;
}

function money(v: unknown): string {
  return typeof v === 'string' ? v : UNKNOWN;
}

function text(v: unknown): string {
  return typeof v === 'string' && v !== '' ? v : NOT_GIVEN;
}

function cell(tag: 'td' | 'th', content: string, scope?: 'row' | 'col'): HTMLTableCellElement {
  const c = el(tag, content);
  if (scope) c.scope = scope;
  return c;
}

function table(caption: string, headers: string[]): { wrap: HTMLElement; body: HTMLTableSectionElement } {
  const wrap = el('div');
  wrap.style.overflowX = 'auto';
  const t = el('table');
  t.appendChild(el('caption', caption));
  const head = el('thead');
  const tr = el('tr');
  for (const h of headers) tr.appendChild(cell('th', h, 'col'));
  head.appendChild(tr);
  t.appendChild(head);
  const body = el('tbody');
  t.appendChild(body);
  wrap.appendChild(t);
  return { wrap, body };
}

function formatFetchedAt(raw: unknown): string {
  const s = typeof raw === 'string' ? raw : '';
  const d = new Date(s);
  if (s === '' || Number.isNaN(d.getTime())) return `Fetched at ${s || NOT_GIVEN}`;
  const opts: Intl.DateTimeFormatOptions = { dateStyle: 'medium', timeStyle: 'long' };
  const local = new Intl.DateTimeFormat(undefined, opts).format(d);
  const utc = new Intl.DateTimeFormat('en-GB', { ...opts, timeZone: 'UTC' }).format(d);
  return `Fetched at ${local} (${utc})`;
}

function totalsRows(body: HTMLTableSectionElement, t: ArcheraTotals): void {
  const rows: Array<[string, string | null]> = [
    ['Commitment cost total (monthly)', t.monthly_730h_commitment_cost_total],
    ['Cloud provider cost (monthly)', t.monthly_730h_cloud_provider_cost],
    ['Archera premium (monthly)', t.monthly_730h_archera_premium],
    ['Gross savings (monthly)', t.monthly_730h_gross_savings],
    ['Net savings (monthly)', t.monthly_730h_net_savings],
    ['Covered on-demand cost (monthly)', t.monthly_730h_covered_on_demand_cost],
    ['Upfront cost (one-time)', t.upfront_one_time_cost],
  ];
  for (const [label, value] of rows) {
    const tr = el('tr');
    tr.appendChild(cell('th', label, 'row'));
    tr.appendChild(cell('td', money(value)));
    body.appendChild(tr);
  }
}

function productSupport(o: ArcheraOffer): HTMLElement {
  const box = el('div');
  const ps = o.archera_product_support;
  if (ps && ps.status === 'supported') {
    box.appendChild(el('div', 'Supported (documented)'));
    if (ps.source) box.appendChild(el('div', `Source: ${ps.source}`));
    if (ps.evidence) box.appendChild(el('div', `Evidence: ${ps.evidence}`));
  } else {
    box.appendChild(el('div', NOT_KNOWN_SUPPORTED));
  }
  box.appendChild(el('div', 'Underwriting allowance: Unknown'));
  box.appendChild(el('div', 'Customer eligibility: Unknown'));
  return box;
}

function offerName(o: ArcheraOffer): HTMLElement {
  const box = el('div');
  if (typeof o.archera_offer_name === 'string' && o.archera_offer_name !== '') {
    box.appendChild(el('div', `Archera's name for this offer: ${o.archera_offer_name}`));
    if (o.archera_offer_name_note) box.appendChild(el('div', o.archera_offer_name_note));
  } else {
    box.textContent = NOT_GIVEN;
  }
  return box;
}

function offerRow(body: HTMLTableSectionElement, o: ArcheraOffer): void {
  const tr = el('tr');
  const id = cell('th', `${text(o.offer_id)}${o.is_current ? ' (current)' : ' (candidate)'}`, 'row');
  tr.appendChild(id);
  tr.appendChild(cell('td', `${text(o.provider)} / ${text(o.commitment_type)}`));
  tr.appendChild(cell('td', text(o.region)));
  tr.appendChild(cell('td', `${text(o.contract_term)} / ${text(o.payment_option)}`));
  tr.appendChild(cell('td', o.lease_attached ? `lease attached (${text(o.lease_menu_item_id)})` : 'no lease'));
  const name = el('td');
  name.appendChild(offerName(o));
  tr.appendChild(name);
  const support = el('td');
  support.appendChild(productSupport(o));
  tr.appendChild(support);
  tr.appendChild(cell('td', money(o.discount_rate)));
  tr.appendChild(cell('td', money(o.breakeven_days)));
  const m: ArcheraFinancials = o.monthly;
  tr.appendChild(cell('td', money(m.monthly_730h_net_savings)));
  tr.appendChild(cell('td', money(o.upfront_one_time_cost)));
  tr.appendChild(cell('td', money(o.delta_vs_current.monthly_730h_net_savings)));
  tr.appendChild(cell('td', money(o.delta_vs_current.upfront_one_time_cost)));
  body.appendChild(tr);
}

const OFFER_HEADERS = [
  'Offer', 'Provider / commitment type', 'Region', 'Term / payment option', 'Lease',
  'Archera offer name', 'Archera product support', 'Discount rate', 'Breakeven days',
  'Net savings (monthly)', 'Upfront cost (one-time)',
  'Net savings change vs current plan (monthly)', 'Upfront change vs current plan (one-time)',
];

/** Builds the full result view from the response. Exported for tests. */
export function buildComparisonView(c: ArcheraComparison): HTMLElement {
  const root = el('div');
  root.appendChild(el('h2', c.title));
  root.appendChild(el('p', formatFetchedAt(c.fetched_at), 'archera-fetched-at'));
  root.appendChild(el('p', 'Archera does not document a cache lifetime. Use Refresh to fetch again.'));
  for (const note of [c.currency_note, c.basis_note, c.delta_basis_note]) {
    root.appendChild(el('p', note, 'archera-note'));
  }
  root.appendChild(el('p', c.non_gating_disclosure, 'archera-disclosure'));
  root.appendChild(el('p', c.sponsorship_disclosure, 'archera-disclosure'));

  const cur = table('Current plan totals', ['Measure', 'Amount']);
  totalsRows(cur.body, c.current);
  root.appendChild(cur.wrap);

  if (c.hypotheticals.length > 0) {
    const h = table('Hypothetical alternatives, compared with the current plan', [
      'Contract term', 'Payment option', 'Commitment cost total (monthly)', 'Net savings (monthly)',
      'Upfront cost (one-time)', 'Net savings change vs current plan (monthly)',
      'Commitment cost change vs current plan (monthly)', 'Upfront change vs current plan (one-time)',
      'Line items',
    ]);
    for (const hy of c.hypotheticals) {
      const tr = el('tr');
      tr.appendChild(cell('th', text(hy.contract_term), 'row'));
      tr.appendChild(cell('td', text(hy.payment_option)));
      tr.appendChild(cell('td', money(hy.totals.monthly_730h_commitment_cost_total)));
      tr.appendChild(cell('td', money(hy.totals.monthly_730h_net_savings)));
      tr.appendChild(cell('td', money(hy.totals.upfront_one_time_cost)));
      tr.appendChild(cell('td', money(hy.delta_vs_current.monthly_730h_net_savings)));
      tr.appendChild(cell('td', money(hy.delta_vs_current.monthly_730h_commitment_cost)));
      tr.appendChild(cell('td', money(hy.delta_vs_current.upfront_one_time_cost)));
      const li = el('td');
      for (const item of hy.line_items) {
        li.appendChild(el('div',
          `${text(item.line_item_id)}: ${text(item.reason)} (term ${text(item.actual_term)}, ` +
          `payment ${text(item.actual_payment_option)}, type ${text(item.actual_commitment_type)})`));
      }
      tr.appendChild(li);
      h.body.appendChild(tr);
    }
    root.appendChild(h.wrap);
  }

  for (const r of c.rows) {
    root.appendChild(el('h3', `Line item ${text(r.line_item_id)}`));
    const t = table(`Offers for line item ${text(r.line_item_id)}`, OFFER_HEADERS);
    offerRow(t.body, r.current);
    for (const cand of r.candidates) offerRow(t.body, cand);
    root.appendChild(t.wrap);
  }
  return root;
}

function errorText(err: unknown): string {
  const e = err as { message?: unknown; details?: Record<string, unknown> };
  const base = typeof e?.message === 'string' && e.message !== '' ? e.message : 'Archera comparison failed';
  const secs = e?.details?.['retry_after_seconds'];
  if ((e as { status?: number })?.status === 429) {
    if (typeof secs === 'number' && secs > 0) return `${base}. Retry after ${secs} seconds.`;
    // The server text for a 429 without Retry-After already says so.
    if (!/retry-after not given/i.test(base)) return `${base}. Retry-after not given.`;
  }
  return base;
}

/**
 * Renders the comparison action into `root` when the backend reports the
 * feature configured. Hidden for configured=false and for 404; any other
 * status failure shows one line. No comparison request is made in any of
 * those cases.
 */
export async function renderComparisonSection(root: HTMLElement): Promise<void> {
  let configured = false;
  try {
    configured = (await getInsuranceStatus()).configured === true;
  } catch (err) {
    if ((err as { status?: number })?.status !== 404) {
      root.appendChild(el('p', 'Archera comparison status unavailable'));
    }
    return;
  }
  if (!configured) return;

  const section = el('section');
  section.setAttribute('aria-label', 'Archera plan comparison');
  const button = el('button', 'Compare with Archera', 'btn');
  button.type = 'button';
  const live = el('div');
  live.setAttribute('aria-live', 'polite');
  const result = el('div');
  section.append(button, live, result);
  root.appendChild(section);

  let inFlight = false;
  let loaded = false;
  button.addEventListener('click', () => {
    if (inFlight) return;
    inFlight = true;
    button.disabled = true;
    button.setAttribute('aria-busy', 'true');
    live.textContent = 'Loading Archera comparison';
    void getInsuranceComparison()
      .then(c => {
        result.replaceChildren(buildComparisonView(c));
        live.textContent = 'Archera comparison loaded';
        loaded = true;
        button.textContent = 'Refresh';
      })
      .catch((err: unknown) => {
        live.textContent = errorText(err);
        if (loaded) button.textContent = 'Refresh';
      })
      .finally(() => {
        inFlight = false;
        button.disabled = false;
        button.removeAttribute('aria-busy');
        button.focus();
      });
  });
}
