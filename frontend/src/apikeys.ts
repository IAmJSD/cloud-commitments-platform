/**
 * API Keys Management module for CUDly
 */

import * as api from './api';
import type { APIKeyInfo, CreateAPIKeyResponse } from './types';
import { loadApiKeysUsageStats } from './apikeys_usage';
import { escapeHtml, formatDateTime, formatRelativeTime } from './utils';
import { confirmDialog } from './confirmDialog';
import { showToast } from './toast';
import { iconSvg } from './icons';
import { openModal, closeModal } from './modal';
import { showSkeletonRows, teardownSkeleton } from './lib/skeleton';

// State for modal management
let currentApiKeys: APIKeyInfo[] = [];

/**
 * Cell content for a request count the backend cannot report, i.e. a key
 * already in use before migration 000094 introduced the counters. Static
 * markup with no interpolated data, matching the "Never" treatment the
 * Last Used and Expires columns already use for absent values.
 */
const NO_COUNT_DATA = '<span class="text-muted" title="Not recorded for this key">n/a</span>';

/**
 * Load and display API keys.
 *
 * Defensive parsing (issue #9): the previous implementation read
 * `response.keys` while the backend returns `{api_keys: [...]}`,
 * leaving `currentApiKeys` set to undefined and crashing the next
 * render with "Cannot read properties of undefined (reading 'length')".
 * Read the documented `api_keys` field, then fall back to a bare array
 * (some other deployments/proxies might unwrap) and finally to `[]` so
 * a contract drift can never crash the page.
 *
 * The usage-stats summary loads in parallel -- a failure in either path
 * only takes down its own region, never both. See loadApiKeysUsageStats
 * for the section header's lifecycle.
 */
export async function loadApiKeys(): Promise<void> {
  const listContainer = document.getElementById('apikeys-list');
  if (listContainer) {
    showSkeletonRows(listContainer, 3, 9);
  }
  // Kick the summary off in parallel -- it has its own container + error
  // path so we don't await it inside the list flow.
  void loadApiKeysUsageStats();

  try {
    const response = await api.getApiKeys();
    const list = (response as { api_keys?: APIKeyInfo[] } | undefined)?.api_keys
      ?? (Array.isArray(response) ? response as APIKeyInfo[] : undefined)
      ?? [];
    currentApiKeys = Array.isArray(list) ? list : [];
    renderApiKeysList();
  } catch (error) {
    console.error('Failed to load API keys:', error);
    if (listContainer) teardownSkeleton(listContainer);
    renderApiKeysListError(listContainer, 'Failed to load API keys');
    showError('Failed to load API keys');
  }
}

/**
 * Replace the API keys list region with an inline error message so the
 * shimmer skeleton doesn't sit beside a stale empty table after a
 * failed fetch. Uses textContent (no innerHTML) to stay XSS-safe and
 * matches the patterns used by other modules (e.g. dashboard.ts).
 */
function renderApiKeysListError(container: HTMLElement | null, message: string): void {
  if (!container) return;
  container.replaceChildren();
  const p = document.createElement('p');
  p.className = 'error';
  p.textContent = message;
  container.appendChild(p);
}

/**
 * Render API keys list
 */
