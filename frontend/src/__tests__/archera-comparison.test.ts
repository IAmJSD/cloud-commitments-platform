/**
 * Archera comparison section (issue #785). The fixture is the Go DTO golden
 * file read from its Go path, so the UI is tested against the real shape.
 * Success rendering is therefore fixture-based; no live Archera calls.
 */
import * as fs from 'fs';
import * as path from 'path';
import { buildComparisonView, renderComparisonSection } from '../archera-comparison';
import { getInsuranceComparison, getInsuranceStatus } from '../api/insurance';
import type { ArcheraComparison, ArcheraOffer, ArcheraProductSupport } from '../api/types';

jest.mock('../api/insurance', () => ({
  getInsuranceStatus: jest.fn(),
  getInsuranceComparison: jest.fn(),
}));

const status = getInsuranceStatus as jest.Mock;
const comparison = getInsuranceComparison as jest.Mock;

const GOLDEN_PATH = path.resolve(__dirname, '../../../internal/archera/testdata/comparison.golden.json');
const golden = JSON.parse(fs.readFileSync(GOLDEN_PATH, 'utf8')) as ArcheraComparison;

// satisfies makes tsc fail when the TS DTO gains or loses a key; the
// runtime check below fails when the Go golden file drifts from the TS keys.
const TOP_KEYS = {
  title: 1, plan_id: 1, fetched_at: 1, currency: 1, currency_note: 1, premium_included: 1,
  basis_note: 1, delta_basis_note: 1, current: 1, hypotheticals: 1, rows: 1,
  non_gating_disclosure: 1, sponsorship_disclosure: 1,
} satisfies Record<keyof ArcheraComparison, 1>;
const SUPPORT_KEYS = {
  status: 1, source: 1, evidence: 1, underwriting_allowance: 1, customer_eligibility: 1,
} satisfies Record<keyof ArcheraProductSupport, 1>;

function clone(): ArcheraComparison {
  return JSON.parse(JSON.stringify(golden)) as ArcheraComparison;
}

function firstOffer(c: ArcheraComparison): ArcheraOffer {
  const row = c.rows[0];
  if (!row) throw new Error('golden has no rows');
  return row.current;
}

async function mount(): Promise<HTMLElement> {
  document.body.replaceChildren();
  const root = document.createElement('div');
  document.body.appendChild(root);
  await renderComparisonSection(root);
  return root;
}

async function clickAndLoad(root: HTMLElement, data: ArcheraComparison): Promise<void> {
  comparison.mockResolvedValueOnce(data);
  root.querySelector('button')!.click();
  await flush();
}

async function flush(): Promise<void> {
  for (let i = 0; i < 5; i++) await Promise.resolve();
}

beforeEach(() => {
  jest.resetAllMocks();
  status.mockResolvedValue({ configured: true, missing: [] });
});

describe('golden drift', () => {
  it('golden keys equal the TS DTO keys', () => {
    expect(Object.keys(golden).sort()).toEqual(Object.keys(TOP_KEYS).sort());
    const ps = firstOffer(golden).archera_product_support;
    expect(Object.keys(ps).sort()).toEqual(Object.keys(SUPPORT_KEYS).sort());
  });
});

describe('status gating', () => {
  it('configured=false: nothing rendered and no comparison request', async () => {
    status.mockResolvedValue({ configured: false, missing: ['ARCHERA_ORG_ID'] });
    const root = await mount();
    expect(root.innerHTML).toBe('');
    expect(comparison).not.toHaveBeenCalled();
  });

  it('404 (scoped session): nothing rendered and no comparison request', async () => {
    status.mockRejectedValue(Object.assign(new Error('not found'), { status: 404 }));
    const root = await mount();
    expect(root.innerHTML).toBe('');
    expect(comparison).not.toHaveBeenCalled();
  });

  it.each([500, 0])('status error %s: one unavailable line, no comparison request', async code => {
    status.mockRejectedValue(Object.assign(new Error('boom'), { status: code }));
    const root = await mount();
    expect(root.textContent).toBe('Archera comparison status unavailable');
    expect(root.querySelector('button')).toBeNull();
    expect(comparison).not.toHaveBeenCalled();
  });

  it('configured: button only, no comparison request until clicked', async () => {
    const root = await mount();
    expect(root.querySelector('button')!.textContent).toBe('Compare with Archera');
    expect(comparison).not.toHaveBeenCalled();
  });
});

