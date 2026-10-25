/**
 * First-run empty state (issue #756).
 *
 * Shown on Opportunities and Home when no recommendations have been loaded at
 * all, which on a fresh install means no cloud account is connected or no
 * collection has produced anything yet. The older copy ("No rows match these
 * filters.") told new users to adjust filters they never set; this card says
 * what is going on and offers the next step: connect an account (admins), ask
 * an admin (everyone else), or refresh recommendations.
 *
 * Navigation to Admin > Accounts is injected by app.ts through
 * setOpenAccountsHandler so this module does not import navigation.ts, which
 * already imports the modules that render this card (an import cycle).
 */

import type { RecommendationsFreshness } from './api/recommendations';
import { formatDateTime } from './utils';
import { isAdmin } from './permissions';

export interface FirstRunEmptyStateOptions {
  /** True/false when the account list loaded; null when it could not be read. */
  hasAccounts: boolean | null;
  /** True when the topbar provider/account filter narrows the scope. */
  scopeFiltered: boolean;
  /** Starts a recommendations refresh. When omitted the button is not shown. */
  onRefresh?: () => Promise<void>;
  /** Loads collection freshness for the status line. When omitted the line is not shown. */
  loadFreshness?: () => Promise<RecommendationsFreshness>;
}

let openAccountsHandler: (() => void) | null = null;

/** Registers how to open Admin > Accounts. Called once from app.ts init. */
export function setOpenAccountsHandler(handler: (() => void) | null): void {
  openAccountsHandler = handler;
}

/**
 * True when the topbar filter narrows the scope: a specific provider or at
 * least one account. Mirrors dashboard.ts buildFilterDesc's notion of "all".
 */
export function isScopeFiltered(provider: string, accountIDs: readonly string[]): boolean {
  return (provider !== '' && provider.toLowerCase() !== 'all') || accountIDs.length > 0;
}

/** One-line description of the last recommendations collection. */
export function describeFreshness(f: RecommendationsFreshness): string {
  if (f.last_collection_started_at) {
    return 'A recommendations collection is running now.';
  }
  const parts: string[] = [];
  const when = formatDateTime(f.last_collected_at);
  parts.push(when ? `Last collected ${when}.` : 'Recommendations have not been collected yet.');
  if (f.last_collection_error) {
    parts.push(`The last collection reported errors: ${f.last_collection_error}`);
  }
  return parts.join(' ');
}

interface Copy {
  heading: string;
  body: string;
  /** Show the Refresh button (when onRefresh is provided). */
  refresh: boolean;
}

function pickCopy(opts: FirstRunEmptyStateOptions): Copy {
  if (opts.scopeFiltered) {
    return {
      heading: 'No opportunities for the selected provider or account',
      body: 'Nothing has been collected for this scope. Choose All Providers and All Accounts in the filters at the top of the page to see every opportunity.',
      refresh: true,
    };
  }
  if (opts.hasAccounts === false) {
    return {
      heading: 'No cloud accounts connected yet',
      body: 'CUDly finds commitment savings by analyzing usage in your cloud accounts. Connect an AWS, Azure or GCP account to start collecting recommendations.',
      refresh: false,
    };
  }
  return {
    heading: 'No opportunities yet',
    body: 'CUDly has not found any commitment opportunities in your connected accounts. New recommendations appear after each collection.',
    refresh: true,
  };
}

function makeButton(label: string, className: string, onClick: () => void): HTMLButtonElement {
  const btn = document.createElement('button');
  btn.type = 'button';
  btn.className = className;
  btn.textContent = label;
  btn.addEventListener('click', onClick);
  return btn;
}

/**
 * Builds the empty-state card. All text goes through textContent, so a
 * collection error message from the API cannot inject markup.
 */
export function buildFirstRunEmptyState(opts: FirstRunEmptyStateOptions): HTMLElement {
  const copy = pickCopy(opts);
  const card = document.createElement('div');
  card.className = 'empty-state first-run-empty-state';
  card.setAttribute('role', 'status');

  const h3 = document.createElement('h3');
  h3.textContent = copy.heading;
  card.appendChild(h3);

  const body = document.createElement('p');
  body.textContent = copy.body;
  card.appendChild(body);

  if (opts.loadFreshness) {
    const status = document.createElement('p');
    status.className = 'first-run-freshness';
    status.hidden = true;
    card.appendChild(status);
    opts.loadFreshness().then((f) => {
      status.textContent = describeFreshness(f);
      status.hidden = false;
    }).catch(() => {
      // Freshness is a nice-to-have; the card is complete without it.
    });
  }

  const actions = document.createElement('div');
  actions.className = 'empty-state-actions';
  const admin = isAdmin();
  const needsAccount = opts.hasAccounts === false && !opts.scopeFiltered;

  if (copy.refresh && opts.onRefresh) {
    const onRefresh = opts.onRefresh;
    const refreshBtn = makeButton('Refresh recommendations', 'primary', () => {
      refreshBtn.disabled = true;
      onRefresh().finally(() => { refreshBtn.disabled = false; });
    });
    actions.appendChild(refreshBtn);
  }

  if (admin && openAccountsHandler) {
    const go = openAccountsHandler;
    const label = needsAccount ? 'Connect a cloud account' : 'Manage cloud accounts';
    const cls = needsAccount ? 'primary' : 'secondary';
    actions.appendChild(makeButton(label, cls, () => go()));
  } else if (needsAccount) {
    const hint = document.createElement('p');
    hint.className = 'first-run-admin-hint';
    hint.textContent = 'Ask an administrator to connect one under Admin > Accounts.';
    card.appendChild(hint);
  }

  if (actions.childElementCount > 0) card.appendChild(actions);
  return card;
}
