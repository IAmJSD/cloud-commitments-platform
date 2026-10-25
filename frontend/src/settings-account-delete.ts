/**
 * Account delete flow for the Settings > Accounts table (issues #606, #734).
 *
 * Split out of settings.ts so the confirmation copy and the pending-purchase
 * cascade live in one small module. The first confirm names the account
 * (name + external ID) like the user, group and API key deletes do; the
 * 409 "pending_executions" path lists the purchases that "Cancel All N
 * Pending + Delete" would cancel, with their scheduled date and upfront
 * cost, so the operator sees the cascade before confirming it. Every
 * successful delete ends with a toast that names the account.
 */

import * as api from './api';
import { confirmDialog } from './confirmDialog';
import { showToast } from './toast';
import { formatCurrency, formatDate } from './utils';

type AccountProvider = 'aws' | 'azure' | 'gcp';
type DeleteApiError = Error & { status?: number; details?: Record<string, unknown> };

/** The fields of a CloudAccount the delete flow needs to name its target. */
export interface DeletableAccount {
  id: string;
  name: string;
  external_id: string;
}

/** Reloads the accounts table for one provider after a delete attempt. */
export type ReloadAccounts = (provider: AccountProvider) => Promise<void>;

// Per-purchase lookups are one GET each; cap them so an account with a
// long backlog does not fire dozens of requests just to draw a dialog.
// Anything past the cap is summarized as "and N more".
const MAX_LISTED_PURCHASES = 10;

/**
 * Re-entrancy guard for the delete + pending-cancel flow. A double-click on
 * Delete (or a click during the cancel-then-retry sequence) used to be able
 * to start two concurrent deletes for the same account, racing on the
 * backend preflight + cancel + delete sequence. Keyed on account ID so
 * different accounts can still be deleted in parallel.
 */
const inflightDeletes = new Set<string>();

function plural(n: number, word: string): string {
  return `${n} ${word}${n === 1 ? '' : 's'}`;
}

/** "Prod" (111111111111): the label every message in this flow uses. */
export function accountDisplayName(account: DeletableAccount): string {
  return account.external_id ? `"${account.name}" (${account.external_id})` : `"${account.name}"`;
}

/**
 * Delete an account after confirmation. On a 409 with reason
 * "pending_executions" the operator gets a second confirm listing the
 * pending purchases; confirming cancels each one and retries the delete.
 */
export async function deleteAccount(
  account: DeletableAccount,
  provider: AccountProvider,
  reload: ReloadAccounts,
): Promise<void> {
  if (inflightDeletes.has(account.id)) {
    showToast({ message: 'Delete already in progress for this account.', kind: 'info' });
    return;
  }
  const label = accountDisplayName(account);
  const ok = await confirmDialog({
    title: `Delete account ${label}?`,
    body: `Delete account ${label}? This also removes its credentials and service overrides. This action cannot be undone.`,
    confirmLabel: 'Delete account',
    destructive: true,
  });
  if (!ok) return;
  inflightDeletes.add(account.id);
  try {
    await api.deleteAccount(account.id);
    showToast({ message: `Deleted account ${label}.`, kind: 'success' });
    await reload(provider);
  } catch (err) {
    const apiErr = err as DeleteApiError;
    if (apiErr.status === 409 && apiErr.details?.['reason'] === 'pending_executions') {
      await handlePendingExecutions409(account, provider, apiErr, reload);
      return;
    }
    console.error('Failed to delete account:', err);
    showToast({ message: `Failed to delete account ${label}: ${apiErr.message}`, kind: 'error' });
    void reload(provider);
  } finally {
    inflightDeletes.delete(account.id);
  }
}

/** One line per pending purchase: plan + step, scheduled date, upfront cost. */
function describePurchase(id: string, details: api.PurchaseDetails | null): string {
  if (!details) return `Purchase ${id} (details unavailable)`;
  const name = details.plan_name
    ? `${details.plan_name}${details.step_number ? `, step ${details.step_number}` : ''}`
    : 'Manual purchase';
  const parts = [name];
  const recs = details.recommendations?.length ?? 0;
  if (recs > 0) parts.push(plural(recs, 'commitment'));
  const date = formatDate(details.scheduled_date);
  if (date) parts.push(`scheduled ${date}`);
  parts.push(`${formatCurrency(details.total_upfront_cost)} upfront`);
  return parts.join(' · ');
}

/**
 * Body of the "Cancel All N Pending + Delete" confirm: an intro naming the
 * account, a list of the purchases that will be cancelled (fetched per ID;
 * a failed lookup still lists the ID), and the summed upfront cost when
 * every listed purchase loaded.
 */
