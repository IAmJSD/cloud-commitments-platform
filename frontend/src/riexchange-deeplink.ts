/**
 * Deep-link handler for /inventory/ri-exchange/approve/:id and
 * /inventory/ri-exchange/reject/:id (issue #579).
 *
 * The RI-exchange approval email used to link straight at
 * /api/ri-exchange/{approve,reject}/:id?token=…, which only routes POST, so a
 * click from the inbox (a GET) returned 401. The email should instead target
 * these SPA paths; this module turns the click into an explicit,
 * confirmed POST, mirroring purchases-deeplink.ts.
 *
 * The paths sit under the canonical /inventory/ri-exchange sub-tab, not the
 * legacy /ri-exchange prefix: applyTabFromPath() rewrites legacy prefixes to
 * /inventory before auth runs, which would drop the action and id (and, for a
 * signed-out user, lose them across the login reload). Under /inventory the
 * router leaves the URL alone and the page already opens on the right tab.
 *
 *
 *   1. init() sees the URL and calls handleRIExchangeDeeplink() after auth.
 *      An unauthenticated visitor gets the login modal first; login reloads
 *      the page with the same URL and the handler then runs.
 *   2. The user confirms in a dialog that spells out what the exchange
 *      does (from the history record, when the session can read it).
 *   3. We POST /api/ri-exchange/{approve,reject}/:id?token=…. The session
 *      headers ride along, so the backend can attribute an approval to the
 *      logged-in user.
 *   4. The URL is replaced with the RI Exchange sub-tab, so a back
 *      navigation never re-enters the one-shot deep link.
 *
 * Nothing here issues a side-effecting GET: landing on the link only reads.
 */

import { apiRequest } from './api/client';
import { getRIExchangeHistory } from './api';
import type { RIExchangeHistoryRecord } from './api/types';
import { confirmDialog } from './confirmDialog';
import { showToast } from './toast';

type RIExchangeDeeplinkAction = 'approve' | 'reject';

export interface ParsedRIExchangeDeeplink {
  action: RIExchangeDeeplinkAction;
  id: string;
  token: string;
}

/** Where the user lands once the deep link has been handled. */
export const RI_EXCHANGE_LANDING_PATH = '/inventory/ri-exchange';

/**
 * Parse a URL as an RI-exchange action deep link. Returns null unless the
 * path is exactly `/inventory/ri-exchange/{approve,reject}/:id`.
 */
export function parseRIExchangeDeeplink(pathname: string, search: string): ParsedRIExchangeDeeplink | null {
  const parts = pathname.split('/').filter(Boolean);
  if (parts.length !== 4 || parts[0] !== 'inventory' || parts[1] !== 'ri-exchange') return null;
  const [, , action, id] = parts;
  if ((action !== 'approve' && action !== 'reject') || !id) return null;
  const token = new URLSearchParams(search).get('token') || '';
  return { action, id, token };
}

/**
 * Best-effort lookup of the exchange so the dialog can say what is being
 * approved. The history read needs the RI-exchange view permission, which an
 * approver holding only the email token may lack; any failure falls back to
 * the generic wording rather than blocking the action the token authorizes.
 */
async function findExchangeRecord(id: string): Promise<RIExchangeHistoryRecord | null> {
  try {
    const records = await getRIExchangeHistory();
    return records.find(r => r.id === id) ?? null;
  } catch {
    return null;
  }
}

function describeExchange(rec: RIExchangeHistoryRecord): string {
  return `${rec.source_count}x ${rec.source_instance_type} -> ${rec.target_count}x ${rec.target_instance_type}`
    + ` in ${rec.region}, previously quoted payment USD ${rec.payment_due}.`;
}

function dialogBody(dl: ParsedRIExchangeDeeplink, rec: RIExchangeHistoryRecord | null): string {
  const what = rec ? `RI exchange ${describeExchange(rec)}` : `RI exchange ${dl.id}.`;
  if (dl.action === 'approve') {
    return `${what} Approving executes it immediately using a fresh quote, within the configured `
      + 'per-exchange and daily spending limits. This exchange cannot be reversed.';
  }
  return `${what} Rejecting cancels the pending exchange; no reservations are changed.`;
}

/**
 * Handle the current URL if it is an RI-exchange deep link. Returns true when
 * one was handled (the URL has then been replaced with the RI Exchange
 * sub-tab), false when there was nothing to do.
 */
export async function handleRIExchangeDeeplink(): Promise<boolean> {
  const dl = parseRIExchangeDeeplink(window.location.pathname, window.location.search);
  if (!dl) return false;
  const leave = (): true => {
    window.history.replaceState({}, '', RI_EXCHANGE_LANDING_PATH);
    return true;
  };

  if (!dl.token) {
    showToast({
      message: 'Missing approval token in link. Open it from the original email instead of a shared copy.',
      kind: 'error',
      timeout: null,
    });
    return leave();
  }

  const rec = await findExchangeRecord(dl.id);
  if (rec && rec.status.toLowerCase() !== 'pending') {
    showToast({
      message: `This RI exchange is no longer pending (status: ${rec.status}), so there is nothing to ${dl.action}.`,
      kind: 'info',
      timeout: null,
    });
    return leave();
  }

  const approve = dl.action === 'approve';
  const ok = await confirmDialog({
    title: approve ? 'Approve RI exchange?' : 'Reject RI exchange?',
    body: dialogBody(dl, rec),
    confirmLabel: approve ? 'Approve exchange' : 'Reject exchange',
    cancelLabel: 'Not now',
    destructive: true,
    // Approving moves money: keep Enter from executing by reflex.
    initialFocus: 'cancel',
  });
  if (!ok) return leave();

  // The backend reads the token from the query string only
  // (router.go approveRIExchangeHandler / rejectRIExchangeHandler).
  const endpoint = `/ri-exchange/${dl.action}/${encodeURIComponent(dl.id)}?token=${encodeURIComponent(dl.token)}`;
  try {
    await apiRequest<unknown>(endpoint, { method: 'POST' });
    showToast({
      message: approve ? 'RI exchange approved and executing.' : 'RI exchange rejected.',
      kind: 'success',
      timeout: 5_000,
    });
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err);
    showToast({ message: `Failed to ${dl.action} RI exchange: ${msg}`, kind: 'error', timeout: null });
  }
  return leave();
}
