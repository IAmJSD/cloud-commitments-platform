import * as fs from 'fs';
import * as path from 'path';

/**
 * CSS-level regression test for issue #10 (responsive nav).
 *
 * JSDOM does not evaluate @media queries, so asserting on
 * getComputedStyle(el).flexWrap would silently pass even if the rule
 * were deleted. Reading the source CSS and asserting on its contents
 * is the only way to lock down the fix.
 */
describe('responsive.css nav wrap rules', () => {
  const css = fs.readFileSync(
    path.join(__dirname, '../styles/responsive.css'),
    'utf-8',
  );

  // Issue #10 wrapped the old top-bar .tabs row at <=1100px and <=768px. The
  // main nav has since moved into the sidebar (.app-sidebar-nav .tab-btn in
  // layout.css), so those rules matched nothing (.tabs) or were outranked
  // (.tab-btn) and were removed as dead code (issue #725).
  it('no longer carries the dead top-bar .tabs / .tab-btn rules', () => {
    expect(css).not.toMatch(/(^|\n)\s*\.tabs\s*{/);
    expect(css).not.toMatch(/(^|\n)\s*\.tab-btn\s*{/);
    expect(css).not.toMatch(/@media\s*\(max-width:\s*1100px\)/);
  });

  /**
   * On the lazy `([\s\S]*?)\n}` capture used here and below: it does reach
   * the end of the 768px block, despite looking like it would stop at the
   * first nested rule. Every rule inside the block is indented, so the
   * block's own closing brace is the only `}` at column zero and `\n}` can
   * only anchor there. Measured against a brace-matched extraction: the true
   * body is 5260 characters, the capture is 5259, the whole difference being
   * the trailing newline the pattern consumes.
   *
   * This holds only while the stylesheet stays formatted that way. A nested
   * rule closed at column zero would truncate the capture and these
   * assertions would start passing or failing for the wrong reason.
   *
   * Issue #10 wrapped #user-info at ≤768px to stop it overflowing the
   * header. That never worked: the topbar is a fixed --cudly-topbar-h box,
   * so the extra rows painted over the page content instead (issue #1779).
   * The regions are relocated into the drawer now, and the topbar copies
   * are laid out away rather than left to paint behind the content.
   *
   * Source-text assertions only -- whether the relocated controls are
   * actually visible and tappable is measured in a real browser by
   * tests-e2e/mobile-header-drawer.spec.ts.
   */
  it('takes #user-info and #topbar-filters out of the topbar at ≤768px', () => {
    const match = css.match(/@media\s*\(max-width:\s*768px\)\s*{([\s\S]*?)\n}/);
    expect(match).not.toBeNull();
    const body = match?.[1] ?? '';
    expect(body).toMatch(
      /\.app-topbar\s*>\s*#topbar-filters,\s*\.app-topbar\s*>\s*#user-info\s*{[\s\S]*?display:\s*none/,
    );
  });

  it('styles both regions for the drawer slot at ≤768px', () => {
    const match = css.match(/@media\s*\(max-width:\s*768px\)\s*{([\s\S]*?)\n}/);
    expect(match).not.toBeNull();
    const body = match?.[1] ?? '';
    expect(body).toMatch(/\.app-sidebar-extras\s*{[\s\S]*?display:\s*flex/);
    expect(body).toMatch(/\.app-sidebar-extras #topbar-filters/);
    expect(body).toMatch(/\.app-sidebar-extras #user-info/);
  });

  it('does not stack the topbar into a column at ≤768px', () => {
    const match = css.match(/@media\s*\(max-width:\s*768px\)\s*{([\s\S]*?)\n}/);
    expect(match).not.toBeNull();
    const body = match?.[1] ?? '';
    // The `header { flex-direction: column }` rule was the overflow source.
    expect(body).not.toMatch(/(^|\n)\s*header\s*{/);
    expect(body).toMatch(/\.app-topbar\s*{[\s\S]*?justify-content:\s*flex-start/);
  });
});
