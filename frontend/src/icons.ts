/**
 * One SVG icon set for the whole UI (issue #751).
 *
 * Before this module the sidebar used inline Feather SVGs while row
 * actions, toasts, sort arrows, filter triggers and tooltips used Unicode
 * glyphs (▶ ⏸ ⏵ ✎ ✕ ↻ ⚠ ↕ ▲ ▼ ⛛ ⓘ ⚙︎). Those glyphs come from whatever
 * font the platform picks, so their size and weight drift between
 * browsers, and ⏸ / ⏵ / ⚠ can render as color emoji on macOS. Every icon
 * here uses the same 24x24 grid, 2px round stroke and `currentColor`, so
 * it inherits the surrounding text color and matches the sidebar icons.
 *
 * Shapes are copied from Feather (MIT, https://feathericons.com); the
 * one icon Feather lacks, chevrons-up-down, comes from its fork Lucide
 * (ISC, https://lucide.dev).
 *
 * Icons are decorative: both renderers set aria-hidden="true" and
 * focusable="false", so callers must give the surrounding control a
 * text label or an aria-label.
 */

type Attrs = Readonly<Record<string, string>>;
type Shape = readonly [tag: 'path' | 'line' | 'polyline' | 'polygon' | 'circle' | 'rect', attrs: Attrs];

const ICONS = {
  'alert-triangle': [
    ['path', { d: 'M10.29 3.86L1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z' }],
    ['line', { x1: '12', y1: '9', x2: '12', y2: '13' }],
    ['line', { x1: '12', y1: '17', x2: '12.01', y2: '17' }],
  ],
  check: [['polyline', { points: '20 6 9 17 4 12' }]],
  'check-circle': [
    ['path', { d: 'M22 11.08V12a10 10 0 1 1-5.93-9.14' }],
    ['polyline', { points: '22 4 12 14.01 9 11.01' }],
  ],
  'chevron-down': [['polyline', { points: '6 9 12 15 18 9' }]],
  'chevron-right': [['polyline', { points: '9 18 15 12 9 6' }]],
  'chevron-up': [['polyline', { points: '18 15 12 9 6 15' }]],
  'chevrons-up-down': [
    ['path', { d: 'm7 15 5 5 5-5' }],
    ['path', { d: 'm7 9 5-5 5 5' }],
  ],
  copy: [
    ['rect', { x: '9', y: '9', width: '13', height: '13', rx: '2', ry: '2' }],
    ['path', { d: 'M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1' }],
  ],
  download: [
    ['path', { d: 'M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4' }],
    ['polyline', { points: '7 10 12 15 17 10' }],
    ['line', { x1: '12', y1: '15', x2: '12', y2: '3' }],
  ],
  edit: [['path', { d: 'M17 3a2.828 2.828 0 1 1 4 4L7.5 20.5 2 22l1.5-5.5L17 3z' }]],
  filter: [['polygon', { points: '22 3 2 3 10 12.46 10 19 14 21 14 12.46 22 3' }]],
  info: [
    ['circle', { cx: '12', cy: '12', r: '10' }],
    ['line', { x1: '12', y1: '16', x2: '12', y2: '12' }],
    ['line', { x1: '12', y1: '8', x2: '12.01', y2: '8' }],
  ],
  pause: [
    ['rect', { x: '6', y: '4', width: '4', height: '16' }],
    ['rect', { x: '14', y: '4', width: '4', height: '16' }],
  ],
  play: [['polygon', { points: '5 3 19 12 5 21 5 3' }]],
  'rotate-cw': [
    ['polyline', { points: '23 4 23 10 17 10' }],
    ['path', { d: 'M20.49 15a9 9 0 1 1-2.12-9.36L23 10' }],
  ],
  settings: [
    ['circle', { cx: '12', cy: '12', r: '3' }],
    ['path', { d: 'M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 1 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06A1.65 1.65 0 0 0 4.6 15a1.65 1.65 0 0 0-1.51-1H3a2 2 0 1 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 1 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 1 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z' }],
  ],
  slash: [
    ['circle', { cx: '12', cy: '12', r: '10' }],
    ['line', { x1: '4.93', y1: '4.93', x2: '19.07', y2: '19.07' }],
  ],
  'trash-2': [
    ['polyline', { points: '3 6 5 6 21 6' }],
    ['path', { d: 'M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2' }],
    ['line', { x1: '10', y1: '11', x2: '10', y2: '17' }],
    ['line', { x1: '14', y1: '11', x2: '14', y2: '17' }],
  ],
  x: [
    ['line', { x1: '18', y1: '6', x2: '6', y2: '18' }],
    ['line', { x1: '6', y1: '6', x2: '18', y2: '18' }],
  ],
  'x-circle': [
    ['circle', { cx: '12', cy: '12', r: '10' }],
    ['line', { x1: '15', y1: '9', x2: '9', y2: '15' }],
    ['line', { x1: '9', y1: '9', x2: '15', y2: '15' }],
  ],
} as const satisfies Record<string, readonly Shape[]>;

export type IconName = keyof typeof ICONS;

const SVG_NS = 'http://www.w3.org/2000/svg';

/** Attributes shared by every icon's <svg> root. Sized in em so the icon tracks the font size of its context. */
function rootAttrs(name: IconName): Attrs {
  return {
    class: `icon icon-${name}`,
    viewBox: '0 0 24 24',
    width: '1em',
    height: '1em',
    fill: 'none',
    stroke: 'currentColor',
    'stroke-width': '2',
    'stroke-linecap': 'round',
    'stroke-linejoin': 'round',
    'aria-hidden': 'true',
    focusable: 'false',
  };
}

function attrString(attrs: Attrs): string {
  return Object.entries(attrs).map(([k, v]) => `${k}="${v}"`).join(' ');
}

/**
 * Markup for an icon, for use inside the template-literal renderers
 * (`<button>${iconSvg('play')}</button>`). The output is built only from
 * the constant table above, never from caller data, so it is safe to
 * interpolate into innerHTML.
 */
export function iconSvg(name: IconName): string {
  const shapes = (ICONS[name] as readonly Shape[])
    .map(([tag, attrs]) => `<${tag} ${attrString(attrs)}/>`)
    .join('');
  return `<svg ${attrString(rootAttrs(name))}>${shapes}</svg>`;
}

/** DOM version of iconSvg for the createElement-based renderers that avoid innerHTML. */
export function createIcon(name: IconName): SVGSVGElement {
  const svg = document.createElementNS(SVG_NS, 'svg');
  for (const [k, v] of Object.entries(rootAttrs(name))) svg.setAttribute(k, v);
  for (const [tag, attrs] of ICONS[name] as readonly Shape[]) {
    const child = document.createElementNS(SVG_NS, tag);
    for (const [k, v] of Object.entries(attrs)) child.setAttribute(k, v);
    svg.appendChild(child);
  }
  return svg;
}
