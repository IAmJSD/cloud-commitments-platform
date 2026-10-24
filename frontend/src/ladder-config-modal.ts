/**
 * Markup for the per-account Commitment Laddering config modal (Add/Edit
 * Account Config). Kept apart from ladder.ts so that module stays focused on
 * loading, wiring and saving. Field IDs are the contract with ladder.ts and
 * ladder-account-selector.ts.
 *
 * Layout (styles/ladder-form.css): labels sit above their inputs, the short
 * fields pair up in a two-column grid that collapses to one column on narrow
 * screens, and range/unit qualifiers live in a muted hint under each input.
 * The header stays put while the body scrolls on short viewports.
 */

export function renderLadderConfigModal(): string {
  return `
<!-- Per-account ladder config modal -->
<div id="ladder-config-modal" class="modal hidden" role="dialog" aria-modal="true"
     aria-labelledby="ladder-modal-title">
  <div class="modal-content ladder-config-dialog">
    <div class="modal-header">
      <h3 id="ladder-modal-title">Commitment Laddering Config</h3>
      <button type="button" class="modal-close" id="ladder-modal-close-btn"
              aria-label="Close">&times;</button>
    </div>
    <div class="modal-body">
      <form id="ladder-config-form" class="ladder-form-grid" novalidate>
        <input type="hidden" id="ladder-cfg-id">

        <div class="ladder-field">
          <label for="ladder-cfg-account">Cloud Account</label>
          <select id="ladder-cfg-account" required aria-describedby="ladder-cfg-account-status"></select>
          <span id="ladder-cfg-account-status" class="form-hint" role="status"></span>
        </div>

        <div class="ladder-field">
          <label for="ladder-cfg-provider">Provider</label>
          <select id="ladder-cfg-provider">
            <option value="aws">AWS</option>
            <option value="azure">Azure</option>
            <option value="gcp">GCP</option>
          </select>
        </div>

        <div class="ladder-field">
          <label for="ladder-cfg-mode">Approval Mode</label>
          <select id="ladder-cfg-mode">
            <option value="email_approval">Email Approval</option>
            <option value="auto_approve">Auto Approve</option>
          </select>
        </div>

        <div class="ladder-field ladder-field-check">
          <label class="form-checkbox-label">
            <input type="checkbox" id="ladder-cfg-enabled">
            Enabled for this account
          </label>
        </div>

        <p id="ladder-cfg-mode-warning" class="warning-message ladder-field-full" role="status" hidden>
          <strong>Warning:</strong> Auto Approve skips human approval. Ladder purchases for
          this account will run without an approval email, and commitments are long-term
          and usually cannot be cancelled once bought.
        </p>

        <h4 class="ladder-form-section">Schedule and limits</h4>

        <div class="ladder-field">
          <label for="ladder-cfg-cadence">Cadence</label>
          <select id="ladder-cfg-cadence">
            <option value="daily">Daily</option>
            <option value="weekly">Weekly</option>
          </select>
        </div>

        <div class="ladder-field">
          <label for="ladder-cfg-lookback-days">Lookback Days</label>
          <input type="number" id="ladder-cfg-lookback-days" aria-describedby="ladder-cfg-lookback-days-hint"
                 value="30" min="10" step="1">
          <span id="ladder-cfg-lookback-days-hint" class="form-hint">Minimum 10</span>
        </div>

        <div class="ladder-field">
          <label for="ladder-cfg-target-coverage">Target Coverage (%)</label>
          <input type="number" id="ladder-cfg-target-coverage" aria-describedby="ladder-cfg-target-coverage-hint"
                 value="100" min="1" max="100" step="0.01">
          <span id="ladder-cfg-target-coverage-hint" class="form-hint">Between 1 and 100</span>
        </div>

        <div class="ladder-field">
          <label for="ladder-cfg-buffer-fraction">Buffer Fraction</label>
          <input type="number" id="ladder-cfg-buffer-fraction" aria-describedby="ladder-cfg-buffer-fraction-hint"
                 value="0.10" min="0" max="0.99" step="0.01">
          <span id="ladder-cfg-buffer-fraction-hint" class="form-hint">Between 0 and 1</span>
        </div>

        <div class="ladder-field">
          <label for="ladder-cfg-baseline-percentile">Baseline Percentile</label>
          <input type="number" id="ladder-cfg-baseline-percentile" aria-describedby="ladder-cfg-baseline-percentile-hint"
                 value="5" min="1" max="50" step="0.1">
          <span id="ladder-cfg-baseline-percentile-hint" class="form-hint">Between 1 and 50</span>
        </div>

        <div class="ladder-field">
          <label for="ladder-cfg-buf-util-threshold">Buffer Utilization Threshold (%)</label>
          <input type="number" id="ladder-cfg-buf-util-threshold" aria-describedby="ladder-cfg-buf-util-threshold-hint"
                 value="90" min="1" max="100" step="0.1">
          <span id="ladder-cfg-buf-util-threshold-hint" class="form-hint">Between 1 and 100</span>
        </div>

        <div class="ladder-field">
          <label for="ladder-cfg-max-hourly">Max Hourly Commit Per Run ($/hr)</label>
          <input type="number" id="ladder-cfg-max-hourly" aria-describedby="ladder-cfg-max-hourly-hint"
                 placeholder="No cap" min="0.000001" step="any">
          <span id="ladder-cfg-max-hourly-hint" class="form-hint">Leave blank for no cap</span>
        </div>

        <div class="ladder-field">
          <label for="ladder-cfg-max-actions">Max Actions Per Run</label>
          <input type="number" id="ladder-cfg-max-actions" aria-describedby="ladder-cfg-max-actions-hint"
                 value="10" min="1" max="50" step="1">
          <span id="ladder-cfg-max-actions-hint" class="form-hint">Between 1 and 50</span>
        </div>

        <div class="ladder-field ladder-field-full">
          <label for="ladder-cfg-ramp-schedule">Ramp Schedule (JSON)</label>
          <textarea id="ladder-cfg-ramp-schedule" rows="4" aria-describedby="ladder-cfg-ramp-schedule-hint"
                    placeholder='{"steps":[{"after_days":0,"fraction":1.0}]}'></textarea>
          <span id="ladder-cfg-ramp-schedule-hint" class="form-hint">
            An array of steps. Each step has <code>after_days</code> (integer) and
            <code>fraction</code> (0&ndash;1). Fractions must sum to 1.0.
            Example: immediate &mdash; <code>{"steps":[{"after_days":0,"fraction":1.0}]}</code>
          </span>
        </div>

        <div class="modal-footer ladder-field-full">
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
