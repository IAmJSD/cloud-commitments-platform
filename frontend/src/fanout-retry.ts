/**
 * Partial bulk-purchase failure recovery (issue #757).
 *
 * A multi-bucket ("fan-out") purchase is one POST per bucket. When some of
 * them fail, the modal used to close and clear the selection, leaving the
 * user to find and reselect the failed rows in a large table, with a risk of
 * resubmitting the buckets that already went through. Instead the modal now
 * stays open: every bucket section shows whether it was submitted or failed
 * (with the reason), the submitted ones are locked, and the submit button
 * becomes "Retry N failed", which resubmits only the failed buckets.
 */

import { getFanOutBuckets, retainFanOutBuckets, type FanOutBucket } from './recommendations';

export interface FanOutBucketOutcome {
  bucket: FanOutBucket;
  ok: boolean;
  /** Short status for the bucket section, e.g. the failure reason. */
  detail: string;
}

const RESULT_ID = 'fanout-result';

function sectionFor(container: HTMLElement, id: string): HTMLElement | null {
  for (const el of container.querySelectorAll<HTMLElement>('section.fanout-bucket')) {
    if (el.dataset['bucketId'] === id) return el;
  }
  return null;
}

function markSection(section: HTMLElement, outcome: FanOutBucketOutcome): void {
  section.querySelector('.fanout-bucket-result')?.remove();
  section.classList.toggle('fanout-bucket-submitted', outcome.ok);
  section.classList.toggle('fanout-bucket-failed', !outcome.ok);
  const line = document.createElement('p');
  line.className = `fanout-bucket-result ${outcome.ok ? 'fanout-bucket-result-ok' : 'fanout-bucket-result-failed'}`;
  line.textContent = outcome.ok ? `Submitted: ${outcome.detail}` : `Failed: ${outcome.detail}`;
  section.querySelector('h4')?.after(line);
  if (outcome.ok) {
    // Submitted buckets are final: lock their controls so nothing suggests
    // they will be sent again.
    section.querySelectorAll<HTMLSelectElement>('select').forEach((s) => { s.disabled = true; });
  }
}

function purchaseModalOpen(): boolean {
  const modal = document.getElementById('purchase-modal');
  return modal !== null && !modal.classList.contains('hidden');
}

/**
 * Whether the purchase modal is still open on the buckets that were just
 * submitted. Bucket ids are unique per modal open, so this is false once the
 * user closed the modal (Escape or Cancel) while the POSTs were in flight,
 * and also once a new purchase modal has replaced it.
 */
function modalHoldsBuckets(buckets: readonly FanOutBucket[]): boolean {
  if (!purchaseModalOpen()) return false;
  const current = getFanOutBuckets();
  if (!current) return false;
  const ids = new Set(current.map((b) => b.id));
  return buckets.every((b) => b.id !== undefined && ids.has(b.id));
}

/**
 * True when a different purchase modal is open than the one these buckets
 * were submitted from: the user closed it and opened another before the
 * POSTs settled. The caller must then leave the modal alone instead of
 * closing it or clearing its state.
 */
export function purchaseModalReplaced(buckets: readonly FanOutBucket[]): boolean {
  if (!buckets.every((b) => b.id)) return false;
  return purchaseModalOpen() && !modalHoldsBuckets(buckets);
}

/**
 * Keeps the fan-out modal open after a partial failure and narrows it to the
 * failed buckets. Returns false (caller reports by toast and closes the modal
 * as before) when nothing failed, when the buckets carry no ids, or when the
 * modal that submitted them is no longer open.
 */
export function showFanOutRetry(outcomes: readonly FanOutBucketOutcome[]): boolean {
  const failed = outcomes.filter((o) => !o.ok);
  if (failed.length === 0) return false;
  if (!modalHoldsBuckets(outcomes.map((o) => o.bucket))) return false;
  const container = document.getElementById('purchase-details');
  if (!container || !container.querySelector('section.fanout-bucket')) return false;

  for (const o of outcomes) {
    const section = sectionFor(container, o.bucket.id!);
    if (section) markSection(section, o);
  }

  retainFanOutBuckets(new Set(failed.map((o) => o.bucket.id!)));

  let banner = document.getElementById(RESULT_ID);
  if (!banner) {
    banner = document.createElement('div');
    banner.id = RESULT_ID;
    banner.className = 'warning-message fanout-result';
    banner.setAttribute('role', 'alert');
    const summary = document.getElementById('fanout-summary');
    if (summary) summary.after(banner);
    else container.prepend(banner);
  }
  // Count submitted sections across attempts, so a second partial failure
  // still reports everything that already went through.
  const submitted = container.querySelectorAll('section.fanout-bucket-submitted').length;
  const head = `${submitted} bucket${submitted === 1 ? '' : 's'} submitted, ${failed.length} failed.`;
  banner.textContent = submitted > 0
    ? `${head} Submitted buckets will not be sent again, and closing this dialog does not cancel them. Retry sends only the failed bucket${failed.length === 1 ? '' : 's'}.`
    : `${head} Nothing was submitted. Check the reasons below, then retry.`;
  // jsdom has no scrollIntoView; browsers always do.
  if (typeof banner.scrollIntoView === 'function') banner.scrollIntoView({ block: 'nearest' });
  return true;
}

/**
 * Submit-button label while a retry is pending, or null when the modal is
 * not in retry mode. The banner only exists in retry mode: opening the modal
 * again rebuilds #purchase-details and drops it.
 */
export function fanOutRetryLabel(): string | null {
  if (!document.getElementById(RESULT_ID)) return null;
  const n = getFanOutBuckets()?.length ?? 0;
  return n > 0 ? `Retry ${n} failed` : null;
}
