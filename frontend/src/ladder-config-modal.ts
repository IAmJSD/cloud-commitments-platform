/**
 * Markup for the per-account Commitment Laddering config modal (Add/Edit
 * Account Config). Kept apart from ladder.ts so that module stays focused on
 * loading, wiring and saving. Field IDs are the contract with ladder.ts and
 * ladder-account-selector.ts.
 */

export function renderLadderConfigModal(): string {
  return `
<!-- Per-account ladder config modal -->
<div id="ladder-config-modal" class="modal hidden" role="dialog" aria-modal="true"
     aria-labelledby="ladder-modal-title">
  <div class="modal-content">
    <div class="modal-header">
      <h3 id="ladder-modal-title">Commitment Laddering Config</h3>
      <button type="button" class="modal-close" id="ladder-modal-close-btn"
              aria-label="Close">&times;</button>
    </div>
    <div class="modal-body">
      <form id="ladder-config-form" novalidate>
        <input type="hidden" id="ladder-cfg-id">

        <div class="form-row">
          <label for="ladder-cfg-account">Cloud Account</label>
          <select id="ladder-cfg-account" required aria-describedby="ladder-cfg-account-status"></select>
          <span id="ladder-cfg-account-status" class="form-hint" role="status"></span>
        </div>

        <div class="form-row">
          <label for="ladder-cfg-provider">Provider</label>
          <select id="ladder-cfg-provider">
            <option value="aws">AWS</option>
            <option value="azure">Azure</option>
            <option value="gcp">GCP</option>
          </select>
        </div>

        <div class="form-row">
          <label class="form-checkbox-label">
            <input type="checkbox" id="ladder-cfg-enabled">
            Enabled for this account
          </label>
        </div>

        <div class="form-row">
          <label for="ladder-cfg-mode">Approval Mode</label>
          <select id="ladder-cfg-mode">
            <option value="email_approval">Email Approval</option>
            <option value="auto_approve">Auto Approve</option>
          </select>
        </div>
        <p id="ladder-cfg-mode-warning" class="warning-message" role="status" hidden>
          <strong>Warning:</strong> Auto Approve skips human approval. Ladder purchases for
          this account will run without an approval email, and commitments are long-term
          and usually cannot be cancelled once bought.
        </p>

        <div class="form-row">
          <label for="ladder-cfg-cadence">Cadence</label>
          <select id="ladder-cfg-cadence">
            <option value="daily">Daily</option>
            <option value="weekly">Weekly</option>
          </select>
        </div>

        <div class="form-row">
          <label for="ladder-cfg-target-coverage">Target Coverage (%)</label>
          <input type="number" id="ladder-cfg-target-coverage"
                 value="100" min="1" max="100" step="0.01">
        </div>

        <div class="form-row">
          <label for="ladder-cfg-buffer-fraction">Buffer Fraction (0&ndash;1)</label>
          <input type="number" id="ladder-cfg-buffer-fraction"
                 value="0.10" min="0" max="0.99" step="0.01">
        </div>

        <div class="form-row">
          <label for="ladder-cfg-baseline-percentile">Baseline Percentile (1&ndash;50)</label>
          <input type="number" id="ladder-cfg-baseline-percentile"
                 value="5" min="1" max="50" step="0.1">
        </div>

        <div class="form-row">
          <label for="ladder-cfg-lookback-days">Lookback Days (minimum 10)</label>
          <input type="number" id="ladder-cfg-lookback-days"
                 value="30" min="10" step="1">
        </div>

        <div class="form-row">
          <label for="ladder-cfg-buf-util-threshold">Buffer Utilization Threshold (%)</label>
          <input type="number" id="ladder-cfg-buf-util-threshold"
                 value="90" min="1" max="100" step="0.1">
        </div>

        <div class="form-row">
          <label for="ladder-cfg-max-hourly">Max Hourly Commit Per Run ($/hr, blank = no cap)</label>
          <input type="number" id="ladder-cfg-max-hourly"
                 placeholder="No cap" min="0.000001" step="any">
        </div>

        <div class="form-row">
          <label for="ladder-cfg-max-actions">Max Actions Per Run (1&ndash;50)</label>
          <input type="number" id="ladder-cfg-max-actions"
                 value="10" min="1" max="50" step="1">
        </div>

        <div class="form-row">
          <label for="ladder-cfg-ramp-schedule">Ramp Schedule (JSON)</label>
          <textarea id="ladder-cfg-ramp-schedule" rows="4"
                    placeholder='{"steps":[{"after_days":0,"fraction":1.0}]}'></textarea>
          <span class="form-hint">
            An array of steps. Each step has <code>after_days</code> (integer) and
            <code>fraction</code> (0&ndash;1). Fractions must sum to 1.0.
            Example: immediate &mdash; <code>{"steps":[{"after_days":0,"fraction":1.0}]}</code>
          </span>
        </div>

        <div class="modal-footer">
          <button type="button" class="btn btn-secondary" id="ladder-modal-cancel-btn">
            Cancel
          </button>
          <button type="submit" class="btn btn-primary" id="ladder-config-save-btn">
            Save Config
          </button>
        </div>
      </form>
    </div>
  </div>
</div>
`;
}