describe('rendering the golden response', () => {
  it('renders the DTO title, notes, both disclosures and fetched-at verbatim', async () => {
    const root = await mount();
    await clickAndLoad(root, golden);
    expect(root.querySelector('h2')!.textContent).toBe(golden.title);
    const txt = root.textContent!;
    for (const s of [golden.currency_note, golden.basis_note, golden.delta_basis_note,
      golden.non_gating_disclosure, golden.sponsorship_disclosure,
      firstOffer(golden).archera_offer_name_note!]) {
      expect(txt).toContain(s);
    }
    expect(root.querySelector('.archera-fetched-at')!.textContent).toContain('Fetched at');
    expect(root.querySelector('.archera-fetched-at')!.textContent).toContain('9 Oct 2026');
    expect(txt).not.toContain('org_id');
  });

  it('renders disclosures from the response, not a hardcoded copy', async () => {
    const c = clone();
    c.non_gating_disclosure = 'DISCLOSURE-A-CHANGED';
    c.sponsorship_disclosure = 'DISCLOSURE-B-CHANGED';
    const root = await mount();
    await clickAndLoad(root, c);
    expect(root.textContent).toContain('DISCLOSURE-A-CHANGED');
    expect(root.textContent).toContain('DISCLOSURE-B-CHANGED');
    expect(root.textContent).not.toContain(golden.non_gating_disclosure);
  });

  it('shows no currency symbol, USD or dollar wording of its own', async () => {
    const c = clone();
    c.basis_note = 'one-time amounts';
    const root = await mount();
    await clickAndLoad(root, c);
    expect(root.textContent).not.toMatch(/\$|USD|dollar/i);
  });

  it('uses captions and column/row header scopes on every table', async () => {
    const root = await mount();
    await clickAndLoad(root, golden);
    const tables = root.querySelectorAll('table');
    expect(tables.length).toBeGreaterThanOrEqual(3);
    tables.forEach(t => {
      expect(t.querySelector('caption')!.textContent).not.toBe('');
      t.querySelectorAll('th').forEach(th => expect(th.getAttribute('scope')).toMatch(/^(col|row)$/));
    });
  });
});

describe('money', () => {
  it('null renders Unknown, "0" stays 0, long decimals are verbatim', async () => {
    const c = clone();
    c.current.monthly_730h_net_savings = null;
    c.current.monthly_730h_gross_savings = '0';
    c.current.upfront_one_time_cost = '0.0000000000000000000000000001234567890123';
    const root = await mount();
    await clickAndLoad(root, c);
    const rows = Array.from(root.querySelectorAll('table')[0]!.querySelectorAll('tr'));
    const byLabel = (l: string) => rows.find(r => r.textContent!.startsWith(l))!.querySelector('td')!.textContent;
    expect(byLabel('Net savings')).toBe('Unknown');
    expect(byLabel('Gross savings')).toBe('0');
    expect(byLabel('Upfront cost')).toBe('0.0000000000000000000000000001234567890123');
  });
});

