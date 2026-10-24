/**
 * Commitment Laddering settings section (issue #1333 phase 3).
 *
 * Renders the per-account ladder config editor inside the
 * #commitment-laddering-settings placeholder div that lives inside the
 * Purchasing Settings panel. The section is flag-gated default-off:
 *
 *   - Global kill-switch: global_config.laddering_enabled (bool)
 *   - Per-account: LadderConfig.enabled (bool) per (account, provider) pair
 *
 * No laddering engine runs fire until both flags are true. This PR (phase 3,
 * schema foundation) wires the UI to read and write configs; actual engine
 * invocation is in a later phase.
 */

import * as api from './api';
import { showToast } from './toast';
import { confirmDialog } from './confirmDialog';
import { openModal, closeModal } from './modal';
import { canAccess } from './permissions';
import { initLadderTimeline, refreshLadderTimeline } from './ladder-timeline';
import { initLadderAccountSelector } from './ladder-account-selector';
import { renderLadderConfigTable } from './ladder-config-table';
import { renderLadderConfigModal } from './ladder-config-modal';
import { listAccountsMinimal, type AccountSummary } from './api/accounts';
import { readLadderNumbers } from './ladder-form-numbers';

// ==========================================
// MODULE STATE
// ==========================================

// Separate in-flight guards for the two independent save paths. Sharing one
// flag let the kill-switch toggle and the per-account modal save block/revert
// each other even though they touch unrelated backend endpoints.
let killSwitchSaveInFlight = false;
let configModalSaveInFlight = false;

// Cached configs from the last successful load.
let cachedConfigs: api.LadderConfig[] = [];
// Registered accounts used to label table rows; empty when the list could not
// be loaded, in which case rows fall back to the stored account ID.
let tableAccounts: AccountSummary[] = [];
let selectedAccount: ReturnType<typeof initLadderAccountSelector> | undefined;

// ==========================================
// PUBLIC API
// ==========================================

/**
 * Initialize the Commitment Laddering settings section.
 *
 * Renders the HTML form into #commitment-laddering-settings and populates
 * it with data from the API. Should be called from loadGlobalSettings after
 * the global config response is available.
 *
 * The globalEnabled parameter is the current value of
 * global_config.laddering_enabled so the kill-switch toggle reflects the
 * persisted state without a second API call.
 */
export async function initLadderingSettings(globalEnabled: boolean): Promise<void> {
  const container = document.getElementById('commitment-laddering-settings');
  if (!container) return;

  container.innerHTML = renderLadderingSection(globalEnabled);
  selectedAccount = undefined;
  wireKillSwitchToggle();
  wireModalCloseButtons();

  try {
    const [configs, accounts] = await Promise.all([
      api.getLadderConfigs(),
      listAccountsMinimal().catch((): AccountSummary[] => []),
    ]);
    cachedConfigs = configs;
    tableAccounts = accounts;
    renderConfigTable(cachedConfigs);
    const timeline = document.getElementById('ladder-purchase-timeline');
    if (timeline) await initLadderTimeline(timeline, cachedConfigs, globalEnabled);
  } catch (err) {
    console.error('Failed to load ladder configs:', err);
    const tableContainer = document.getElementById('ladder-configs-table-container');
    if (tableContainer) {
      tableContainer.innerHTML = '<p class="settings-error">Failed to load per-account configurations.</p>';
    }
  }

  if (canAccess('update', 'config')) {
    document.getElementById('ladder-add-config-btn')?.addEventListener('click', () => openLadderConfigModal());
  }
}

// ==========================================
// RENDERING
// ==========================================

