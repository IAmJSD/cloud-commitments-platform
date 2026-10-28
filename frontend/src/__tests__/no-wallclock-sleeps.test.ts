/**
 * Guard against wall-clock sleeps in the unit tests (issue #36).
 *
 * `await new Promise(r => setTimeout(r, 50))` guesses how long an async
 * handler needs. It wastes that time on every run and still flakes when a
 * loaded CI runner needs longer. Wait for the work itself instead:
 * flushPromises() from ./flush-promises when the handler awaits mocked
 * promises, jest fake timers when the code under test schedules a timer, or
 * waitFor() from @testing-library/dom for a condition. A zero-delay timer
 * (the flush itself) is allowed.
 */
import fs from 'fs';
import path from 'path';

const testsDir = __dirname;

// `new Promise(r => setTimeout(r, N))` and `new Promise(r => setTimeout(() => r(), N))`,
// with or without a type argument or parentheses around the parameter.
const SLEEP = /new Promise\s*(?:<[^>]*>)?\(\s*\(?\s*([A-Za-z_$][\w$]*)\s*\)?\s*=>\s*\{?\s*setTimeout\(\s*(?:\1|\(\)\s*=>\s*\1\(\))\s*,\s*([^)]+?)\s*\)/g;

function testSources(dir: string): string[] {
  return fs.readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) return testSources(full);
    return entry.name.endsWith('.ts') ? [full] : [];
  });
}

function findSleeps(source: string): { line: number; delay: string }[] {
  const found: { line: number; delay: string }[] = [];
  for (const match of source.matchAll(SLEEP)) {
    const delay = match[2] ?? '';
    if (/^0+$/.test(delay)) continue;
    found.push({ line: source.slice(0, match.index).split('\n').length, delay });
  }
  return found;
}

describe('no wall-clock sleeps in unit tests (issue #36)', () => {
  test('the matcher flags non-zero sleeps and allows the zero-delay flush', () => {
    expect(findSleeps('await new Promise(resolve => setTimeout(resolve, 50));')).toEqual([{ line: 1, delay: '50' }]);
    expect(findSleeps('await new Promise((r) => setTimeout(r, 10));')).toHaveLength(1);
    expect(findSleeps('const tick = (): Promise<void> => new Promise((r) => setTimeout(r, 10));')).toHaveLength(1);
    expect(findSleeps('await new Promise<void>((done) => setTimeout(() => done(), 2100));')).toHaveLength(1);
    expect(findSleeps('await new Promise(r => setTimeout(r, DELAY_MS));')).toEqual([{ line: 1, delay: 'DELAY_MS' }]);
    expect(findSleeps('\nawait new Promise(r => { setTimeout(r, 5); });')).toEqual([{ line: 2, delay: '5' }]);
    expect(findSleeps('await new Promise((resolve) => setTimeout(resolve, 0));')).toEqual([]);
    expect(findSleeps('setTimeout(() => closeModal(modal), 2000);')).toEqual([]);
  });

  test('no test file sleeps for a fixed time to wait for async work', () => {
    // This file is skipped: its matcher samples above are sleeps on purpose.
    const offenders = testSources(testsDir).filter((file) => file !== __filename).flatMap((file) =>
      findSleeps(fs.readFileSync(file, 'utf8'))
        .map(({ line, delay }) => `${path.relative(testsDir, file)}:${line} sleeps ${delay}ms`));
    expect(offenders).toEqual([]);
  });
});