export function renderApiKeysList(): void {
  const container = document.getElementById('apikeys-list');
  if (!container) return;

  if (!Array.isArray(currentApiKeys) || currentApiKeys.length === 0) {
    // DOM construction rather than template literal so the security hook
    // doesn't flag the innerHTML write — and all copy is static anyway.
    container.replaceChildren();
    delete container.dataset['skeletonActive'];
    const wrap = document.createElement('div');
    wrap.className = 'empty apikeys-empty';
    const h = document.createElement('h4');
    h.textContent = 'No API keys yet';
    const p = document.createElement('p');
    p.textContent = 'Create an API key to let automation tools (CI pipelines, scripts, integrations) call CUDly programmatically. Each key can be revoked or rotated at any time.';
    wrap.appendChild(h);
    wrap.appendChild(p);
    container.appendChild(wrap);
    return;
  }

  const table = `
    <table class="data-table">
      <thead>
        <tr>
          <th>Name</th>
          <th>Key Prefix</th>
          <th>Status</th>
          <th>Created</th>
          <th>Last Used</th>
          <th>Requests (window)</th>
          <th>Requests (total)</th>
          <th>Expires</th>
          <th>Actions</th>
        </tr>
      </thead>
      <tbody>
        ${currentApiKeys.map(key => {
          const isExpired = key.expires_at && new Date(key.expires_at) < new Date();
          const statusClass = !key.is_active ? 'badge-danger' : isExpired ? 'badge-warning' : 'badge-success';
          const statusText = !key.is_active ? 'Revoked' : isExpired ? 'Expired' : 'Active';
          const countWindow = formatRequestCount(key.request_count_window);
          const countTotal = formatRequestCount(key.request_count_total);

          return `
            <tr>
              <td><strong>${escapeHtml(key.name)}</strong></td>
              <td><code>${escapeHtml(key.key_prefix)}...</code></td>
              <td><span class="badge ${statusClass}">${statusText}</span></td>
              <td>${formatDateTime(key.created_at)}</td>
              <td>${key.last_used_at ? `<span title="${escapeHtml(new Date(key.last_used_at).toISOString())}">${escapeHtml(formatRelativeTime(key.last_used_at))}</span>` : '<span class="text-muted">Never</span>'}</td>
              <td class="apikeys-count-cell">${countWindow}</td>
              <td class="apikeys-count-cell">${countTotal}</td>
              <td>${key.expires_at ? formatDateTime(key.expires_at) : '<span class="text-muted">Never</span>'}</td>
              <td>
                <div class="row-actions">
                  ${key.is_active && !isExpired ? `<button type="button" class="btn-small btn-warning icon-btn revoke-key-btn" data-key-id="${escapeHtml(key.id)}" aria-label="Revoke API key" title="Revoke API key">${iconSvg('slash')}</button>` : ''}
                  <button type="button" class="btn-small btn-danger icon-btn delete-key-btn" data-key-id="${escapeHtml(key.id)}" aria-label="Delete API key" title="Delete API key">${iconSvg('trash-2')}</button>
                </div>
              </td>
            </tr>
          `;
        }).join('')}
      </tbody>
    </table>
  `;

  // Clear the skeleton marker before the render — the existing
  // innerHTML write (kept intact for diff minimality) replaces all
  // children, and we don't want a stale `data-skeleton-active`
  // attribute lingering on the live table.
  delete container.dataset['skeletonActive'];
  container.innerHTML = table;

  // Add event delegation after rendering
  container.querySelectorAll('.revoke-key-btn').forEach(btn => {
    btn.addEventListener('click', () => {
      const keyId = (btn as HTMLElement).dataset.keyId;
      if (keyId) void revokeApiKey(keyId);
    });
  });
  container.querySelectorAll('.delete-key-btn').forEach(btn => {
    btn.addEventListener('click', () => {
      const keyId = (btn as HTMLElement).dataset.keyId;
      if (keyId) void deleteApiKey(keyId);
    });
  });
}

/**
 * Format an (action, resource) permission entry for display in the
 * create-key picker. No catalog of human labels is maintained here --
 * the values are the closed vocabulary the backend already validates
 * (internal/auth/types.go), so the raw "action:resource" pair is
 * unambiguous and never goes stale against a separate label list.
 */
function formatPermissionLabel(entry: api.PermissionEntry): string {
  if (entry.action === 'admin' && entry.resource === '*') {
    return 'Admin (all actions, all resources)';
  }
  return `${entry.action}:${entry.resource}`;
}

/**
 * Render the create-key permission picker as one checkbox per entry the
 * calling user actually holds (issue #61's frontend follow-up): after the
 * backend fix, CreateAPIKey rejects an empty permissions array, so a
 * picker limited to the owner's own effective permissions is both the
 * fix for the resulting "every key creation 400s" regression and the
 * least-privilege UI the per-key scoping feature always intended.
 *
 * Every interpolated value is escaped even though action/resource come
 * from the closed backend vocabulary -- defense in depth for an innerHTML
 * write.
 */
function renderApiKeyPermissionPicker(entries: api.PermissionEntry[]): void {
  const container = document.getElementById('apikey-permissions-list');
  if (!container) return;

  if (entries.length === 0) {
    container.innerHTML = '<p class="text-muted">No permissions available on your account -- an API key cannot be created.</p>';
    return;
  }

  container.innerHTML = entries.map((entry, i) => `
    <label class="apikey-permission-option">
      <input type="checkbox" class="apikey-permission-checkbox" id="apikey-perm-${i}"
             data-action="${escapeHtml(entry.action)}" data-resource="${escapeHtml(entry.resource)}">
      ${escapeHtml(formatPermissionLabel(entry))}
    </label>
  `).join('');

  container.querySelectorAll('.apikey-permission-checkbox').forEach(cb => {
    cb.addEventListener('change', updateCreateKeySubmitState);
  });
}