function renderLadderingSection(globalEnabled: boolean): string {
  const canEdit = canAccess('update', 'config');
  const disabledAttr = canEdit ? '' : ' disabled';

  return `
<fieldset class="settings-category" id="commitment-laddering-automation-settings">
  <legend>Commitment Laddering</legend>
  <p class="settings-help">
    Commitment Laddering automatically ramps up cloud commitments towards a
    target coverage level using a configurable ramp schedule. It is feature-gated:
    enable the global kill-switch below, then enable individual accounts via the
    per-account configuration table.
  </p>

  <div class="setting-row">
    <div class="setting-info">
      <!-- Plain span (not <label for=>) so clicking the descriptive text
           does not flip the toggle. The toggle's .toggle-label wraps the input
           and provides the click target. aria-labelledby on the input keeps the
           descriptive text as the toggle's accessible name. Same pattern as the
           Auto-collect row (issue #464 / #1412). -->
      <span class="label-like" id="setting-laddering-enabled-label">Enable Commitment Laddering</span>
      <span class="setting-hint">Global kill-switch. Must be on before any per-account config can fire.</span>
    </div>
    <div class="setting-input">
      <label class="toggle-label">
        <input type="checkbox" id="setting-laddering-enabled"
               aria-labelledby="setting-laddering-enabled-label"
               ${globalEnabled ? 'checked' : ''}${disabledAttr}>
        <span class="slider"></span>
      </label>
    </div>
  </div>

  <div id="ladder-configs-section">
    <h4>Per-Account Configurations</h4>
    <div id="ladder-configs-table-container">
      <p class="settings-help">Loading&hellip;</p>
    </div>
    ${canEdit ? `
    <button type="button" id="ladder-add-config-btn" class="btn btn-secondary">
      Add Account Config
    </button>` : ''}
  </div>
</fieldset>
<!-- Planned purchases renders its own settings card (issue #753). -->
<div id="ladder-purchase-timeline"></div>

${renderLadderConfigModal()}
`;
}

// Exported for unit testing (mirrors the apikeys.ts convention of exposing
// render/action helpers so tests can drive them directly).
export function renderConfigTable(configs: api.LadderConfig[]): void {
  const container = document.getElementById('ladder-configs-table-container');
  if (!container) return;
  renderLadderConfigTable(container, configs, {
    accounts: tableAccounts,
    canEdit: canAccess('update', 'config'),
    onEdit: cfg => openLadderConfigModal(cfg),
  });
}

// ==========================================
// MODAL CLOSE WIRING
// ==========================================

// closeLadderModal hides the per-account config modal through the shared
// helper, which also releases the focus trap and restores focus (#731).
function closeLadderModal(): void {
  const modal = document.getElementById('ladder-config-modal');
  if (modal) closeModal(modal);
}

// wireModalCloseButtons attaches click listeners to the modal's × and Cancel
// buttons. Uses addEventListener rather than inline onclick for CSP
// consistency with the dynamically-wired edit buttons (no inline handlers
// anywhere in this module).
function wireModalCloseButtons(): void {
  document.getElementById('ladder-modal-close-btn')?.addEventListener('click', closeLadderModal);
  document.getElementById('ladder-modal-cancel-btn')?.addEventListener('click', closeLadderModal);
}

// ==========================================
// KILL-SWITCH TOGGLE
// ==========================================

function wireKillSwitchToggle(): void {
  const toggle = document.getElementById('setting-laddering-enabled') as HTMLInputElement | null;
  if (!toggle || !canAccess('update', 'config')) return;

  toggle.addEventListener('change', async () => {
    if (killSwitchSaveInFlight) {
      toggle.checked = !toggle.checked; // revert optimistic
      return;
    }
    killSwitchSaveInFlight = true;
    try {
      // The backend merges a partial PUT over the stored config (json.Unmarshal
      // only sets keys present in the body), so we send ONLY the field we are
      // changing. Every other setting is preserved server-side; enumerating the
      // whole config here was fragile and dropped fields it did not list.
      await api.updateConfig({ laddering_enabled: toggle.checked });
      showToast({
        message: `Commitment Laddering ${toggle.checked ? 'enabled' : 'disabled'} globally.`,
        kind: 'success',
      });
      refreshLadderTimeline(cachedConfigs, toggle.checked);
    } catch (err) {
      toggle.checked = !toggle.checked; // revert on error
      showToast({ message: 'Failed to update laddering kill-switch.', kind: 'error' });
      console.error('Failed to toggle laddering_enabled:', err);
    } finally {
      killSwitchSaveInFlight = false;
    }
  });
}

