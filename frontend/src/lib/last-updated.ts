/**
 * "Updated <when>" stamp for config rows that carry an updated_at timestamp
 * (issue #8, stage 1). The visible text uses the app's canonical
 * formatDateTime (browser-local, 24-hour clock); the exact UTC instant sits
 * in the title tooltip and the <time datetime> attribute, so compliance-minded
 * users can read it without guessing the timezone.
 */
import { formatDateTime } from '../utils';

/**
 * Go serializes an unset time.Time as 0001-01-01T00:00:00Z. No row this app
 * wrote can predate 2000, so anything earlier means "unknown" and gets no
 * stamp rather than a misleading "Updated Jan 1, 1".
 */
const EARLIEST_REAL_YEAR = 2000;

export function buildLastUpdatedStamp(updatedAt: string | null | undefined): HTMLTimeElement | null {
  if (!updatedAt) return null;
  const d = new Date(updatedAt);
  if (isNaN(d.getTime()) || d.getUTCFullYear() < EARLIEST_REAL_YEAR) return null;

  const iso = d.toISOString();
  const stamp = document.createElement('time');
  stamp.className = 'last-updated-stamp';
  stamp.dateTime = iso;
  stamp.title = `Last updated ${iso.slice(0, 10)} ${iso.slice(11, 16)} UTC`;
  stamp.textContent = `Updated ${formatDateTime(d)}`;
  return stamp;
}