/**
 * Read the permissions currently checked in the picker.
 */
function collectSelectedApiKeyPermissions(): api.Permission[] {
  const container = document.getElementById('apikey-permissions-list');
  if (!container) return [];

  const permissions: api.Permission[] = [];
  container.querySelectorAll<HTMLInputElement>('.apikey-permission-checkbox:checked').forEach(cb => {
    const action = cb.dataset['action'];
    const resource = cb.dataset['resource'];
    if (action && resource) permissions.push({ action, resource });
  });
  return permissions;
}

/**
 * Enable the Create button only while at least one permission is
 * selected. An unscoped key is now rejected server-side (400), and
 * disabling here catches it before the round-trip rather than after.
 */
function updateCreateKeySubmitState(): void {
  const submitBtn = document.getElementById('create-apikey-submit-btn') as HTMLButtonElement | null;
  if (!submitBtn) return;
  submitBtn.disabled = collectSelectedApiKeyPermissions().length === 0;
}

/**
 * Show create API key modal.
 *
 * Fetches the caller's own effective permissions fresh on every open
 * (rather than reusing a cached snapshot from login) so the picker
 * reflects the account's CURRENT grants -- a group change since login
 * shows up immediately instead of offering a permission the key would
 * then fail to obtain from the intersection check server-side.
 */
export async function showCreateKeyModal(): Promise<void> {
  const modal = document.getElementById('create-apikey-modal');
  const form = document.getElementById('create-apikey-form') as HTMLFormElement;
  const errorEl = document.getElementById('create-apikey-error');
  const submitBtn = document.getElementById('create-apikey-submit-btn') as HTMLButtonElement | null;

  if (!modal || !form) return;

  form.reset();
  if (errorEl) errorEl.classList.add('hidden');
  if (submitBtn) submitBtn.disabled = true;

  // Expiration is required (issue #102: no more "never expires"). Prefill
  // 90 days out so the field never starts empty; the actual cap is
  // enforced server-side.
  const expiresAtInput = document.getElementById('apikey-expires-at') as HTMLInputElement | null;
  if (expiresAtInput) {
    const defaultDate = new Date();
    defaultDate.setDate(defaultDate.getDate() + 90);
    expiresAtInput.value = toLocalDateInputValue(defaultDate);
  }

  const permissionsContainer = document.getElementById('apikey-permissions-list');
  if (permissionsContainer) permissionsContainer.innerHTML = '<p class="text-muted">Loading your permissions&hellip;</p>';

  // Open the modal before the permissions fetch resolves so the dialog
  // never sits closed while waiting on the network.
  openModal(modal);

  try {
    const { permissions } = await api.getUserPermissions();
    renderApiKeyPermissionPicker(permissions);
  } catch (error) {
    console.error('Failed to load permissions for API key creation:', error);
    if (permissionsContainer) {
      permissionsContainer.innerHTML = '<p class="error">Failed to load your permissions. Close and reopen this dialog to retry.</p>';
    }
  }
  updateCreateKeySubmitState();
}

/**
 * Formats a Date as the YYYY-MM-DD value an <input type="date"> expects,
 * using local calendar components. toISOString() converts to UTC first,
 * which can shift the date by a day near local midnight.
 */
function toLocalDateInputValue(date: Date): string {
  const year = date.getFullYear();
  const month = String(date.getMonth() + 1).padStart(2, '0');
  const day = String(date.getDate()).padStart(2, '0');
  return `${year}-${month}-${day}`;
}

/**
 * Parses an <input type="date"> value (YYYY-MM-DD) as local midnight.
 * new Date(dateString) parses it as UTC midnight instead, which can read as
 * "yesterday evening" in timezones behind UTC and reject a same-day or
 * next-day expiration that should be valid (CodeRabbit finding on #392).
 */
function parseLocalDateInputValue(value: string): Date {
  const [year, month, day] = value.split('-').map(Number);
  return new Date(year ?? 0, (month ?? 1) - 1, day ?? 1);
}

/**
 * Close create API key modal
 */
export function closeCreateKeyModal(): void {
  const modal = document.getElementById('create-apikey-modal');
  if (modal) closeModal(modal);
}

