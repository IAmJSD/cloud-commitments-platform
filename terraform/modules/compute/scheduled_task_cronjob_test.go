// AKS checks for issue #893: the AKS module schedules its sweep_tasks through a
// single for_each Kubernetes CronJob. wiredFrom counts every sweep_tasks key as
// wired; cronJobProblems checks that a CronJob actually runs each enabled key
// through the server's --task entry point.
package compute_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const cronJobFile = "scheduled-tasks.tf"

var (
	cronForEachRe = regexp.MustCompile(`(?m)^\s*for_each\s*=\s*local\.active_sweeps\s*$`)
	cronCommandRe = regexp.MustCompile(`(?m)^\s*command\s*=\s*\[\s*"/app/cudly"\s*,\s*"--task"\s*,\s*each\.key\s*\]\s*$`)
	// active_sweeps must keep the enabled entries of sweep_tasks, keyed by the
	// task name (any outer gate, such as deploy_kubernetes_resources, is fine).
	activeSweepsRe = regexp.MustCompile(`(?m)^\s*active_sweeps\s*=.*\{\s*for\s+(\w+)\s*,\s*(\w+)\s+in\s+local\.sweep_tasks\s*:\s*(\w+)\s*=>\s*(\w+)\s+if\s+(\w+)\.enabled\s*\}`)
)

// cronJobProblems cross-checks dir/scheduled-tasks.tf: one CronJob iterating
// local.active_sweeps, running `/app/cudly --task <key>`, with active_sweeps
// derived from the enabled sweep_tasks entries.
func cronJobProblems(dir string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, cronJobFile))
	if err != nil {
		return nil, err
	}
	src := stripComments(string(raw))
	body, ok := blockOnce(src, `resource\s+"kubernetes_cron_job_v1"\s+"sweep"\s*\{`)
	if !ok {
		return []string{"kubernetes_cron_job_v1.sweep is missing or duplicated"}, nil
	}
	var problems []string
	if !cronForEachRe.MatchString(body) {
		problems = append(problems, "kubernetes_cron_job_v1.sweep lacks for_each = local.active_sweeps")
	}
	if !cronCommandRe.MatchString(body) {
		problems = append(problems, `kubernetes_cron_job_v1.sweep lacks command = ["/app/cudly", "--task", each.key]`)
	}
	if m := activeSweepsRe.FindStringSubmatch(src); m == nil || m[1] != m[3] || m[2] != m[4] || m[2] != m[5] {
		problems = append(problems, "active_sweeps is not { for k, v in local.sweep_tasks : k => v if v.enabled }")
	}
	return problems, nil
}

func TestCronJobCrossWiring(t *testing.T) {
	const active = "locals {\n  active_sweeps = var.deploy_kubernetes_resources ? { for k, v in local.sweep_tasks : k => v if v.enabled } : {}\n}\n"
	const job = "resource \"kubernetes_cron_job_v1\" \"sweep\" {\n  for_each = local.active_sweeps\n  spec {\n    container {\n      command = [\"/app/cudly\", \"--task\", each.key]\n    }\n  }\n}\n"
	for name, c := range map[string]struct {
		src      string
		wantProb bool
	}{
		"wired":                         {active + job, false},
		"ungated active_sweeps":         {strings.Replace(active, "var.deploy_kubernetes_resources ? ", "", 1) + strings.Replace(job, " : {}", "", 1), false},
		"missing CronJob":               {active, true},
		"duplicated CronJob":            {active + job + job, true},
		"CronJob under another name":    {active + strings.Replace(job, `"sweep"`, `"tasks"`, 1), true},
		"not for_each active_sweeps":    {active + strings.Replace(job, "local.active_sweeps", "local.sweep_tasks", 1), true},
		"literal task name":             {active + strings.Replace(job, "each.key", `"cleanup"`, 1), true},
		"task from each.value":          {active + strings.Replace(job, "each.key", "each.value.job", 1), true},
		"wrong binary":                  {active + strings.Replace(job, "/app/cudly", "/entrypoint.sh", 1), true},
		"commented-out command":         {active + strings.Replace(job, "      command", "      # command", 1), true},
		"active_sweeps ignores enabled": {strings.Replace(active, " if v.enabled", "", 1) + job, true},
		"active_sweeps filters a field": {strings.Replace(active, "if v.enabled", "if v.job", 1) + job, true},
		"active_sweeps from other map":  {strings.Replace(active, "local.sweep_tasks", "local.other", 1) + job, true},
		"active_sweeps renames the key": {strings.Replace(active, "k => v", "v.job => v", 1) + job, true},
	} {
		got, err := cronJobProblems(writeFiles(t, map[string]string{cronJobFile: c.src}))
		if err != nil || (len(got) > 0) != c.wantProb {
			t.Errorf("%s: problems = %v, err = %v", name, got, err)
		}
	}
	if _, err := cronJobProblems(t.TempDir()); err == nil {
		t.Error("a missing scheduled-tasks.tf must be an error")
	}
}
