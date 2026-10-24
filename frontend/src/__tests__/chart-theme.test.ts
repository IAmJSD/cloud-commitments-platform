/**
 * Tests for chart colors from design tokens (issue #699).
 */
import fs from 'fs';
import path from 'path';
import {
  CHART_TOKEN_FALLBACKS, applyChartDefaults, chartColor, chartPalette, requireToken, withAlpha,
  type ChartToken,
} from '../lib/chart-theme';

const SRC = path.join(__dirname, '..');
const STYLES = path.join(SRC, 'styles');
const TOKENS = Object.keys(CHART_TOKEN_FALLBACKS) as ChartToken[];

/** The declarations of the first :root block in base.css. */
function baseRootTokens(): Map<string, string> {
  const css = fs.readFileSync(path.join(STYLES, 'base.css'), 'utf8');
  const root = /:root\s*\{([\s\S]*?)\n\}/.exec(css)![1]!;
  const out = new Map<string, string>();
  for (const m of root.matchAll(/(--cudly-[\w-]+)\s*:\s*([^;]+);/g)) out.set(m[1]!, m[2]!.trim());
  return out;
}

/** Hex / rgb() / hsl() color literals in CSS, ignoring comments, token definitions and var() fallbacks. */
function cssColorLiterals(css: string): string[] {
  const code = css
    .replace(/\/\*[\s\S]*?\*\//g, '')
    .split('\n')
    .filter((line) => !/^\s*--[\w-]+\s*:/.test(line))
    .join('\n')
    .replace(/var\(--[\w-]+\s*,[^)]*\)/g, '');
  return code.match(/#[0-9a-fA-F]{3,8}\b|\b(?:rgba?|hsla?)\(/g) ?? [];
}

afterEach(() => {
  document.documentElement.removeAttribute('style');
});

describe('chart-theme', () => {
  it('fallbacks match the --cudly-chart-* tokens defined in base.css', () => {
    const defined = baseRootTokens();
    for (const name of TOKENS) expect([name, defined.get(name)]).toEqual([name, CHART_TOKEN_FALLBACKS[name]]);
    const cssChartTokens = [...defined.keys()].filter((k) => k.startsWith('--cudly-chart-'));
    expect(cssChartTokens.sort()).toEqual([...TOKENS].sort());
  });

  it('series and layer tokens are #rrggbb so charts can derive shades from them', () => {
    for (const name of TOKENS) {
      if (/text|grid/.test(name)) continue;
      expect(CHART_TOKEN_FALLBACKS[name]).toMatch(/^#[0-9a-f]{6}$/);
    }
  });

  it('chartColor prefers the live token over the fallback', () => {
    expect(chartColor('--cudly-chart-1')).toBe('#1a73e8');
    document.documentElement.style.setProperty('--cudly-chart-1', '#8ab4f8');
    expect(chartColor('--cudly-chart-1')).toBe('#8ab4f8');
    expect(chartPalette()[0]).toBe('#8ab4f8');
    expect(chartPalette()).toHaveLength(10);
  });

  it('requireToken throws for a missing token', () => {
    expect(() => requireToken('--cudly-does-not-exist')).toThrow('Missing CSS token --cudly-does-not-exist');
    document.documentElement.style.setProperty('--cudly-primary', '#2563eb');
    expect(requireToken('--cudly-primary')).toBe('#2563eb');
  });

  it('withAlpha turns #rrggbb into rgba()', () => {
    expect(withAlpha('#1a73e8', 0.1)).toBe('rgba(26,115,232,0.1)');
    expect(withAlpha('not-a-color', 0.5)).toBe('rgba(26,115,232,0.5)');
  });

  it('applyChartDefaults points Chart.js text and gridlines at the tokens', () => {
    const defaults = { color: '#666', borderColor: 'rgba(0,0,0,0.1)' };
    document.documentElement.style.setProperty('--cudly-chart-text', '#e5e7eb');
    applyChartDefaults(defaults);
    expect(defaults).toEqual({ color: '#e5e7eb', borderColor: 'rgba(0, 0, 0, 0.1)' });
    expect(() => applyChartDefaults(undefined)).not.toThrow();
  });

  it('the dashboard follows a themed palette', () => {
    const { savingsLayerColors } = require('../dashboard') as {
      savingsLayerColors: () => { current: string; lowestOption: string; upside: string };
    };
    expect(savingsLayerColors().current).toBe('#2a78d6');
    document.documentElement.style.setProperty('--cudly-chart-layer-current', '#8ab4f8');
    expect(savingsLayerColors().current).toBe('#8ab4f8');
  });
});

describe('color literals (theming ratchet)', () => {
  // Chart modules must take every color from lib/chart-theme.ts.
  it.each(['dashboard.ts', 'modules/savings-history.ts', 'ladder-timeline-chart.ts'])('%s has no color literals', (file) => {
    const code = fs.readFileSync(path.join(SRC, file), 'utf8')
      .split('\n')
      .filter((line) => !/^\s*(\/\/|\*|\/\*)/.test(line))
      .map((line) => line.replace(/\/\/.*$/, ''))
      .join('\n');
    expect(code.match(/'#[0-9a-fA-F]{3,8}'|\brgba?\(\s*\d/g)).toBeNull();
  });

  // Stylesheet literals bypass the tokens, so a theme cannot reach them.
  // This ceiling may only go down: migrate a literal to a token, then lower
  // it. Raising it means a new hard-coded color; use or add a token instead.
  const CSS_LITERAL_CEILING = 139;

  it(`styles/*.css hold at most ${CSS_LITERAL_CEILING} color literals outside token definitions`, () => {
    const counts = fs.readdirSync(STYLES)
      .filter((f) => f.endsWith('.css'))
      .map((f) => [f, cssColorLiterals(fs.readFileSync(path.join(STYLES, f), 'utf8')).length] as const);
    const total = counts.reduce((n, [, c]) => n + c, 0);
    expect(total).toBeLessThanOrEqual(CSS_LITERAL_CEILING);
  });
});