/**
 * Create new API key. password, expiresAt and permissions are all required
 * server-side: creation re-verifies the caller's password (issue #102), a
 * key can no longer be minted with no expiration (issue #102), and an
 * empty permissions array is rejected outright (issue #61).
 */
export async function createApiKey(name: string, password: string, expiresAt: Date, permissions: api.Permission[]): Promise<CreateAPIKeyResponse> {
  try {
    const request: api.CreateAPIKeyRequest = {
      name,
      password: api.base64Encode(password),
      expires_at: expiresAt.toISOString(),
      permissions,
    };

    const response = await api.createApiKey(request);
    return response;
  } catch (error) {
    console.error('Failed to create API key:', error);
    throw error;
  }
}

/**
 * Handle create API key form submission
 */
let createInFlight = false;

export async function handleCreateApiKey(e: Event): Promise<void> {
  e.preventDefault();
  if (createInFlight) return;

  const errorEl = document.getElementById('create-apikey-error');
  if (errorEl) errorEl.classList.add('hidden');

  const name = (document.getElementById('apikey-name') as HTMLInputElement | null)?.value.trim() ?? '';
  const password = (document.getElementById('apikey-password') as HTMLInputElement | null)?.value ?? '';
  const expiresAtInput = (document.getElementById('apikey-expires-at') as HTMLInputElement | null)?.value ?? '';

  if (!name) {
    showError('API key name is required');
    return;
  }
  if (!password) {
    showError('Your password is required to create an API key');
    return;
  }
  if (!expiresAtInput) {
    showError('An expiration date is required');
    return;
  }

  // Belt-and-suspenders alongside the disabled submit button: the backend
  // now rejects an empty permissions array with 400 (issue #61), so an
  // unscoped key can never be minted. Catching it here avoids the round
  // trip and gives a clearer message than the raw 400 body.
  const permissions = collectSelectedApiKeyPermissions();
  if (permissions.length === 0) {
    showError('Select at least one permission');
    return;
  }

  const expiresAt = parseLocalDateInputValue(expiresAtInput);
  if (expiresAt <= new Date()) {
    showError('Expiration date must be in the future');
    return;
  }

  createInFlight = true;
  const submitBtn = document.getElementById('create-apikey-submit-btn') as HTMLButtonElement | null;
  if (submitBtn) submitBtn.disabled = true;
  try {
    const response = await createApiKey(name, password, expiresAt, permissions);
    closeCreateKeyModal();
    showKeyCreatedModal(response.api_key);
    await loadApiKeys();
  } catch (error) {
    const err = error as Error;
    showError(`Failed to create API key: ${err.message}`);
  } finally {
    createInFlight = false;
    updateCreateKeySubmitState();
  }
}

/**
 * Show key created modal with one-time display
 */
export function showKeyCreatedModal(apiKey: string): void {
  // Remove any existing modal to prevent duplicates
  document.getElementById('apikey-created-modal')?.remove();

  const modal = document.createElement('div');
  modal.id = 'apikey-created-modal';
  modal.className = 'modal';
  modal.dataset.requireAck = 'true';
  modal.innerHTML = `
    <div class="modal-content">
      <h2>API Key Created Successfully</h2>
      <div class="warning-box">
        <strong>Important:</strong> This is the only time you'll see this API key.
        Please copy it now and store it securely.
      </div>
      <div class="apikey-display">
        <label>Your API Key:</label>
        <div class="apikey-value-container">
          <code id="apikey-value" class="apikey-value">${escapeHtml(apiKey)}</code>
          <button type="button" id="copy-apikey-btn" class="btn-small primary">Copy</button>
        </div>
      </div>
      <div class="modal-info">
        <p>Use this key in the <code>X-API-Key</code> header when making API requests:</p>
        <pre class="code-example">curl -H "X-API-Key: ${escapeHtml(apiKey)}" https://your-api-endpoint.com/api/...</pre>
      </div>
      <div class="modal-buttons">
        <button type="button" id="close-apikey-created-btn" class="primary">I've Copied the Key</button>
      </div>
    </div>
  `;

  document.body.appendChild(modal);
  openModal(modal);

  // Setup copy button
  const copyBtn = document.getElementById('copy-apikey-btn');
  if (copyBtn) {
    copyBtn.addEventListener('click', () => {
      navigator.clipboard.writeText(apiKey).then(() => {
        copyBtn.textContent = 'Copied!';
        copyBtn.classList.add('copied');
        setTimeout(() => {
          copyBtn.textContent = 'Copy';
          copyBtn.classList.remove('copied');
        }, 2000);
      }).catch(err => {
        console.error('Failed to copy:', err);
        alert('Failed to copy to clipboard. Please copy manually.');
      });
    });
  }

  // Setup close button. closeModal first so the focus trap is torn
  // down and focus is restored to the original trigger; then remove
  // the dynamically-injected element from the DOM.
  const closeBtn = document.getElementById('close-apikey-created-btn');
  if (closeBtn) {
    closeBtn.addEventListener('click', () => {
      delete modal.dataset.requireAck;
      closeModal(modal);
      modal.remove();
    });
  }
}

