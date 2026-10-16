package api

import "net/http"

// codeEditorReconciliationRequired is the machine-readable body code for a
// legacy write refused because the plan is owned by the schedule editor.
const codeEditorReconciliationRequired = "editor_reconciliation_required"

// codeManagedPlanLiveExecutions is the body code for deleting a managed plan
// that still has active or unsettled purchases.
const codeManagedPlanLiveExecutions = "managed_plan_has_live_executions"

// planEditorRefusal builds a client error carrying a machine-readable code.
func planEditorRefusal(status int, code, msg string) error {
	return NewClientErrorWithDetails(status, msg, map[string]any{"code": code})
}

func editorReconciliationError() error {
	return planEditorRefusal(http.StatusConflict, codeEditorReconciliationRequired,
		"this plan is managed by the schedule editor; make this change in the editor")
}
