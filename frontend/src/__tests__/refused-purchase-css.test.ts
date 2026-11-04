/** Height caps for long failure text (issue #1694); jsdom has no layout, so assert the CSS rules. */
import * as fs from 'fs';
import * as path from 'path';

const css = fs.readFileSync(path.join(__dirname, '../styles/components.css'), 'utf8');
const rule = (sel: string): string => {
  const m = css.match(new RegExp(`(?:^|\\n)${sel.replace('.', '\\.')}\\s*\\{([^}]*)\\}`));
  return m?.[1] ?? '';
};

test('toast message is height-capped and scrolls', () => {
  const r = rule('.toast-message');
  expect(r).toMatch(/max-height:\s*\d/);
  expect(r).toMatch(/overflow-y:\s*auto/);
});

test('history status description is line-clamped', () => {
  const r = rule('.history-status-desc');
  expect(r).toMatch(/-webkit-line-clamp:\s*\d/);
  expect(r).toMatch(/overflow:\s*hidden/);
});
