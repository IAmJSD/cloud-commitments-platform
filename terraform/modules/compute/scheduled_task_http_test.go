// HTTP sweep checks for issue #712: Cloud Run, GKE and Azure Container Apps
// schedule their sweep_tasks through one for_each scheduler resource named
// "sweep" that POSTs to /api/scheduled/<task>. wiredFrom counts every
// sweep_tasks key as wired; httpSweepProblems checks that the resource
// actually calls each enabled key, and on GCP that the OIDC audience it signs
// is one the server accepts.
package compute_test

import (
	"regexp"
	"strings"
	"testing"
)

var (
	sweepURIRe = regexp.MustCompile(`(?m)^\s*uri\s*=\s*"[^"\n]*/api/scheduled/\$\{each\.key\}"\s*$`)
	// GCP: the job signs local.sweep_audiences[each.key], the map is built from
	// active_sweeps with one /api/scheduled/<key> audience per key, and the
	// server's SCHEDULED_TASK_OIDC_AUDIENCE allowlist includes its values.
	sweepAudienceRe     = regexp.MustCompile(`(?m)^\s*audience\s*=\s*local\.sweep_audiences\[each\.key\]\s*$`)
	sweepAudiencesDefRe = regexp.MustCompile(`(?m)^\s*sweep_audiences\s*=\s*\{\s*for\s+(\w+)\s*,\s*\w+\s+in\s+local\.active_sweeps\s*:\s*(\w+)\s*=>\s*"[^"\n]*/api/scheduled/\$\{(\w+)\}"\s*\}`)
	sweepAllowlistRe    = regexp.MustCompile(`(?s)scheduled_task_oidc_audiences?\s*=\s*join\(",",.*?values\(local\.sweep_audiences\)`)
)

// httpSweepProblems cross-checks the module files (as returned by scanDir):
// one typ.sweep iterating local.active_sweeps whose uri ends in
// /api/scheduled/${each.key}, with active_sweeps derived from the enabled
// sweep_tasks entries.
func httpSweepProblems(files map[string]string, typ string) []string {
	all := make([]string, 0, len(files))
	for _, raw := range files {
		all = append(all, stripComments(raw))
	}
	src := strings.Join(all, "\n")
	body, ok := blockOnce(src, `resource\s+"`+typ+`"\s+"sweep"\s*\{`)
	if !ok {
		return []string{typ + ".sweep is missing or duplicated"}
	}
	var problems []string
	if !cronForEachRe.MatchString(body) {
		problems = append(problems, typ+".sweep lacks for_each = local.active_sweeps")
	}
	if !sweepURIRe.MatchString(body) {
		problems = append(problems, typ+`.sweep lacks uri = ".../api/scheduled/${each.key}"`)
	}
	if m := activeSweepsRe.FindStringSubmatch(src); m == nil || m[1] != m[3] || m[2] != m[4] || m[2] != m[5] {
		problems = append(problems, "active_sweeps is not { for k, v in local.sweep_tasks : k => v if v.enabled }")
	}
	if typ != "google_cloud_scheduler_job" {
		return problems
	}
	if !sweepAudienceRe.MatchString(body) {
		problems = append(problems, typ+".sweep lacks audience = local.sweep_audiences[each.key]")
	}
	if m := sweepAudiencesDefRe.FindStringSubmatch(src); m == nil || m[1] != m[2] || m[1] != m[3] {
		problems = append(problems, `sweep_audiences is not { for k, v in local.active_sweeps : k => ".../api/scheduled/${k}" }`)
	}
	if !sweepAllowlistRe.MatchString(src) {
		problems = append(problems, "the scheduled_task_oidc_audience(s) allowlist does not include values(local.sweep_audiences)")
	}
	return problems
}

func TestHTTPSweepCrossWiring(t *testing.T) {
	const active = "locals {\n  active_sweeps = var.enable_scheduled_tasks ? { for k, v in local.sweep_tasks : k => v if v.enabled } : {}\n}\n"
	const azure = "resource \"azurerm_logic_app_action_custom\" \"sweep\" {\n  for_each = local.active_sweeps\n  body = jsonencode({\n    inputs = {\n      uri = \"https://${azurerm_container_app.main.ingress[0].fqdn}/api/scheduled/${each.key}\"\n    }\n  })\n}\n"
	const gcpLocals = "locals {\n  sweep_audiences = { for k, v in local.active_sweeps : k => \"${var.service_name}/api/scheduled/${k}\" }\n  scheduled_task_oidc_audiences = join(\",\", compact(concat([\n    \"a\",\n  ], values(local.sweep_audiences))))\n}\n"
	const gcpJob = "resource \"google_cloud_scheduler_job\" \"sweep\" {\n  for_each = local.active_sweeps\n  http_target {\n    uri = \"${google_cloud_run_v2_service.main.uri}/api/scheduled/${each.key}\"\n    oidc_token {\n      audience = local.sweep_audiences[each.key]\n    }\n  }\n}\n"
	const gcp = active + gcpLocals + gcpJob
	const az, gc = "azurerm_logic_app_action_custom", "google_cloud_scheduler_job"
	for name, c := range map[string]struct {
		src, typ string
		wantProb bool
	}{
		"azure wired":                    {active + azure, az, false},
		"gcp wired":                      {gcp, gc, false},
		"missing resource":               {active, az, true},
		"duplicated resource":            {active + azure + azure, az, true},
		"resource under another name":    {active + strings.Replace(azure, `"sweep"`, `"tasks"`, 1), az, true},
		"not for_each active_sweeps":     {active + strings.Replace(azure, "local.active_sweeps", "local.sweep_tasks", 1), az, true},
		"literal task in uri":            {active + strings.Replace(azure, "${each.key}", "cleanup", 1), az, true},
		"task from each.value":           {active + strings.Replace(azure, "${each.key}", "${each.value.job}", 1), az, true},
		"wrong path":                     {active + strings.Replace(azure, "/api/scheduled/", "/api/", 1), az, true},
		"commented-out uri":              {active + strings.Replace(azure, "      uri", "      # uri", 1), az, true},
		"active_sweeps ignores enabled":  {strings.Replace(active, " if v.enabled", "", 1) + azure, az, true},
		"gcp audience not per key":       {strings.Replace(gcp, "local.sweep_audiences[each.key]", "local.aud", 1), gc, true},
		"gcp audiences from sweep_tasks": {strings.Replace(gcp, "in local.active_sweeps", "in local.sweep_tasks", 1), gc, true},
		"gcp audience not the task path": {strings.Replace(gcp, "/api/scheduled/${k}", "/api/scheduled/x", 1), gc, true},
		"gcp audience from another var":  {strings.Replace(gcp, "scheduled/${k}", "scheduled/${v}", 1), gc, true},
		"gcp allowlist misses sweeps":    {strings.Replace(gcp, "values(local.sweep_audiences)", "[]", 1), gc, true},
		"gcp allowlist in another local": {strings.Replace(gcp, "scheduled_task_oidc_audiences =", "other =", 1), gc, true},
	} {
		if got := httpSweepProblems(map[string]string{"a.tf": c.src}, c.typ); (len(got) > 0) != c.wantProb {
			t.Errorf("%s: problems = %v", name, got)
		}
	}
}