describe('money in every cell matches the golden value for that column', () => {
  function grid(t: HTMLTableElement): Array<Record<string, string>> {
    const heads = Array.from(t.querySelectorAll('thead th')).map(h => h.textContent!);
    return Array.from(t.querySelectorAll('tbody tr')).map(tr => {
      const out: Record<string, string> = {};
      Array.from(tr.children).forEach((c, i) => { out[heads[i]!] = c.textContent!; });
      return out;
    });
  }
  const tableByCaption = (root: HTMLElement, cap: RegExp) =>
    Array.from(root.querySelectorAll('table')).find(t => cap.test(t.querySelector('caption')!.textContent!))!;

  it('current plan totals table', async () => {
    const root = await mount();
    await clickAndLoad(root, golden);
    const rows = Array.from(tableByCaption(root, /^Current plan totals/).querySelectorAll('tbody tr'))
      .map(tr => [tr.children[0]!.textContent, tr.children[1]!.textContent]);
    const c = golden.current;
    expect(rows).toEqual([
      ['Commitment cost total (monthly)', c.monthly_730h_commitment_cost_total],
      ['Cloud provider cost (monthly)', c.monthly_730h_cloud_provider_cost],
      ['Archera premium (monthly)', c.monthly_730h_archera_premium],
      ['Gross savings (monthly)', c.monthly_730h_gross_savings],
      ['Net savings (monthly)', c.monthly_730h_net_savings],
      ['Covered on-demand cost (monthly)', c.monthly_730h_covered_on_demand_cost],
      ['Upfront cost (one-time)', c.upfront_one_time_cost],
    ]);
    expect(new Set(rows.map(r => r[1])).size).toBe(7);
  });

  it('hypotheticals table', async () => {
    const root = await mount();
    await clickAndLoad(root, golden);
    const g = grid(tableByCaption(root, /^Hypothetical/));
    expect(g).toHaveLength(golden.hypotheticals.length);
    golden.hypotheticals.forEach((h, i) => {
      const r = g[i]!;
      expect(r['Contract term']).toBe(h.contract_term);
      expect(r['Payment option']).toBe(h.payment_option);
      expect(r['Commitment cost total (monthly)']).toBe(h.totals.monthly_730h_commitment_cost_total);
      expect(r['Net savings (monthly)']).toBe(h.totals.monthly_730h_net_savings);
      expect(r['Upfront cost (one-time)']).toBe(h.totals.upfront_one_time_cost);
      expect(r['Net savings change vs current plan (monthly)']).toBe(h.delta_vs_current.monthly_730h_net_savings);
      expect(r['Commitment cost change vs current plan (monthly)']).toBe(h.delta_vs_current.monthly_730h_commitment_cost);
      expect(r['Upfront change vs current plan (one-time)']).toBe(h.delta_vs_current.upfront_one_time_cost);
      h.line_items.forEach(li => {
        expect(r['Line items']).toContain(`${li.line_item_id}: ${li.reason} (term ${li.actual_term}, payment ${li.actual_payment_option}, type ${li.actual_commitment_type})`);
      });
    });
  });

  it('offer tables: current and every candidate', async () => {
    const root = await mount();
    await clickAndLoad(root, golden);
    golden.rows.forEach(row => {
      const g = grid(tableByCaption(root, new RegExp(`^Offers for line item ${row.line_item_id}$`)));
      const offers = [row.current, ...row.candidates];
      expect(g).toHaveLength(offers.length);
      offers.forEach((o, i) => {
        const r = g[i]!;
        expect(r['Offer']).toBe(`${o.offer_id} (${o.is_current ? 'current' : 'candidate'})`);
        expect(r['Provider / commitment type']).toBe(`${o.provider} / ${o.commitment_type}`);
        expect(r['Region']).toBe(o.region);
        expect(r['Term / payment option']).toBe(`${o.contract_term} / ${o.payment_option}`);
        expect(r['Discount rate']).toBe(o.discount_rate);
        expect(r['Breakeven days']).toBe(o.breakeven_days);
        expect(r['Net savings (monthly)']).toBe(o.monthly.monthly_730h_net_savings);
        expect(r['Upfront cost (one-time)']).toBe(o.upfront_one_time_cost);
        expect(r['Net savings change vs current plan (monthly)']).toBe(o.delta_vs_current.monthly_730h_net_savings);
        expect(r['Upfront change vs current plan (one-time)']).toBe(o.delta_vs_current.upfront_one_time_cost);
      });
    });
  });

  it('golden money values are distinct per column so a swap cannot hide', () => {
    const vals = [golden.current, golden.hypotheticals[0]!.totals].flatMap(t => Object.values(t));
    expect(new Set(vals).size).toBe(vals.length);
  });
});

