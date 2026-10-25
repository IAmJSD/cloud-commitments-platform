/**
 * Every CSS custom property the frontend reads must be defined (issue #745).
 * An undefined name silently resolves to its fallback, so a retheme of the
 * real --cudly-* tokens never reaches the element.
 */
import fs from 'fs';
import path from 'path';

const srcDir = path.join(__dirname, '..');
const stylesDir = path.join(srcDir, 'styles');

function sources(dir: string, exts: string[]): string[] {
  return fs.readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) return entry.name === '__tests__' ? [] : sources(full, exts);
    return exts.some((ext) => entry.name.endsWith(ext)) ? [full] : [];
  });
}

const stylesheets = sources(stylesDir, ['.css']).map((f) => fs.readFileSync(f, 'utf8')).join('\n');
const defined = new Set([...stylesheets.matchAll(/(--[\w-]+)\s*:/g)].map((m) => m[1]));

test('stylesheets define the custom properties the MFA panels and banners need', () => {
  expect(defined.has('--cudly-font-mono')).toBe(true);
});

test.each(sources(srcDir, ['.css', '.ts', '.html']).map((f) => [path.relative(srcDir, f), f]))(
  '%s only reads defined custom properties',
  (_name, file) => {
    const text = fs.readFileSync(file, 'utf8');
    // Skip template-built names such as var(--cudly-${token}).
    const used = [...text.matchAll(/var\(\s*(--[\w-]+)(?![\w-]*\$\{)/g)].map((m) => m[1]!);
    expect(used.filter((name) => !defined.has(name))).toEqual([]);
  },
);
