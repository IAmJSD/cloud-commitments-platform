/**
 * Issue #726: coverage bar tone and target helpers.
 */
import {
  buildCoverageBar, coverageBarLabel, coverageTargetFor, coverageTargetsFromConfig, coverageTone,
  NO_COVERAGE_TARGETS,
} from '../lib/coverage-bar';
import type { ConfigResponse } from '../types';

describe('coverageTone', () => {
  test.each([
    [95, 80, 'on-track'],
    [80, 80, 'on-track'],
    [60, 80, 'on-track'], // exactly 0.75 of target
    [59.9, 80, 'below'],
    [32, 80, 'below'], // exactly 0.4 of target
    [31.9, 80, 'far-below'],
    [5, 80, 'far-below'],
    [0, 80, 'far-below'],
  ])('%d%% against a %d%% target is %s', (pct, target, tone) => {
    expect(coverageTone(pct, target)).toBe(tone);
  });

  test('with no known target nothing is flagged', () => {
    expect(coverageTone(0, null)).toBe('on-track');
  });
});

describe('coverageTargetsFromConfig', () => {
  const cfg = {
    global: { default_coverage: 80 },
    services: [
      { provider: 'aws', service: 'RDS', coverage: 50 },
      { provider: 'aws', service: 'ec2', coverage: 0 }, // invalid: falls back
      { provider: 'gcp', service: 'compute', coverage: 150 }, // invalid: falls back
    ],
  } as unknown as ConfigResponse;

  test('prefers the per-service target, case-insensitively', () => {
    const t = coverageTargetsFromConfig(cfg);
    expect(coverageTargetFor(t, 'AWS', 'rds')).toBe(50);
  });

  test('ignores out-of-range service targets and uses the global default', () => {
    const t = coverageTargetsFromConfig(cfg);
    expect(coverageTargetFor(t, 'aws', 'ec2')).toBe(80);
    expect(coverageTargetFor(t, 'gcp', 'compute')).toBe(80);
    expect(coverageTargetFor(t, 'azure', 'vm')).toBe(80);
  });

  test('no config, a services object instead of an array, or no default -> no target', () => {
    expect(coverageTargetsFromConfig(null)).toBe(NO_COVERAGE_TARGETS);
    const odd = coverageTargetsFromConfig({ global: {}, services: {} } as unknown as ConfigResponse);
    expect(coverageTargetFor(odd, 'aws', 'ec2')).toBeNull();
  });
});

describe('buildCoverageBar', () => {
  test('clamps the fill, places the target tick and names both', () => {
    const bar = buildCoverageBar(130, 80);
    expect(bar.className).toBe('coverage-bar coverage-bar--on-track');
    expect(bar.querySelector<HTMLElement>('.coverage-bar-fill')!.style.width).toBe('100%');
    expect(bar.querySelector<HTMLElement>('.coverage-bar-target')!.style.left).toBe('80%');
    expect(bar.getAttribute('role')).toBe('img');
    expect(bar.getAttribute('aria-label')).toBe('130.0% covered, target 80%');
    expect(bar.title).toBe(bar.getAttribute('aria-label'));
  });

  test('below target gets the amber class and says so', () => {
    const bar = buildCoverageBar(40, 80);
    expect(bar.className).toBe('coverage-bar coverage-bar--below');
    expect(coverageBarLabel(40, 80)).toBe('40.0% covered, target 80%, below target');
  });

  test('without a target there is no tick and the label is just the value', () => {
    const bar = buildCoverageBar(12.5, null);
    expect(bar.querySelector('.coverage-bar-target')).toBeNull();
    expect(bar.getAttribute('aria-label')).toBe('12.5% covered');
  });
});