// ==========================================
// PER-ACCOUNT CONFIG MODAL
// ==========================================

function openLadderConfigModal(existing?: api.LadderConfig): void {
  const modal = document.getElementById('ladder-config-modal');
  if (!modal) return;

  // Clone before populating: select values do not survive cloneNode.
  const form = document.getElementById('ladder-config-form');
  if (form) {
    const newForm = form.cloneNode(true) as HTMLElement;
    form.replaceWith(newForm);
    newForm.addEventListener('submit', (e) => {
      e.preventDefault();
      saveLadderConfig();
    });
    // Cancel is inside the cloned form; the outside Close button keeps its listener.
    newForm.querySelector('#ladder-modal-cancel-btn')
      ?.addEventListener('click', closeLadderModal);
    newForm.querySelector('#ladder-cfg-mode')?.addEventListener('change', syncAutoApproveWarning);
  }

  // Populate modal fields.
  setValue('ladder-cfg-id', existing?.id ?? '');
  setSelectValue('ladder-cfg-provider', existing?.provider ?? 'aws');
  setChecked('ladder-cfg-enabled', existing?.enabled ?? false);
  setSelectValue('ladder-cfg-mode', existing?.mode ?? 'email_approval');
  syncAutoApproveWarning();
  setSelectValue('ladder-cfg-cadence', existing?.cadence ?? 'daily');
  setValue('ladder-cfg-target-coverage', String(existing?.target_coverage ?? 100));
  setValue('ladder-cfg-buffer-fraction', String(existing?.buffer_fraction ?? 0.10));
  setValue('ladder-cfg-baseline-percentile', String(existing?.baseline_percentile ?? 5.0));
  setValue('ladder-cfg-lookback-days', String(existing?.lookback_days ?? 30));
  setValue('ladder-cfg-buf-util-threshold', String(existing?.buffer_utilization_threshold ?? 90));
  setValue('ladder-cfg-max-hourly', existing?.max_hourly_commit_per_run != null
    ? String(existing.max_hourly_commit_per_run)
    : '');
  setValue('ladder-cfg-max-actions', String(existing?.max_actions_per_run ?? 10));

  const defaultRamp = JSON.stringify({ steps: [{ after_days: 0, fraction: 1.0 }] }, null, 2);
  setValue('ladder-cfg-ramp-schedule',
    existing?.ramp_schedule ? JSON.stringify(existing.ramp_schedule, null, 2) : defaultRamp);

  selectedAccount = initLadderAccountSelector(document.getElementById('ladder-config-form')!, existing);
  // openModal shows it, moves focus inside, traps Tab and closes on Escape (#731).
  openModal(modal);
}