describe('fetched-at shows local time and UTC', () => {
  const OPTS: Intl.DateTimeFormatOptions = { dateStyle: 'medium', timeStyle: 'long' };
  const instant = new Date(golden.fetched_at);
  const utcText = new Intl.DateTimeFormat('en-GB', { ...OPTS, timeZone: 'UTC' }).format(instant);
  const RealDTF = Intl.DateTimeFormat;

  afterEach(() => {
    Intl.DateTimeFormat = RealDTF;
  });

  // jest cannot change the process zone, so the browser's default zone is
  // simulated: formatters built without an explicit timeZone get `tz`.
  async function fetchedAt(tz: string): Promise<string> {
    Intl.DateTimeFormat = function (locale?: string | string[], o?: Intl.DateTimeFormatOptions) {
      return new RealDTF(locale, o?.timeZone ? o : { ...o, timeZone: tz });
    } as unknown as typeof Intl.DateTimeFormat;
    const root = await mount();
    await clickAndLoad(root, golden);
    return root.querySelector('.archera-fetched-at')!.textContent!;
  }

  it('non-UTC zone: local text and UTC text are both present and exact', async () => {
    const got = await fetchedAt('America/New_York');
    const local = new Intl.DateTimeFormat(undefined, { ...OPTS, timeZone: 'America/New_York' }).format(instant);
    expect(local).not.toBe(utcText);
    expect(utcText).toContain('12:00:00');
    expect(got).toBe(`Fetched at ${local} (${utcText})`);
  });

  it('UTC zone: exact text', async () => {
    const got = await fetchedAt('UTC');
    const local = new Intl.DateTimeFormat(undefined, { ...OPTS, timeZone: 'UTC' }).format(instant);
    expect(got).toBe(`Fetched at ${local} (${utcText})`);
  });

  it('UTC part stays in UTC when the browser zone is far away', async () => {
    const got = await fetchedAt('Pacific/Auckland');
    expect(got.endsWith(`(${utcText})`)).toBe(true);
  });
});

describe('captions, notes, live text and unknown money in every cell', () => {
  const captions = (root: HTMLElement) =>
    Array.from(root.querySelectorAll('caption')).map(c => c.textContent);

  it('offer table captions carry the line item id; empty is "Not given"; hostile stays text', async () => {
    const root = await mount();
    await clickAndLoad(root, golden);
    expect(captions(root)).toContain(`Offers for line item ${golden.rows[0]!.line_item_id}`);

    const empty = clone();
    empty.rows[0]!.line_item_id = '';
    const r2 = await mount();
    await clickAndLoad(r2, empty);
    expect(captions(r2)).toContain('Offers for line item Not given');

    const hostile = clone();
    hostile.rows[0]!.line_item_id = '<img src=x onerror=1>';
    const r3 = await mount();
    await clickAndLoad(r3, hostile);
    expect(captions(r3)).toContain('Offers for line item <img src=x onerror=1>');
    expect(r3.querySelector('img')).toBeNull();
  });

  it('shows the no-cache note', async () => {
    const root = await mount();
    await clickAndLoad(root, golden);
    expect(root.textContent).toContain('Archera does not document a cache lifetime. Use Refresh to fetch again.');
  });

  it('live region text: loading, loaded; button stays Refresh after a later error', async () => {
    const root = await mount();
    const button = root.querySelector('button')!;
    const live = root.querySelector('.archera-comparison [aria-live="polite"]') ?? root.querySelector('[aria-live="polite"]')!;
    let resolve!: (c: ArcheraComparison) => void;
    comparison.mockReturnValueOnce(new Promise<ArcheraComparison>(r => { resolve = r; }));
    button.click();
    expect(live.textContent).toBe('Loading Archera comparison');
    resolve(golden);
    await flush();
    expect(live.textContent).toBe('Archera comparison loaded');
    expect(button.textContent).toBe('Refresh');
    comparison.mockRejectedValueOnce(Object.assign(new Error('x'), { status: 502 }));
    button.click();
    await flush();
    expect(button.textContent).toBe('Refresh');
  });

  function nullMoney(node: unknown, key = ''): unknown {
    if (Array.isArray(node)) return node.map(x => nullMoney(x, key));
    if (node && typeof node === 'object') {
      return Object.fromEntries(Object.entries(node).map(([k, v]) => [k, nullMoney(v, k)]));
    }
    return /^(monthly_730h_|upfront_one_time_cost$|discount_rate$|breakeven_days$)/.test(key) ? null : node;
  }

  it('a null money value renders Unknown in every money cell of every table', async () => {
    const root = await mount();
    await clickAndLoad(root, nullMoney(clone()) as ArcheraComparison);
    const moneyHeads = /savings|cost|upfront|Upfront|Discount|Breakeven|premium|Amount/i;
    let checked = 0;
    root.querySelectorAll('table').forEach(t => {
      const heads = Array.from(t.querySelectorAll('thead th')).map(h => h.textContent!);
      t.querySelectorAll('tbody tr').forEach(tr => {
        Array.from(tr.children).forEach((c, i) => {
          const h = heads[i]!;
          const rowLabel = tr.children[0]!.textContent!;
          const isMoneyCell = h === 'Amount' ? true : (moneyHeads.test(h) && h !== 'Line items');
          if (isMoneyCell && !(h === 'Amount' && c === tr.children[0])) {
            expect({ h, rowLabel, v: c.textContent }).toEqual({ h, rowLabel, v: 'Unknown' });
            checked++;
          }
        });
      });
    });
    // 7 totals + 7 hypothetical + 6 per offer * 2 offers
    expect(checked).toBe(7 + 6 + 6 * 2);
  });
});

