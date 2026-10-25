/**
 * Issue #191: CSS guards for the 2026-08 UX review polish items.
 *
 *  - Summary-card rows (four cards) step 4 -> 2x2 -> 1 instead of an auto-fit
 *    grid that wrapped them 3 + 1 on mid-width screens.
 *  - Muted text and error text that sat under WCAG AA (4.5:1) now use tokens
 *    that pass on the backgrounds they render on.
 *
 * jsdom does not compute layout or cascade, so these read the stylesheets
 * directly; the Playwright spec tests-e2e/ui-polish.spec.ts covers the real
 * rendered grid.
 */
import fs from 'fs';
import path from 'path';

const stylesDir = path.join(__dirname, '..', 'styles');
const read = (file: string): string => fs.readFileSync(path.join(stylesDir, file), 'utf8');

/** Declarations of the first rule whose selector list is exactly `selector`. */
function ruleBody(css: string, selector: string): string {
  const stripped = css.replace(/\/\*[\s\S]*?\*\//g, '');
  const re = /([^{}]+)\{([^{}]*)\}/g;
  let m: RegExpExecArray | null;
  while ((m = re.exec(stripped)) !== null) {
    const sel = m[1]!.trim().replace(/\s+/g, ' ');
    if (sel === selector) return m[2]!;
  }
  throw new Error(`rule not found: ${selector}`);
}

function decl(body: string, prop: string): string {
  const m = new RegExp(`(?:^|;|\\s)${prop}\\s*:\\s*([^;]+)`).exec(body);
  if (!m) throw new Error(`${prop} not set`);
  return m[1]!.trim();
}

const tokens = (() => {
  const base = read('base.css');
  const out: Record<string, string> = {};
  for (const m of base.matchAll(/(--cudly-[\w-]+):\s*(#[0-9a-fA-F]{3,6})\b/g)) out[m[1]!] = m[2]!;
  return out;
})();

function resolveColor(value: string): string {
  const v = /var\((--cudly-[\w-]+)\)/.exec(value);
  const hex = v ? tokens[v[1]!] : value;
  if (!hex || !/^#[0-9a-fA-F]{3,6}$/.test(hex)) throw new Error(`cannot resolve color ${value}`);
  return hex;
}

function luminance(hex: string): number {
  let h = hex.slice(1);
  if (h.length === 3) h = h.split('').map((c) => c + c).join('');
  const [r, g, b] = [0, 2, 4].map((i) => parseInt(h.slice(i, i + 2), 16) / 255)
    .map((c) => (c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4)) as [number, number, number];
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

function contrast(a: string, b: string): number {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x) as [number, number];
  return (hi + 0.05) / (lo + 0.05);
}

describe('summary-card grids (issue #191)', () => {
  const FOUR = 'repeat(4, minmax(0, 1fr))';

  test('#summary and the Opportunities / History summaries use four fixed columns', () => {
    expect(decl(ruleBody(read('layout.css'), '#summary'), 'grid-template-columns')).toBe(FOUR);
    expect(decl(ruleBody(read('tables.css'), '#recommendations-summary, #history-summary'), 'grid-template-columns')).toBe(FOUR);
  });

  test('responsive.css steps to 2x2, then 1 column, never auto-fit', () => {
    const responsive = read('responsive.css');
    // The media block whose first selector is #summary (480px has another block).
    const block = (maxWidth: string): string => {
      const m = new RegExp(`@media \\(max-width: ${maxWidth}\\) \\{\\s*(#summary,[^}]*)\\}`).exec(responsive);
      if (!m) throw new Error(`no summary block at ${maxWidth}`);
      return m[1]!;
    };
    expect(block('1199px')).toMatch(/grid-template-columns:\s*repeat\(2, minmax\(0, 1fr\)\)/);
    expect(block('480px')).toMatch(/grid-template-columns:\s*minmax\(0, 1fr\)/);
    for (const id of ['#summary', '#recommendations-summary', '#history-summary']) {
      expect(block('1199px')).toContain(id);
    }
  });

  test('a non-card child (error message) spans the whole row', () => {
    const body = ruleBody(read('layout.css'), ':is(#summary, #recommendations-summary, #history-summary) > :not(.card)');
    expect(decl(body, 'grid-column')).toBe('1 / -1');
  });
});

describe('text contrast meets WCAG AA (issue #191)', () => {
  const AA = 4.5;
  // [file, selector, background it renders on]
  const cases: Array<[string, string, string]> = [
    ['modals.css', '.archera-cta', '#ffffff'],
    ['modals.css', '.archera-signup-note', '#f0f7ff'],
    ['modals.css', '.archera-offer-disclosure', '#f0f7ff'],
    ['base.css', '.error', tokens['--cudly-error-bg']!],
  ];

  test.each(cases)('%s %s', (file, selector, bg) => {
    const color = resolveColor(decl(ruleBody(read(file), selector), 'color'));
    expect(contrast(color, bg)).toBeGreaterThanOrEqual(AA);
  });

  test('the muted and subtle text tokens pass on white', () => {
    expect(contrast(tokens['--cudly-text-muted']!, '#ffffff')).toBeGreaterThanOrEqual(AA);
    expect(contrast(tokens['--cudly-text-subtle']!, '#ffffff')).toBeGreaterThanOrEqual(AA);
  });
});
