/**
 * One status vocabulary for every badge in the app (issue #737).
 *
 * Before this module each page picked its own colors: "pending" was blue in
 * RI exchange history and amber on Purchases, "running" was orange in one
 * table and blue in another, and RI exchange printed the raw lowercase
 * status. Every status badge now goes through statusTone() for its color and
 * statusLabel() for its text, and renders as `.badge .badge-<tone>`
 * (styles/components.css), so the same state reads the same everywhere:
 *
 *   success  finished well        completed, approved, active
 *   warning  not finished yet     pending, notified, scheduled, running,
 *                                 processing, in progress, partial
 *   danger   failed or refused    failed, rejected
 *   muted    inactive / final     canceled, expired, paused, disabled
 *   info     informational tags   not a status; "You", "Self", "creds", ...
 *
 * An unknown status is muted rather than green: guessing "success" would tell
 * the user something finished when we don't know that.
 */
import { escapeHtml } from '../utils';

export type BadgeTone = 'success' | 'warning' | 'danger' | 'info' | 'muted';

const STATUS_TONES: ReadonlyMap<string, BadgeTone> = new Map<string, BadgeTone>([
  ['completed', 'success'],
  ['approved', 'success'],
  ['active', 'success'],
  ['pending', 'warning'],
  ['notified', 'warning'],
  ['scheduled', 'warning'],
  ['running', 'warning'],
  ['processing', 'warning'],
  ['in_progress', 'warning'],
  ['partially_completed', 'warning'],
  ['failed', 'danger'],
  ['rejected', 'danger'],
  // Migration 000089 renames 'cancelled' to 'canceled'; rows may carry either
  // spelling until the contract migration (#1278) lands.
  ['canceled', 'muted'],
  ['cancelled', 'muted'],
  ['expired', 'muted'],
  ['paused', 'muted'],
  ['disabled', 'muted'],
]);

// Display text where title-casing the wire value is not enough. The canceled
// label keeps the History page's existing "Cancelled" copy so both spellings
// and every page show one word.
const STATUS_LABELS: ReadonlyMap<string, string> = new Map([
  ['in_progress', 'In Progress'],
  ['partially_completed', 'Partial'],
  ['canceled', 'Cancelled'],
  ['cancelled', 'Cancelled'],
]);

function normalize(status: string | null | undefined): string {
  return (status ?? '').trim().toLowerCase();
}

/** Color family for a status value from any API (purchases, plans, RI exchange, registrations). */
export function statusTone(status: string | null | undefined): BadgeTone {
  return STATUS_TONES.get(normalize(status)) ?? 'muted';
}

/** Human display text for a status: "in_progress" -> "In Progress", "pending" -> "Pending". */
export function statusLabel(status: string | null | undefined): string {
  const key = normalize(status);
  const mapped = STATUS_LABELS.get(key);
  if (mapped) return mapped;
  const words = key.replace(/_/g, ' ');
  return words ? words.charAt(0).toUpperCase() + words.slice(1) : 'Unknown';
}

/** Class list for a badge of the given tone, for DOM callers that set className. */
export function badgeClass(tone: BadgeTone): string {
  return `badge badge-${tone}`;
}

/** `<span class="badge badge-<tone>">label</span>`; the label is HTML-escaped. */
export function badgeHtml(tone: BadgeTone, label: string, title?: string): string {
  const titleAttr = title ? ` title="${escapeHtml(title)}"` : '';
  return `<span class="${badgeClass(tone)}"${titleAttr}>${escapeHtml(label)}</span>`;
}

/** Badge markup for a raw status value: tone from statusTone, text from statusLabel. */
export function statusBadgeHtml(status: string | null | undefined): string {
  return badgeHtml(statusTone(status), statusLabel(status));
}
