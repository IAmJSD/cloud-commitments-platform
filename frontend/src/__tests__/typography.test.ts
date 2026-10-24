/**
 * Typography foundation (issue #741): body line-height, form controls that
 * inherit the page font, a base heading scale on the --cudly-fs-* tokens, and
 * no literal sizes or weights that duplicate a token.
 */
import fs from 'fs';
import path from 'path';

const stylesDir = path.join(__dirname, '..', 'styles');
const read = (file: string): string => fs.readFileSync(path.join(stylesDir, file), 'utf8');
const files = fs.readdirSync(stylesDir).filter(file => file.endsWith('.css'));

/** Body of the first rule whose selector list matches `selector` exactly. */
function ruleBody(css: string, selector: RegExp): string {
  const match = new RegExp(`(?:^|\\})\\s*${selector.source}\\s*\\{([^}]*)\\}`, 'm').exec(css);
  if (!match) throw new Error(`no rule for ${selector.source}`);
  return match[1]!;
}

describe('Typography foundation (issue #741)', () => {
  const base = read('base.css');

  test('body sets the normal line-height token', () => {
    expect(ruleBody(base, /body/)).toMatch(/line-height:\s*var\(--cudly-lh-normal\)/);
  });

  test('form controls inherit the page font at the button size', () => {
    const body = ruleBody(base, /button,\s*input,\s*select,\s*textarea/);
    expect(body).toMatch(/font:\s*inherit/);
    expect(body).toMatch(/font-size:\s*var\(--cudly-fs-sm\)/);
  });

  test.each([
    ['h1', 'fs-2xl'],
    ['h2', 'fs-xl'],
    ['h3', 'fs-lg'],
    ['h4', 'fs-md'],
    ['h5, h6', 'fs-sm'],
  ])('%s has a zero-specificity base size on --cudly-%s', (tags, token) => {
    const selector = new RegExp(`:where\\(${tags.replace(/ /g, '\\s*')}\\)`);
    expect(ruleBody(base, selector)).toContain(`var(--cudly-${token})`);
  });

  test.each(files)('%s has no literal font size or weight that duplicates a token', file => {
    const css = read(file);
    expect(css).not.toMatch(/font-size:\s*(0\.75|0\.875|1|1\.125|1\.375|1\.75|2\.25)rem\s*;/);
    expect(css).not.toMatch(/font-weight:\s*(bold|400|500|600|700)\s*;/);
  });
});
