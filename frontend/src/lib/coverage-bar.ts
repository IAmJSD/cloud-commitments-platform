/**
 * Coverage bar for Inventory & Coverage -> Coverage (issue #726).
 *
 * The bar used to be success-green at every value, so a service at 5%
 * coverage looked as healthy as one at 95%. It now has a neutral fill, a tick
 * at the service's coverage target, and turns amber or red when coverage is
 * far below that target.
 *
 * The target is the coverage % purchases aim for: the per-service setting
 * when one exists, else the global default (Admin -> Purchasing). Both come
 * from GET /api/config. When the config cannot be read there is no target,
 * so the bar stays neutral with no tick rather than guessing one.
 */
import type { ConfigResponse } from '../types';

export type CoverageTone = 'on-track' | 'below' | 'far-below';

// Coverage at or above this share of the target reads as on track.
export const COVERAGE_ON_TRACK_RATIO = 0.75;
// Below this share of the target the bar turns red; in between it is amber.
// With an 80% target: >= 60% neutral, 32-60% amber, < 32% red.
export const COVERAGE_FAR_BELOW_RATIO = 0.4;

export interface CoverageTargets {
  /** Per-service targets keyed by "provider:service" (lowercase). */
  byService: ReadonlyMap<string, number>;
  /** Global default target, or null when unknown. */
  fallback: number | null;
}

export const NO_COVERAGE_TARGETS: CoverageTargets = { byService: new Map(), fallback: null };

function validTarget(value: unknown): number | null {
  return typeof value === 'number' && Number.isFinite(value) && value > 0 && value <= 100 ? value : null;
}

function serviceKey(provider: string, service: string): string {
  return `${provider.toLowerCase()}:${service.toLowerCase()}`;
}

/** Collect the coverage targets from a /api/config response (null when it could not be read). */
export function coverageTargetsFromConfig(cfg: ConfigResponse | null | undefined): CoverageTargets {
  if (!cfg) return NO_COVERAGE_TARGETS;
  const byService = new Map<string, number>();
  // The API sends an array; tolerate anything else rather than throw.
  for (const svc of Array.isArray(cfg.services) ? cfg.services : []) {
    const target = validTarget(svc?.coverage);
    if (target !== null && svc.provider && svc.service) byService.set(serviceKey(svc.provider, svc.service), target);
  }
  return { byService, fallback: validTarget(cfg.global?.default_coverage) };
}

/** The coverage target for one service row, or null when none is known. */
export function coverageTargetFor(targets: CoverageTargets, provider: string, service: string): number | null {
  return targets.byService.get(serviceKey(provider, service)) ?? targets.fallback;
}

/** How far a coverage value sits below its target. No target means on track. */
export function coverageTone(pct: number, target: number | null): CoverageTone {
  if (target === null) return 'on-track';
  const ratio = pct / target;
  if (ratio >= COVERAGE_ON_TRACK_RATIO) return 'on-track';
  if (ratio >= COVERAGE_FAR_BELOW_RATIO) return 'below';
  return 'far-below';
}

const clampPct = (value: number): number => Math.min(100, Math.max(0, value));

/** Text for the bar's accessible name and tooltip. */
export function coverageBarLabel(pct: number, target: number | null): string {
  const value = `${pct.toFixed(1)}% covered`;
  if (target === null) return value;
  const tone = coverageTone(pct, target);
  const suffix = tone === 'far-below' ? ', far below target' : tone === 'below' ? ', below target' : '';
  return `${value}, target ${target.toFixed(0)}%${suffix}`;
}

/** Build the `div.coverage-bar` track with its fill and, when a target is known, a target tick. */
export function buildCoverageBar(pct: number, target: number | null): HTMLDivElement {
  const bar = document.createElement('div');
  bar.className = `coverage-bar coverage-bar--${coverageTone(pct, target)}`;
  // The bar carries no text, so screen readers need the value spelled out.
  bar.setAttribute('role', 'img');
  const label = coverageBarLabel(pct, target);
  bar.setAttribute('aria-label', label);
  bar.title = label;

  const fill = document.createElement('div');
  fill.className = 'coverage-bar-fill';
  // Clamp to [0, 100] so a misconfigured value can't overflow.
  fill.style.width = `${clampPct(pct)}%`;
  bar.appendChild(fill);

  if (target !== null) {
    const tick = document.createElement('div');
    tick.className = 'coverage-bar-target';
    tick.style.left = `${clampPct(target)}%`;
    bar.appendChild(tick);
  }
  return bar;
}
