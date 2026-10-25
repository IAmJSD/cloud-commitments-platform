/**
 * Loading and failure states for the Admin > Users sections (issue #182).
 *
 * loadUsers() fills three containers from one listUsers + listGroups
 * round-trip. Before this module, the containers stayed empty while that
 * request was in flight, and stayed empty for good when it failed: the
 * only signal was a toast that disappears after a few seconds. An admin
 * was left looking at three boxes holding nothing but a Create button,
 * with no way to tell "no users" from "the list never loaded".
 *
 * Now each section shows a skeleton while loading and, on failure, an
 * inline error with a Retry button. Content that already rendered on an
 * earlier successful load is never replaced: a failed refresh after a
 * delete keeps the last good list on screen (the toast still reports it).
 */

import { showSkeletonBlock, showSkeletonRows, teardownSkeleton } from '../lib/skeleton';

interface SectionSpec {
  id: string;
  failure: string;
}

const SECTIONS: readonly SectionSpec[] = [
  { id: 'users-list', failure: 'Could not load users' },
  { id: 'groups-list', failure: 'Could not load groups' },
  { id: 'permission-matrix', failure: 'Could not load the permission overview' },
];

/** Columns of the users table (see renderUsers), so the skeleton matches it. */
const USERS_TABLE_COLS = 8;

/** True when the container holds nothing but a skeleton or nothing at all. */
function holdsNoContent(el: HTMLElement): boolean {
  return el.childElementCount === 0 || el.dataset['skeletonActive'] === '1'
    || el.dataset['loadError'] === '1';
}

/**
 * Show a skeleton in every section that has nothing rendered yet. Sections
 * already showing a list keep it until the fresh data replaces it, so a
 * refresh after an edit does not flash.
 */
export function showUserSectionsLoading(): void {
  for (const { id } of SECTIONS) {
    const el = document.getElementById(id);
    if (!el || !holdsNoContent(el)) continue;
    delete el.dataset['loadError'];
    if (id === 'users-list') {
      showSkeletonRows(el, 3, USERS_TABLE_COLS);
    } else {
      showSkeletonBlock(el, '100%', '4rem');
    }
  }
}

/**
 * Replace the skeleton (or empty box) in each section with an inline error
 * and a Retry button. Sections that still show an earlier successful render
 * are left alone.
 */
export function renderUserSectionsError(detail: string, onRetry: () => void): void {
  for (const { id, failure } of SECTIONS) {
    const el = document.getElementById(id);
    if (!el || !holdsNoContent(el)) continue;
    teardownSkeleton(el);
    el.textContent = '';
    el.dataset['loadError'] = '1';

    const msg = document.createElement('p');
    msg.className = 'error';
    // No role="alert" here: loadUsers' toast is already the live region
    // for this failure, and three more alerts would repeat it four times.
    msg.textContent = detail ? `${failure}: ${detail}` : `${failure}.`;

    const retry = document.createElement('button');
    retry.type = 'button';
    retry.className = 'btn btn-secondary btn-small';
    retry.dataset['action'] = 'retry-load-users';
    retry.textContent = 'Retry';
    retry.addEventListener('click', () => {
      // The button is about to be replaced by a skeleton; keep focus in
      // the section instead of dropping it to <body>.
      el.tabIndex = -1;
      el.focus();
      onRetry();
    });

    el.append(msg, retry);
  }
}

/**
 * Drop skeletons and error notes once the data has arrived, right before
 * the renderers fill the sections. Clearing the markers here (rather than
 * relying on the renderers' innerHTML swap) keeps a later failed refresh
 * from mistaking a rendered list for a placeholder and wiping it.
 */
export function clearUserSectionsStatus(): void {
  for (const { id } of SECTIONS) {
    const el = document.getElementById(id);
    if (!el) continue;
    teardownSkeleton(el);
    if (el.dataset['loadError'] === '1') {
      el.textContent = '';
      delete el.dataset['loadError'];
    }
  }
}
