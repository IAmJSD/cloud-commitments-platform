/**
 * Issue #743: one dialog surface and one card surface.
 *
 * The four modal systems (.modal + .modal-content, .modal-overlay,
 * confirmDialog's .modal-confirm, the Archera offer panel) each had their
 * own scrim opacity, radius, shadow and z-index. The card styles differed in
 * border, shadow and fill. These read the stylesheets (jsdom has no cascade)
 * and pin every surface to the shared tokens.
 */
import fs from 'fs';
import path from 'path';

const stylesDir = path.join(__dirname, '..', 'styles');
const all = fs.readdirSync(stylesDir).filter((f) => f.endsWith('.css'))
  .map((f) => fs.readFileSync(path.join(stylesDir, f), 'utf8').replace(/\/\*[\s\S]*?\*\//g, ''))
  .join('\n');

/** Declarations of the rule whose whole selector list is `selector`. */
function rule(selector: string): Record<string, string> {
  const re = /([^{}]+)\{([^{}]*)\}/g;
  let m: RegExpExecArray | null;
  while ((m = re.exec(all)) !== null) {
    if (m[1]!.trim().replace(/\s+/g, ' ') !== selector) continue;
    const out: Record<string, string> = {};
    for (const d of m[2]!.split(';')) {
      const i = d.indexOf(':');
      if (i > 0) out[d.slice(0, i).trim()] = d.slice(i + 1).trim();
    }
    return out;
  }
  throw new Error(`rule not found: ${selector}`);
}

describe('design tokens', () => {
  test.each([
    '--cudly-scrim', '--cudly-shadow-dialog', '--cudly-z-modal', '--cudly-z-modal-raised',
    '--cudly-z-page-overlay', '--cudly-z-confirm', '--cudly-z-toast',
  ])('%s is defined', (token) => {
    expect(all).toMatch(new RegExp(`${token}:\\s*[^;]+;`));
  });

  test('the stacking ladder keeps confirm above modals and toasts on top', () => {
    const z = (t: string) => Number(new RegExp(`${t}:\\s*(\\d+)`).exec(all)![1]);
    expect(z('--cudly-z-modal')).toBeLessThan(z('--cudly-z-modal-raised'));
    expect(z('--cudly-z-modal-raised')).toBeLessThan(z('--cudly-z-page-overlay'));
    expect(z('--cudly-z-page-overlay')).toBeLessThan(z('--cudly-z-confirm'));
    expect(z('--cudly-z-confirm')).toBeLessThan(z('--cudly-z-toast'));
  });
});

describe('one dialog surface (issue #743)', () => {
  test.each(['.modal', '.modal-overlay', '.modal-confirm-backdrop', '.archera-offer-backdrop'])(
    '%s uses the shared scrim', (sel) => {
      expect(rule(sel)['background']).toBe('var(--cudly-scrim)');
    },
  );

  test.each(['.modal-content', '.modal-confirm', '.archera-offer-panel'])(
    '%s uses the shared radius, shadow and surface', (sel) => {
      const r = rule(sel);
      expect(r['border-radius']).toBe('var(--cudly-r-lg)');
      expect(r['box-shadow']).toBe('var(--cudly-shadow-dialog)');
      expect(r['background']).toBe('var(--cudly-surface)');
    },
  );

  test.each([
    ['.modal', 'var(--cudly-z-modal)'],
    ['.modal-overlay', 'var(--cudly-z-modal)'],
    ['#archera-offer-modal-container', 'var(--cudly-z-modal-raised)'],
    ['#archera-page-container', 'var(--cudly-z-page-overlay)'],
    ['.modal-confirm-backdrop', 'var(--cudly-z-confirm)'],
    ['.toast-container', 'var(--cudly-z-toast)'],
  ])('%s stacks at %s', (sel, z) => {
    expect(rule(sel)['z-index']).toBe(z);
  });
});

describe('one card surface (issue #743)', () => {
  test('.card is a hairline border + small shadow on the surface color', () => {
    const r = rule('.card');
    expect(r['border']).toBe('1px solid var(--cudly-border)');
    expect(r['box-shadow']).toBe('var(--cudly-shadow-sm)');
    expect(r['border-radius']).toBe('var(--cudly-r-md)');
    expect(r['background']).toBe('var(--cudly-surface)');
  });

  test('.kpi-tile inherits the surface from .card instead of redefining it', () => {
    const r = rule('.kpi-tile');
    expect(r['border']).toBeUndefined();
    expect(r['box-shadow']).toBeUndefined();
  });

  test.each(['.plan-card', '.group-card', '.upcoming-card'])('%s matches the card surface', (sel) => {
    const r = rule(sel);
    expect(r['border']).toBe('1px solid var(--cudly-border)');
    expect(r['box-shadow']).toBe('var(--cudly-shadow-sm)');
    expect(r['border-radius']).toBe('var(--cudly-r-md)');
    expect(r['background']).toBe('var(--cudly-surface)');
  });
});