describe('multiple rows, candidates, hypotheticals, line items; refresh replaces', () => {
  // Rewrites every money string to a unique value so any index mixup shows.
  function big(seed: number): ArcheraComparison {
    const c = clone();
    let n = seed;
    const uniq = (node: unknown, key = ''): unknown => {
      if (Array.isArray(node)) return node.map(x => uniq(x, key));
      if (node && typeof node === 'object') {
        return Object.fromEntries(Object.entries(node).map(([k, v]) => [k, uniq(v, k)]));
      }
      return typeof node === 'string' && /^(monthly_730h_|upfront_one_time_cost$|discount_rate$|breakeven_days$)/.test(key)
        ? String(n++) : node;
    };
    const base = uniq(c) as ArcheraComparison;
    const h0 = base.hypotheticals[0]!;
    base.hypotheticals = [h0, JSON.parse(JSON.stringify(uniq(h0))) as typeof h0];
    base.hypotheticals.forEach((h, i) => {
      h.contract_term = `term-${seed}-${i}`;
      const li0 = h.line_items[0]!;
      h.line_items = [0, 1].map(j => ({ ...li0, line_item_id: `li-${seed}-${i}-${j}`, reason: `reason-${i}-${j}` }));
    });
    const r0 = base.rows[0]!;
    const mk = (i: number): typeof r0 => {
      const r = JSON.parse(JSON.stringify(uniq(r0))) as typeof r0;
      r.line_item_id = `row-${seed}-${i}`;
      const cand0 = r.candidates[0]!;
      r.candidates = [0, 1].map(j => ({ ...JSON.parse(JSON.stringify(uniq(cand0))), offer_id: `cand-${seed}-${i}-${j}` }));
      r.current.offer_id = `cur-${seed}-${i}`;
      return r;
    };
    base.rows = [mk(0), mk(1)];
    return base;
  }

  function offerMoney(o: ArcheraOffer): string[] {
    return [o.discount_rate, o.breakeven_days, o.monthly.monthly_730h_net_savings, o.upfront_one_time_cost,
      o.delta_vs_current.monthly_730h_net_savings, o.delta_vs_current.upfront_one_time_cost] as string[];
  }

  it('every row, candidate, hypothetical and line item renders its own values in order', async () => {
    const c = big(1000);
    const root = await mount();
    await clickAndLoad(root, c);
    const tables = Array.from(root.querySelectorAll('table'));
    // totals + hypotheticals + one per row
    expect(tables).toHaveLength(2 + c.rows.length);
    const hyp = tables[1]!;
    const hrows = Array.from(hyp.querySelectorAll('tbody tr'));
    expect(hrows).toHaveLength(2);
    c.hypotheticals.forEach((h, i) => {
      const cells = Array.from(hrows[i]!.children).map(x => x.textContent);
      expect(cells.slice(0, 2)).toEqual([h.contract_term, h.payment_option]);
      expect(cells.slice(2, 8)).toEqual([
        h.totals.monthly_730h_commitment_cost_total, h.totals.monthly_730h_net_savings, h.totals.upfront_one_time_cost,
        h.delta_vs_current.monthly_730h_net_savings, h.delta_vs_current.monthly_730h_commitment_cost,
        h.delta_vs_current.upfront_one_time_cost]);
      const lis = Array.from(hrows[i]!.children[8]!.children).map(x => x.textContent);
      expect(lis).toHaveLength(2);
      h.line_items.forEach((li, j) => expect(lis[j]).toContain(`${li.line_item_id}: ${li.reason}`));
    });
    c.rows.forEach((r, i) => {
      const t = tables[2 + i]!;
      expect(t.querySelector('caption')!.textContent).toBe(`Offers for line item ${r.line_item_id}`);
      const trs = Array.from(t.querySelectorAll('tbody tr'));
      const offers = [r.current, ...r.candidates];
      expect(trs).toHaveLength(3);
      offers.forEach((o, k) => {
        const cells = Array.from(trs[k]!.children).map(x => x.textContent);
        expect(cells[0]).toContain(o.offer_id);
        expect(cells.slice(7)).toEqual(offerMoney(o));
      });
    });
    expect(root.querySelectorAll('h3')).toHaveLength(2);
  });

  it('Refresh replaces the old view: one set of tables, new values, no stale ones', async () => {
    const first = big(2000);
    const second = big(3000);
    const root = await mount();
    await clickAndLoad(root, first);
    const oldVal = first.current.monthly_730h_net_savings!;
    expect(root.textContent).toContain(`row-2000-0`);
    comparison.mockResolvedValueOnce(second);
    root.querySelector('button')!.click();
    await flush();
    expect(root.querySelectorAll('table')).toHaveLength(2 + second.rows.length);
    expect(root.querySelectorAll('h2')).toHaveLength(1);
    expect(root.textContent).toContain('row-3000-0');
    expect(root.textContent).not.toContain('row-2000-0');
    expect(root.textContent).toContain(second.current.monthly_730h_net_savings!);
    expect(root.textContent).not.toContain(`li-2000`);
    void oldVal;
  });
});