/**
 * Revoke an API key
 */
export async function revokeApiKey(keyId: string): Promise<void> {
  const key = currentApiKeys.find(k => k.id === keyId);
  if (!key) return;

  const ok = await confirmDialog({
    title: `Revoke API key "${key.name}"?`,
    body: 'The key will immediately stop working. This action cannot be undone. (You can delete the row afterwards to remove it from the list.)',
    confirmLabel: 'Revoke key',
    destructive: true,
  });
  if (!ok) return;

  try {
    await api.revokeApiKey(keyId);
    await loadApiKeys();
  } catch (error) {
    console.error('Failed to revoke API key:', error);
    showError('Failed to revoke API key');
  }
}

/**
 * Delete an API key
 */
export async function deleteApiKey(keyId: string): Promise<void> {
  const key = currentApiKeys.find(k => k.id === keyId);
  if (!key) return;

  const ok = await confirmDialog({
    title: `Delete API key "${key.name}"?`,
    body: 'This permanently removes the key from the list. If the key is still active, it will also stop working.',
    confirmLabel: 'Delete key',
    destructive: true,
  });
  if (!ok) return;

  try {
    await api.deleteApiKey(keyId);
    await loadApiKeys();
  } catch (error) {
    console.error('Failed to delete API key:', error);
    showError('Failed to delete API key');
  }
}

/**
 * Initialize API keys management
 */
export function initApiKeys(): void {
  // Setup create key button
  const createKeyBtn = document.getElementById('create-apikey-btn');
  if (createKeyBtn) {
    createKeyBtn.addEventListener('click', () => void showCreateKeyModal());
  }

  // Setup close modal button
  const closeModalBtn = document.getElementById('close-create-apikey-modal-btn');
  if (closeModalBtn) {
    closeModalBtn.addEventListener('click', () => closeCreateKeyModal());
  }

  // Setup form submission
  const form = document.getElementById('create-apikey-form');
  if (form) {
    form.addEventListener('submit', (e) => void handleCreateApiKey(e));
  }

  // Close modal when clicking outside
  const modal = document.getElementById('create-apikey-modal');
  if (modal) {
    modal.addEventListener('click', (e) => {
      if (e.target === modal) closeCreateKeyModal();
    });
  }
}

/**
 * Show error message.
 *
 * When the Create-API-Key modal is open, validation-style errors belong
 * inline on the form (so the user sees them next to the offending
 * field). Outside that context — load failures, revoke/delete errors —
 * we surface via the shared toast system (Q4) so the message matches
 * the rest of the app rather than using a blocking alert().
 */
function showError(message: string): void {
  const errorEl = document.getElementById('create-apikey-error');
  const createModalOpen = !document.getElementById('create-apikey-modal')?.classList.contains('hidden');
  if (errorEl && createModalOpen) {
    errorEl.textContent = message;
    errorEl.classList.remove('hidden');
  } else {
    showToast({ message, kind: 'error' });
  }
}

/**
 * Format a per-row request count for display in the table cells.
 * Renders the exact integer (no abbreviation) so a row with "1,234,567"
 * is unambiguous — the section-level summary card in apikeys_usage.ts
 * uses a separate `formatCount` that abbreviates large values to fit
 * the tile width.
 *
 * `null` means the backend does not know the count (a key already in use
 * before migration 000094 added the counters) and renders as NO_COUNT_DATA
 * ("n/a") rather than "0", which would state a request volume nobody
 * measured.
 * `undefined` (an older cached response with no counter fields at all) is
 * treated the same way, as is any non-finite or negative value.
 */
function formatRequestCount(n: number | null | undefined): string {
  if (typeof n !== 'number' || !Number.isFinite(n) || n < 0) return NO_COUNT_DATA;
  return Math.trunc(n).toLocaleString('en-US');
}
