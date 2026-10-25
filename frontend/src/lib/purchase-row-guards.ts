/**
 * Guards against rendering impossible values on scheduled and pending
 * purchase rows (issue #181): a "0 Years" term, a zero count, "0
 * commitment(s)" and all-$0 costs for an execution that carries no
 * recommendations, and an overdue scheduled purchase that still reads as a
 * plain "pending".
 */

/** Visible label for a row with nothing to buy. Matches the Plans copy (#609). */
export const NO_RECOMMENDATIONS_LABEL = 'No recommendations attached';
export const NO_RECOMMENDATIONS_TITLE =
  'This purchase has no recommendations attached, so it cannot buy anything.';

/**
 * True for a purchase-history / approval-queue row that carries no
 * commitments. The API sets count to the number of recommendations on a
 * synthetic execution row (internal/api/handler_history.go), so 0 means the
 * row's type, term and costs are placeholders, not data.
 */
export function hasNoCommitments(row: { count: number }): boolean {
  return row.count === 0;
}

/** A term in years that can actually be bought (1, 3, ...). */
export function isRealTerm(term: number | null | undefined): term is number {
  return typeof term === 'number' && Number.isFinite(term) && term > 0;
}

/** Local calendar date as YYYY-MM-DD. */
function localISODate(d: Date): string {
  const mm = String(d.getMonth() + 1).padStart(2, '0');
  const dd = String(d.getDate()).padStart(2, '0');
  return `${d.getFullYear()}-${mm}-${dd}`;
}

/**
 * True when a YYYY-MM-DD (or ISO timestamp) scheduled date is strictly
 * before today in the user's calendar. Compares the date part as text so a
 * date-only string is not shifted a day by UTC parsing.
 */
export function isPastScheduledDate(scheduled: string | null | undefined, now: Date = new Date()): boolean {
  if (!scheduled) return false;
  const day = scheduled.slice(0, 10);
  if (!/^\d{4}-\d{2}-\d{2}$/.test(day)) return false;
  return day < localISODate(now);
}