describe('aria-busy and empty vendor strings', () => {
  it('aria-busy is absent after success and after error', async () => {
    const root = await mount();
    const button = root.querySelector('button')!;
    await clickAndLoad(root, golden);
    expect(button.hasAttribute('aria-busy')).toBe(false);
    comparison.mockRejectedValueOnce(Object.assign(new Error('fixed server text'), { status: 502 }));
    button.click();
    expect(button.getAttribute('aria-busy')).toBe('true');
    await flush();
    expect(button.hasAttribute('aria-busy')).toBe(false);
  });

  const VENDOR_KEYS = new Set([
    'offer_id', 'provider', 'commitment_type', 'region', 'contract_term', 'payment_option',
    'lease_menu_item_id', 'archera_offer_name', 'line_item_id', 'reason', 'actual_term',
    'actual_payment_option', 'actual_commitment_type',
  ]);
  function blank(node: unknown, key = ''): unknown {
    if (typeof node === 'string') return VENDOR_KEYS.has(key) ? '' : node;
    if (Array.isArray(node)) return node.map(x => blank(x, key));
    if (node && typeof node === 'object') {
      return Object.fromEntries(Object.entries(node).map(([k, v]) => [k, blank(v, k)]));
    }
    return node;
  }

  it('an empty vendor string renders "Not given" in every place it is shown', async () => {
    const c = blank(clone()) as ArcheraComparison;
    const root = await mount();
    await clickAndLoad(root, c);
    const NG = 'Not given';
    const hyp = Array.from(root.querySelectorAll('table')).find(t => /^Hypothetical/.test(t.querySelector('caption')!.textContent!))!;
    const hcells = Array.from(hyp.querySelectorAll('tbody tr')[0]!.children).map(x => x.textContent);
    expect(hcells[0]).toBe(NG);
    expect(hcells[1]).toBe(NG);
    expect(hcells[8]).toBe(`${NG}: ${NG} (term ${NG}, payment ${NG}, type ${NG})`);
    expect(root.querySelector('h3')!.textContent).toBe(`Line item ${NG}`);
    expect(root.querySelector('caption + thead')).not.toBeNull();
    const offers = root.querySelectorAll('table')[2]!;
    const trs = Array.from(offers.querySelectorAll('tbody tr'));
    expect(trs.length).toBeGreaterThan(1);
    trs.forEach((tr, i) => {
      const cells = Array.from(tr.children).map(x => x.textContent);
      expect(cells[0]).toBe(`${NG} (${i === 0 ? 'current' : 'candidate'})`);
      expect(cells[1]).toBe(`${NG} / ${NG}`);
      expect(cells[2]).toBe(NG);
      expect(cells[3]).toBe(`${NG} / ${NG}`);
      expect(cells[4]).toBe(i === 0 ? `lease attached (${NG})` : 'no lease');
      expect(cells[5]).toBe(NG);
    });
  });
});