async function buildPendingPurchasesBody(
  label: string,
  pendingCount: number,
  pendingIDs: string[],
): Promise<HTMLElement> {
  const listed = pendingIDs.slice(0, MAX_LISTED_PURCHASES);
  const results = await Promise.allSettled(listed.map(id => api.getPurchaseDetails(id)));
  const details = results.map(r => (r.status === 'fulfilled' && r.value ? r.value : null));

  const wrap = document.createElement('div');
  wrap.className = 'account-delete-pending';
  const intro = document.createElement('p');
  intro.textContent = `Account ${label} has ${plural(pendingCount, 'pending purchase')}. `
    + 'They must be cancelled before the account can be deleted. These purchases will be cancelled:';
  wrap.appendChild(intro);

  const ul = document.createElement('ul');
  ul.className = 'account-delete-pending-list';
  listed.forEach((id, i) => {
    const li = document.createElement('li');
    li.textContent = describePurchase(id, details[i] ?? null);
    ul.appendChild(li);
  });
  const remaining = pendingIDs.length - listed.length;
  if (remaining > 0) {
    const li = document.createElement('li');
    li.textContent = `and ${remaining} more (see the History page)`;
    ul.appendChild(li);
  }
  wrap.appendChild(ul);

  if (remaining === 0 && details.every(d => d !== null)) {
    const total = details.reduce((sum, d) => sum + (d?.total_upfront_cost ?? 0), 0);
    const totalEl = document.createElement('p');
    totalEl.className = 'account-delete-pending-total';
    totalEl.textContent = `Total upfront cost cancelled: ${formatCurrency(total)}`;
    wrap.appendChild(totalEl);
  }
  return wrap;
}

/**
 * 409 handler: a second confirm listing the pending purchases and, on
 * confirm, cancels each one before retrying the delete. On any cancel
 * failure it stops short of the delete and says how many failed, so the
 * operator can investigate instead of partially cancelling and moving on.
 */
async function handlePendingExecutions409(
  account: DeletableAccount,
  provider: AccountProvider,
  apiErr: DeleteApiError,
  reload: ReloadAccounts,
): Promise<void> {
  const label = accountDisplayName(account);
  const rawCount = apiErr.details?.['pending_count'];
  const rawIDs = apiErr.details?.['pending_execution_ids'];
  const pendingIDs = Array.isArray(rawIDs) ? (rawIDs as unknown[]).filter((x): x is string => typeof x === 'string') : [];
  const pendingCount = Math.max(typeof rawCount === 'number' ? rawCount : Number(rawCount) || 0, pendingIDs.length);

  // No IDs to cancel (the server's list call errored). Skip the "Cancel
  // All N Pending" confirm since we can't enumerate cancellations, and
  // point the operator at the History page instead.
  if (pendingIDs.length === 0) {
    showToast({
      message: `Cannot auto-cancel: pending purchase IDs unavailable. Cancel them from the History page and try again.`,
      kind: 'error',
      timeout: null,
    });
    return;
  }

  const proceed = await confirmDialog({
    title: `Cancel pending purchases and delete ${label}?`,
    body: await buildPendingPurchasesBody(label, pendingCount, pendingIDs),
    confirmLabel: `Cancel All ${pendingIDs.length} Pending + Delete`,
    destructive: true,
  });
  if (!proceed) {
    // Operator backed out of the second dialog; the account is unchanged.
    void reload(provider);
    return;
  }

  const failures: { id: string; err: Error }[] = [];
  for (const execID of pendingIDs) {
    try {
      await api.cancelPurchase(execID);
    } catch (cancelErr) {
      failures.push({ id: execID, err: cancelErr as Error });
    }
  }

  if (failures.length > 0) {
    console.error('Failed to cancel some pending executions:', failures);
    showToast({
      message: `Cancelled ${pendingIDs.length - failures.length} of ${pendingIDs.length} purchases; ${failures.length} failed. Account ${label} NOT deleted — review History and retry.`,
      kind: 'error',
      timeout: null,
    });
    void reload(provider);
    return;
  }

  try {
    await api.deleteAccount(account.id);
    showToast({
      message: `Cancelled ${plural(pendingIDs.length, 'pending purchase')} and deleted account ${label}.`,
      kind: 'success',
    });
  } catch (retryErr) {
    console.error('Failed to delete account after cancelling pending purchases:', retryErr);
    showToast({
      message: `Cancelled ${plural(pendingIDs.length, 'purchase')} but deleting account ${label} still failed: ${(retryErr as Error).message}`,
      kind: 'error',
      timeout: null,
    });
  } finally {
    void reload(provider);
  }
}
