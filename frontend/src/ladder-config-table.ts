/**
 * Per-account ladder configuration table (issue #742).
 *
 * Shows each configuration's registered account name and external ID instead
 * of the internal account UUID. The UUID stays available as the cell's title
 * so support can still match it against API payloads. When the account list
 * is unavailable (still loading, forbidden, or the account was removed) the
 * cell falls back to the stored ID so the row is never blank.
 */

import type { AccountSummary } from './api/accounts';
import type { LadderConfig } from './api/ladder';
import { escapeHtml, formatDate } from './utils';

export interface LadderConfigTableOptions {
  accounts: readonly AccountSummary[];
  canEdit: boolean;
  onEdit: (config: LadderConfig) => void;
}

export function ladderAccountLabel(config: LadderConfig, accounts: readonly AccountSummary[]): string {
  const account = accounts.find(item => item.id === config.cloud_account_id && item.provider === config.provider);
  return account ? `${account.name} (${account.external_id})` : config.cloud_account_id;
}

export function renderLadderConfigTable(container: HTMLElement, configs: readonly LadderConfig[], options: LadderConfigTableOptions): void {
  if (configs.length === 0) {
    container.innerHTML = '<p class="settings-help">No per-account configurations yet. Click <strong>Add Account Config</strong> to create one.</p>';
    return;
  }

  const rows = configs.map(cfg => `
    <tr>
      <td title="${escapeHtml(cfg.cloud_account_id)}">${escapeHtml(ladderAccountLabel(cfg, options.accounts))}</td>
      <td>${escapeHtml(cfg.provider.toUpperCase())}</td>
      <td>${cfg.enabled ? 'Yes' : 'No'}</td>
      <td>${escapeHtml(cfg.mode === 'email_approval' ? 'Email Approval' : 'Auto Approve')}</td>
      <td>${escapeHtml(cfg.cadence === 'daily' ? 'Daily' : 'Weekly')}</td>
      <td>${cfg.target_coverage.toFixed(1)}%</td>
      <td>${cfg.updated_at ? escapeHtml(formatDate(cfg.updated_at)) : 'N/A'}</td>
      ${options.canEdit
        ? `<td><button type="button" class="btn btn-sm btn-secondary ladder-edit-btn"
                       data-account-id="${escapeHtml(cfg.cloud_account_id)}"
                       data-provider="${escapeHtml(cfg.provider)}">Edit</button></td>`
        : '<td></td>'}
    </tr>
  `).join('');

  container.innerHTML = `
    <table class="data-table" aria-label="Per-account ladder configurations">
      <thead>
        <tr>
          <th>Account</th>
          <th>Provider</th>
          <th>Enabled</th>
          <th>Mode</th>
          <th>Cadence</th>
          <th>Target Coverage</th>
          <th>Updated</th>
          <th></th>
        </tr>
      </thead>
      <tbody>${rows}</tbody>
    </table>
  `;

  // Wire edit buttons via querySelectorAll so no user data is ever injected
  // into a JS string context (data-* attributes are HTML-attribute-context only).
  container.querySelectorAll<HTMLButtonElement>('.ladder-edit-btn').forEach(btn => {
    btn.addEventListener('click', () => {
      const accountID = btn.dataset['accountId'] ?? '';
      const provider = btn.dataset['provider'] ?? '';
      const cfg = configs.find(c => c.cloud_account_id === accountID && c.provider === provider);
      if (cfg) options.onEdit(cfg);
    });
  });
}