describe('vendor strings are text only', () => {
  const HOSTILE = (n: number) => `<img src=x onerror=H${n}><script>S${n}</script><svg onload=V${n}>\u0007\u202e${n}`;

  // Replaces every string leaf except the enum-like ones that gate rendering.
  function poison(node: unknown, tokens: Map<string, string>, p: string): unknown {
    if (typeof node === 'string') {
      if (/(^|\.)(status|fetched_at)$/.test(p)) return node;
      const v = HOSTILE(tokens.size);
      tokens.set(p, v);
      return v;
    }
    if (Array.isArray(node)) return node.map((x, i) => poison(x, tokens, `${p}[${i}]`));
    if (node && typeof node === 'object') {
      return Object.fromEntries(Object.entries(node).map(([k, v]) => [k, poison(v, tokens, p ? `${p}.${k}` : k)]));
    }
    return node;
  }

  // Leaves the UI deliberately does not display.
  const NOT_RENDERED = /^(plan_id|rows\[\d+\]\.(current|candidates\[\d+\])\.(monthly\.monthly_730h_(commitment_cost_total|cloud_provider_cost|archera_premium|gross_savings|covered_on_demand_cost)|delta_vs_current\.(discount_rate|breakeven_days)|archera_product_support\.(underwriting_allowance|customer_eligibility))|hypotheticals\[\d+\]\.totals\.monthly_730h_(cloud_provider_cost|archera_premium|gross_savings|covered_on_demand_cost))$/;

  it('every rendered vendor string is text; no element is created from any of them', async () => {
    const tokens = new Map<string, string>();
    const c = poison(clone(), tokens, '') as ArcheraComparison;
    const root = await mount();
    await clickAndLoad(root, c);
    const rendered = root.textContent!;
    const missing: string[] = [];
    for (const [p, v] of tokens) {
      if (!NOT_RENDERED.test(p) && !rendered.includes(v)) missing.push(p);
    }
    expect(missing).toEqual([]);
    expect(root.querySelectorAll('img, script, svg, iframe, a').length).toBe(0);
    // Only the structural elements the component itself builds exist.
    const allowed = new Set(['H2', 'H3', 'P', 'DIV', 'TABLE', 'CAPTION', 'THEAD', 'TBODY', 'TR', 'TH', 'TD', 'SECTION', 'BUTTON']);
    root.querySelectorAll('*').forEach(n => expect(allowed.has(n.tagName)).toBe(true));
  });
});

describe('product support', () => {
  it('supported shows source and evidence', async () => {
    const root = await mount();
    await clickAndLoad(root, golden);
    const ps = firstOffer(golden).archera_product_support;
    expect(root.textContent).toContain('Supported (documented)');
    expect(root.textContent).toContain(ps.source!);
    expect(root.textContent).toContain(ps.evidence!);
  });

  it.each(['unknown', 'unsupported', '', 'SUPPORTED', 'garbage'])('status %p renders not-known and never "unsupported"', async st => {
    const c = clone();
    for (const r of c.rows) for (const o of [r.current, ...r.candidates]) {
      o.archera_product_support = {
        status: st, source: 'SRC-LEAK', evidence: 'EVD-LEAK',
        underwriting_allowance: 'allowed', customer_eligibility: 'eligible',
      };
    }
    const root = await mount();
    await clickAndLoad(root, c);
    const txt = root.textContent!;
    expect(txt).toContain('Not known to be supported by Archera');
    expect(txt).not.toMatch(/unsupported/i);
    expect(txt).not.toContain('SRC-LEAK');
    expect(txt).not.toContain('EVD-LEAK');
    expect(txt).not.toMatch(/allowed|eligible\b/);
    expect(txt).toContain('Underwriting allowance: Unknown');
    expect(txt).toContain('Customer eligibility: Unknown');
  });

  it('lease attached is labelled', async () => {
    const root = await mount();
    await clickAndLoad(root, golden);
    expect(root.textContent).toContain('lease attached (lease-cur)');
  });
});

