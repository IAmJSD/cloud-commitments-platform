/**
 * Tests for the shared SVG icon set (issue #751).
 */
import fs from 'fs';
import path from 'path';
import { createIcon, iconSvg, type IconName } from '../icons';
import { showToast } from '../toast';

const ALL: IconName[] = [
  'alert-triangle', 'check', 'check-circle', 'chevron-down', 'chevron-right',
  'chevron-up', 'chevrons-up-down', 'download', 'edit', 'filter', 'info',
  'pause', 'play', 'rotate-cw', 'settings', 'x', 'x-circle',
];

function parse(markup: string): SVGSVGElement {
  const host = document.createElement('div');
  host.innerHTML = markup;
  return host.firstElementChild as SVGSVGElement;
}

describe('icons', () => {
  it.each(ALL)('%s renders as a decorative currentColor SVG', (name) => {
    const svg = parse(iconSvg(name));
    expect(svg.tagName.toLowerCase()).toBe('svg');
    expect(svg.getAttribute('class')).toBe(`icon icon-${name}`);
    expect(svg.getAttribute('viewBox')).toBe('0 0 24 24');
    expect(svg.getAttribute('stroke')).toBe('currentColor');
    expect(svg.getAttribute('aria-hidden')).toBe('true');
    expect(svg.getAttribute('focusable')).toBe('false');
    expect(svg.childElementCount).toBeGreaterThan(0);
  });

  it.each(ALL)('createIcon(%s) builds the same SVG as iconSvg', (name) => {
    const built = createIcon(name);
    expect(built.namespaceURI).toBe('http://www.w3.org/2000/svg');
    expect(built.outerHTML).toBe(parse(iconSvg(name)).outerHTML);
  });

  it('toasts use an SVG icon and an SVG close button for every kind', () => {
    for (const kind of ['success', 'info', 'warning', 'error'] as const) {
      showToast({ message: kind, kind, timeout: null });
    }
    const toasts = Array.from(document.querySelectorAll('.toast'));
    expect(toasts).toHaveLength(4);
    for (const t of toasts) {
      expect(t.querySelector('.toast-icon svg.icon')).not.toBeNull();
      expect(t.querySelector('.toast-icon')?.textContent).toBe('');
      expect(t.querySelector('.toast-close svg.icon-x')).not.toBeNull();
    }
    document.body.replaceChildren();
  });

  // Regression guard: the platform-font glyphs replaced by the icon set
  // must not creep back into the rendered UI. Comments and tests are
  // allowed to name them.
  it('the replaced Unicode icon glyphs no longer appear in UI source', () => {
    const banned = /[⏸⏵✎⛛ⓘ⚙↻]|&#9432;|&#x26DB;|\\u26db|\\u2195/i;
    const srcDir = path.join(__dirname, '..');
    const offenders: string[] = [];
    const walk = (dir: string): void => {
      for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
        const full = path.join(dir, entry.name);
        if (entry.isDirectory()) {
          if (entry.name !== '__tests__') walk(full);
          continue;
        }
        if (!/\.(ts|html)$/.test(entry.name) || entry.name === 'docs.html') continue;
        fs.readFileSync(full, 'utf8').split('\n').forEach((line, i) => {
          const code = line.replace(/\/\/.*$/, '').replace(/^\s*\*.*$/, '');
          if (banned.test(code)) offenders.push(`${path.relative(srcDir, full)}:${i + 1}`);
        });
      }
    };
    walk(srcDir);
    expect(offenders).toEqual([]);
  });
});
