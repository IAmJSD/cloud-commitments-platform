/**
 * Chart colors from design tokens (issue #699).
 *
 * Chart.js paints on a <canvas>, so its colors cannot come from a
 * stylesheet the way the rest of the UI does: each chart has to read the
 * CSS custom properties at render time. Before this module the dashboard
 * and savings-history charts hard-coded their hex values, so a theme
 * (a dark palette, a contrast fix) could restyle every surface except the
 * charts. The `--cudly-chart-*` tokens live in styles/base.css next to the
 * other design tokens; overriding them there restyles every chart.
 *
 * The values below are fallbacks for environments without the stylesheet
 * (jsdom tests, a failed CSS load). They must match base.css; a jest test
 * compares the two so they cannot drift.
 *
 * Series and legend tokens must be #rrggbb: the charts derive darker and
 * translucent shades from them (see withAlpha and darkenHexColor).
 */

export const CHART_TOKEN_FALLBACKS = {
  // Per-service bar palette. A service keeps its slot across refreshes
  // (dashboard.serviceBarColor), so reordering these recolors services.
  '--cudly-chart-1': '#1a73e8',
  '--cudly-chart-2': '#34a853',
  '--cudly-chart-3': '#fbbc04',
  '--cudly-chart-4': '#ea4335',
  '--cudly-chart-5': '#9c27b0',
  '--cudly-chart-6': '#00bcd4',
  '--cudly-chart-7': '#ff5722',
  '--cudly-chart-8': '#607d8b',
  '--cudly-chart-9': '#795548',
  '--cudly-chart-10': '#e91e63',
  // Cumulative savings line on the savings-history chart.
  '--cudly-chart-cumulative': '#4285f4',
  // Neutral legend swatches for the stacked savings-range layers.
  '--cudly-chart-legend-dark': '#37474f',
  '--cudly-chart-legend-mid': '#78909c',
  '--cudly-chart-legend-border': '#546e7a',
  // Axis labels and gridlines (Chart.js's own defaults until now).
  '--cudly-chart-text': '#666666',
  '--cudly-chart-grid': 'rgba(0, 0, 0, 0.1)',
  '--cudly-chart-grid-faint': 'rgba(0, 0, 0, 0.05)',
} as const;

export type ChartToken = keyof typeof CHART_TOKEN_FALLBACKS;

const SERIES: readonly ChartToken[] = [
  '--cudly-chart-1', '--cudly-chart-2', '--cudly-chart-3', '--cudly-chart-4', '--cudly-chart-5',
  '--cudly-chart-6', '--cudly-chart-7', '--cudly-chart-8', '--cudly-chart-9', '--cudly-chart-10',
];

/** The computed value of a CSS custom property on :root, or '' when unset. */
export function readToken(name: string): string {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
}

/** Resolve a design token; a missing token is a bug, not a reason to paint a default color. */
export function requireToken(name: string): string {
  const value = readToken(name);
  if (!value) throw new Error(`Missing CSS token ${name}`);
  return value;
}

/** A chart color: the token's themed value, else its base.css default. */
export function chartColor(name: ChartToken): string {
  return readToken(name) || CHART_TOKEN_FALLBACKS[name];
}

/** The per-service series palette, in slot order. */
export function chartPalette(): string[] {
  return SERIES.map(chartColor);
}

/**
 * Parse a #rrggbb hex string into its r/g/b components. Falls back to the
 * first series color for malformed input so a chart never renders a NaN
 * color.
 */
export function parseHexColor(hex: string): { r: number; g: number; b: number } {
  const m = /^#?([0-9a-fA-F]{6})$/.exec(hex.trim());
  const clean = m ? m[1]! : CHART_TOKEN_FALLBACKS['--cudly-chart-1'].slice(1);
  return {
    r: parseInt(clean.substring(0, 2), 16),
    g: parseInt(clean.substring(2, 4), 16),
    b: parseInt(clean.substring(4, 6), 16),
  };
}

/** The same #rrggbb color at the given opacity, as rgba(). */
export function withAlpha(hex: string, alpha: number): string {
  const { r, g, b } = parseHexColor(hex);
  return `rgba(${r},${g},${b},${alpha})`;
}

/** Minimal shape of Chart.defaults this module writes, so tests need not load Chart.js. */
interface ChartDefaultsLike {
  color: unknown;
  borderColor: unknown;
}

/**
 * Point Chart.js's global text and gridline colors at the theme tokens.
 * Call right before creating a chart, not at import time: the stylesheet
 * may not have applied yet when the module first loads.
 */
export function applyChartDefaults(defaults: ChartDefaultsLike | undefined): void {
  // Unit tests replace Chart.js with a mock that has no defaults object.
  if (!defaults) return;
  defaults.color = chartColor('--cudly-chart-text');
  defaults.borderColor = chartColor('--cudly-chart-grid');
}