describe('refresh, errors and accessibility', () => {
  it('refresh re-requests once per click; in-flight clicks are ignored; nothing polls', async () => {
    jest.useFakeTimers();
    try {
      const root = await mount();
      const button = root.querySelector('button')!;
      let resolve!: (c: ArcheraComparison) => void;
      comparison.mockReturnValueOnce(new Promise<ArcheraComparison>(r => { resolve = r; }));
      button.click();
      expect(button.disabled).toBe(true);
      expect(button.getAttribute('aria-busy')).toBe('true');
      button.click();
      expect(comparison).toHaveBeenCalledTimes(1);
      resolve(golden);
      await flush();
      expect(button.disabled).toBe(false);
      expect(button.textContent).toBe('Refresh');
      expect(document.activeElement).toBe(button);
      jest.advanceTimersByTime(10 * 60 * 1000);
      await flush();
      expect(comparison).toHaveBeenCalledTimes(1);
      comparison.mockResolvedValueOnce(golden);
      button.click();
      await flush();
      expect(comparison).toHaveBeenCalledTimes(2);
    } finally {
      jest.useRealTimers();
    }
  });

  it('never schedules timers or intervals', async () => {
    const si = jest.spyOn(global, 'setInterval');
    const st = jest.spyOn(global, 'setTimeout');
    const root = await mount();
    await clickAndLoad(root, golden);
    expect(si).not.toHaveBeenCalled();
    expect(st).not.toHaveBeenCalled();
    si.mockRestore();
    st.mockRestore();
  });

  it('429 with retry_after_seconds shows the wait; Refresh stays enabled', async () => {
    const root = await mount();
    comparison.mockRejectedValueOnce(Object.assign(new Error('Archera rate limit reached'),
      { status: 429, details: { retry_after_seconds: 30 } }));
    root.querySelector('button')!.click();
    await flush();
    const live = root.querySelector('[aria-live="polite"]')!;
    expect(live.textContent).toContain('Retry after 30 seconds');
    expect(root.querySelector('button')!.disabled).toBe(false);
  });

  it('429 without retry_after_seconds uses the real server text, saying so exactly once', async () => {
    const root = await mount();
    // Exact text from mapInsuranceError (internal/api/handler_insurance_comparison.go).
    comparison.mockRejectedValueOnce(Object.assign(new Error('Archera rate limit reached; retry-after not given'), { status: 429 }));
    root.querySelector('button')!.click();
    await flush();
    const t = root.querySelector('[aria-live="polite"]')!.textContent!;
    expect(t).toBe('Archera rate limit reached; retry-after not given');
    expect(t.match(/retry-after not given/gi)).toHaveLength(1);
  });

  it('429 whose message lacks the wait text gets it appended once', async () => {
    const root = await mount();
    comparison.mockRejectedValueOnce(Object.assign(new Error('Rate limited'), { status: 429 }));
    root.querySelector('button')!.click();
    await flush();
    expect(root.querySelector('[aria-live="polite"]')!.textContent!.match(/retry-after not given/gi)).toHaveLength(1);
  });

  it.each([502, 503])('server error %s message shown as text in the live region', async code => {
    const root = await mount();
    comparison.mockRejectedValueOnce(Object.assign(new Error('fixed server text'), { status: code }));
    root.querySelector('button')!.click();
    await flush();
    expect(root.querySelector('[aria-live="polite"]')!.textContent).toBe('fixed server text');
  });

  it('unparseable fetched_at is shown raw', () => {
    const c = clone();
    c.fetched_at = 'not-a-date';
    expect(buildComparisonView(c).querySelector('.archera-fetched-at')!.textContent).toBe('Fetched at not-a-date');
  });
});