// Exported for unit testing (see renderConfigTable note).
export async function saveLadderConfig(): Promise<void> {
  if (configModalSaveInFlight) return;
  const identity = selectedAccount?.();
  if (!canAccess('update', 'config') || !identity) {
    showToast({ message: 'Select an available registered account before saving. Configuration update permission is required.', kind: 'error' });
    return;
  }

  const maxHourlyRaw = (document.getElementById('ladder-cfg-max-hourly') as HTMLInputElement | null)?.value?.trim() ?? '';
  const maxHourly: number | null = maxHourlyRaw === '' ? null : Number(maxHourlyRaw);
  if (maxHourlyRaw !== '' && (!Number.isFinite(maxHourly) || (maxHourly as number) <= 0)) {
    showToast({ message: 'Max hourly commit must be a positive number (or blank for no cap).', kind: 'error' });
    return;
  }

  const rampRaw = (document.getElementById('ladder-cfg-ramp-schedule') as HTMLTextAreaElement | null)?.value?.trim() ?? '';
  let rampSchedule: api.LadderConfig['ramp_schedule'];
  try {
    rampSchedule = JSON.parse(rampRaw);
  } catch {
    showToast({ message: 'Ramp schedule is not valid JSON.', kind: 'error' });
    return;
  }

  const numbers = readLadderNumbers();
  if (!numbers.ok) {
    showToast({ message: numbers.message, kind: 'error' });
    numbers.field?.focus();
    return;
  }

  const cfg: api.LadderConfig = {
    id: (document.getElementById('ladder-cfg-id') as HTMLInputElement | null)?.value || undefined,
    ...identity,
    enabled: (document.getElementById('ladder-cfg-enabled') as HTMLInputElement | null)?.checked ?? false,
    mode: ((document.getElementById('ladder-cfg-mode') as HTMLSelectElement | null)?.value ?? 'email_approval') as api.LadderConfig['mode'],
    cadence: ((document.getElementById('ladder-cfg-cadence') as HTMLSelectElement | null)?.value ?? 'daily') as api.LadderConfig['cadence'],
    ...numbers.values,
    max_hourly_commit_per_run: maxHourly,
    ramp_schedule: rampSchedule,
  };

  configModalSaveInFlight = true;
  const saveBtn = document.getElementById('ladder-config-save-btn') as HTMLButtonElement | null;
  if (saveBtn) saveBtn.disabled = true;

  try {
    if (activatesAutoApprove(cfg) && !(await confirmAutoApprove())) return;
    const saved = await api.upsertLadderConfig(cfg);

    // Update the in-memory cache and re-render the table.
    const idx = cachedConfigs.findIndex(
      c => c.cloud_account_id === saved.cloud_account_id && c.provider === saved.provider
    );
    if (idx >= 0) {
      cachedConfigs[idx] = saved;
    } else {
      cachedConfigs.push(saved);
    }
    renderConfigTable(cachedConfigs);
    refreshLadderTimeline(cachedConfigs);

    closeLadderModal();
    showToast({ message: 'Ladder config saved.', kind: 'success' });
  } catch (err) {
    // Surface the server's validation text (apiRequest copies the response's
    // `error` field into err.message) instead of a generic failure.
    const detail = err instanceof Error && err.message ? err.message : 'Check inputs and try again.';
    showToast({ message: `Failed to save ladder config: ${detail}`, kind: 'error' });
    console.error('saveLadderConfig:', err);
  } finally {
    configModalSaveInFlight = false;
    if (saveBtn) saveBtn.disabled = false;
  }
}

// ==========================================
// AUTO-APPROVE SAFEGUARDS
// ==========================================

function syncAutoApproveWarning(): void {
  const mode = document.getElementById('ladder-cfg-mode') as HTMLSelectElement | null;
  const warning = document.getElementById('ladder-cfg-mode-warning');
  if (warning) warning.hidden = mode?.value !== 'auto_approve';
}

// True when this save turns on unattended purchasing for the account: the
// saved config would be enabled with Auto Approve and the stored one was not.
function activatesAutoApprove(cfg: api.LadderConfig): boolean {
  const unattended = (c?: api.LadderConfig): boolean => !!c && c.enabled && c.mode === 'auto_approve';
  const previous = cachedConfigs.find(c => c.cloud_account_id === cfg.cloud_account_id && c.provider === cfg.provider);
  return unattended(cfg) && !unattended(previous);
}

function confirmAutoApprove(): Promise<boolean> {
  return confirmDialog({
    title: 'Enable Auto Approve?',
    body: 'Ladder purchases for this account will run without human approval. Commitments are long-term and usually cannot be cancelled once bought. Check the target coverage and per-run caps before continuing.',
    confirmLabel: 'Enable Auto Approve',
    destructive: true,
  });
}

// ==========================================
// DOM HELPERS
// ==========================================

function setValue(id: string, value: string): void {
  const el = document.getElementById(id) as HTMLInputElement | HTMLTextAreaElement | null;
  if (el) el.value = value;
}

function setSelectValue(id: string, value: string): void {
  const el = document.getElementById(id) as HTMLSelectElement | null;
  if (el) el.value = value;
}

function setChecked(id: string, checked: boolean): void {
  const el = document.getElementById(id) as HTMLInputElement | null;
  if (el) el.checked = checked;
}
