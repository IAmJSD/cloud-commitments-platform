// AWS env-layer checks for issue #712: the Lambda and Fargate modules' task
// variables must be declared, passed name-identically by
// terraform/environments/aws/compute.tf and defaulted per the opt-in policy,
// and no tfvars file may switch off a default-on task.
package compute_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/server"
)

type taskVar struct{ enable, schedule string }

// taskVars names the module/env variables that switch each task and set its
// cadence; the AWS env checks use it instead of inferring names.
var taskVars = map[string]taskVar{
	"collect_recommendations":     {"enable_scheduled_tasks", "recommendation_schedule"},
	"reap_stuck_purchases":        {"enable_reap_stuck_purchases_schedule", "reap_stuck_purchases_schedule"},
	"analytics_collect":           {"enable_analytics_collect_schedule", "analytics_collect_schedule"},
	"fire_scheduled_purchases":    {"enable_fire_scheduled_purchases_schedule", "fire_scheduled_purchases_schedule"},
	"ladder_run":                  {"enable_ladder_run_schedule", "ladder_run_schedule"},
	"ri_exchange_reshape":         {"enable_ri_exchange_schedule", "ri_exchange_schedule"},
	"process_scheduled_purchases": {"enable_process_scheduled_purchases_schedule", "process_scheduled_purchases_schedule"},
	"send_notifications":          {"enable_send_notifications_schedule", "send_notifications_schedule"},
	"finalize_revocations":        {"enable_finalize_revocations_schedule", "finalize_revocations_schedule"},
	"cleanup":                     {"enable_cleanup_schedule", "cleanup_schedule"},
}

const awsEnvDir = "../../environments/aws"

var (
	passLineRe  = regexp.MustCompile(`(?m)^\s*(\w+)\s*=\s*var\.(\w+)\s*$`)
	tfvarsOffRe = regexp.MustCompile(`(?m)^\s*(enable_\w+)\s*=\s*false\b`)
	defaultRe   = regexp.MustCompile(`(?m)^\s*default\s*=\s*(\S+)`)
)

func defaultOn(task string) bool {
	_, exempt := exemptTasks[task]
	return !exempt && !optInRequired[task]
}

// envDefaultProblem checks an enable var's default against the opt-in policy.
func envDefaultProblem(task, def string) string {
	if want := defaultOn(task); (def == "true") != want {
		return fmt.Sprintf("%s: enable var defaults to %s, policy wants default-on=%v", task, def, want)
	}
	return ""
}

// blockOnce returns the body of the single block matching header; zero or
// several matches report not-found so a duplicate cannot be silently ignored.
func blockOnce(src, header string) (string, bool) {
	if found := blocks(src, header); len(found) == 1 {
		return found[0], true
	}
	return "", false
}

// envProblems checks, for the wired tasks of one AWS module, that the module
// declares each task's variables, that the env layer's module block passes
// them name-identically, and that the env enable defaults follow the opt-in
// policy. Variables the env layer lacks are returned as gaps, not problems.
func envProblems(modVars, envVars, compute, blockName string, wired map[string]bool) (problems []string, gaps map[string][]string) {
	modVars, envVars = stripComments(modVars), stripComments(envVars)
	gaps = map[string][]string{}
	pass, ok := blockOnce(stripComments(compute), `module\s+"`+blockName+`"\s*\{`)
	if !ok {
		return []string{"module block " + blockName + " missing or duplicated in compute.tf"}, gaps
	}
	passed := map[string]string{}
	for _, m := range passLineRe.FindAllStringSubmatch(pass, -1) {
		passed[m[1]] = m[2]
	}
	for _, task := range server.ScheduledTaskNames() {
		if !wired[task] {
			continue
		}
		tv := taskVars[task]
		for _, v := range []string{tv.enable, tv.schedule} {
			if !strings.Contains(modVars, `variable "`+v+`"`) {
				problems = append(problems, "module variables.tf does not declare "+v)
			}
			if rhs, ok := passed[v]; !ok {
				gaps[dimPass] = append(gaps[dimPass], task)
			} else if rhs != v {
				problems = append(problems, fmt.Sprintf("compute.tf passes %s = var.%s; must be name-identical", v, rhs))
			}
		}
		body, declared := blockOnce(envVars, `variable\s+"`+tv.enable+`"\s*\{`)
		_, schedDeclared := blockOnce(envVars, `variable\s+"`+tv.schedule+`"\s*\{`)
		if m := defaultRe.FindStringSubmatch(body); !declared || !schedDeclared {
			gaps[dimDefault] = append(gaps[dimDefault], task)
		} else if m == nil {
			problems = append(problems, "env variable "+tv.enable+" has no default")
		} else if prob := envDefaultProblem(task, m[1]); prob != "" {
			problems = append(problems, prob)
		}
	}
	return problems, gaps
}

// tfvarsProblems returns the default-on tasks a tfvars file switches off.
// JSON tfvars are rejected rather than silently skipped.
func tfvarsProblems(name, src string) []string {
	if filepath.Ext(name) == ".json" {
		return []string{name + ": JSON tfvars not supported by this check"}
	}
	var bad []string
	for _, m := range tfvarsOffRe.FindAllStringSubmatch(src, -1) {
		for task, v := range taskVars {
			if v.enable == m[1] && defaultOn(task) {
				bad = append(bad, name+" disables default-on task "+task)
			}
		}
	}
	return bad
}

// sweepProblems cross-checks dir/sweeps.tf: each sweep target must point at
// the rule with the same key, and (Lambda) so must its invoke permission. An
// absent file is fine; an existing one must have the blocks.
func sweepProblems(dir string, needPermission bool) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "sweeps.tf"))
	if os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	src := stripComments(string(raw))
	var problems []string
	check := func(typ, attr, ref string) {
		body, ok := blockOnce(src, `resource\s+"`+typ+`"\s+"sweep"\s*\{`)
		want := regexp.MustCompile(`(?m)^\s*` + attr + `\s*=\s*aws_cloudwatch_event_rule\.sweep\[each\.key\]\.` + ref + `\b`)
		if !ok || !want.MatchString(body) {
			problems = append(problems, typ+".sweep is missing or lacks "+attr+" = aws_cloudwatch_event_rule.sweep[each.key]."+ref)
		}
	}
	check("aws_cloudwatch_event_target", "rule", "name")
	if needPermission {
		check("aws_lambda_permission", "source_arn", "arn")
	}
	return problems, nil
}

func TestAWSEnvWiring(t *testing.T) {
	envVars, err := os.ReadFile(awsEnvDir + "/variables.tf")
	if err != nil {
		t.Fatal(err)
	}
	compute, err := os.ReadFile(awsEnvDir + "/compute.tf")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range platforms {
		if p.aws != strings.HasPrefix(p.name, "aws-") {
			t.Errorf("%s: aws flag does not match the platform name", p.name)
		}
		if !p.aws {
			continue
		}
		if p.envBlock == "" {
			t.Errorf("%s: aws platform has no envBlock", p.name)
			continue
		}
		files, err := scanDir(p.dir)
		if err != nil {
			t.Errorf("%s: %v", p.name, err)
			continue
		}
		wired, _ := wiredFrom(files)
		problems, gaps := envProblems(files["variables.tf"], string(envVars), string(compute), p.envBlock, wired)
		for _, prob := range problems {
			t.Errorf("%s: %s", p.name, prob)
		}
		for _, dim := range []string{dimPass, dimDefault} {
			for _, prob := range checkGaps(gaps[dim], knownGaps[p.name][dim]) {
				t.Errorf("%s %s: %s", p.name, dim, prob)
			}
		}
	}
}

func TestNoTfvarsDisablesDefaultOnTask(t *testing.T) {
	paths, err := filepath.Glob(awsEnvDir + "/*.tfvars*")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no tfvars files found: %v", err)
	}
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, prob := range tfvarsProblems(path, string(b)) {
			t.Error(prob)
		}
	}
}

// envFixture builds minimal module variables, env variables and compute.tf
// text that satisfy envProblems for one task with the given enable default.
func envFixture(task, def string) (mod, env, compute string) {
	tv := taskVars[task]
	mod = fmt.Sprintf("variable %q {}\nvariable %q {}\n", tv.enable, tv.schedule)
	env = fmt.Sprintf("variable %q {\n  default = %s\n}\nvariable %q {\n  default = \"x\"\n}\n", tv.enable, def, tv.schedule)
	compute = fmt.Sprintf("module \"m\" {\n  %[1]s = var.%[1]s\n  %[2]s = var.%[2]s\n}\n", tv.enable, tv.schedule)
	return mod, env, compute
}

func TestEnvChecksNegatives(t *testing.T) {
	const cr, fire = "collect_recommendations", "fire_scheduled_purchases"
	type fixture struct {
		task, def  string
		edit       func(mod, env, compute string) (string, string, string)
		wantProb   bool
		wantGapDim string
	}
	same := func(m, e, c string) (string, string, string) { return m, e, c }
	for name, f := range map[string]fixture{
		"clean":                          {cr, "true", same, false, ""},
		"opt-in task opt-in":             {fire, "false", same, false, ""},
		"opt-in task defaulting true":    {fire, "true", same, true, ""},
		"default-on task defaulting off": {cr, "false", same, true, ""},
		"pass-through not name-identical": {cr, "true", func(m, e, c string) (string, string, string) {
			return m, e, strings.Replace(c, "= var.recommendation_schedule", "= var.other", 1)
		}, true, ""},
		"undeclared module variable": {cr, "true", func(m, e, c string) (string, string, string) {
			return strings.Replace(m, `variable "recommendation_schedule" {}`, "", 1), e, c
		}, true, ""},
		"commented-out module declaration": {cr, "true", func(m, e, c string) (string, string, string) {
			return "# " + strings.Replace(m, "\n", "\n# ", 1), e, c
		}, true, ""},
		"schedule var missing in env": {cr, "true", func(m, e, c string) (string, string, string) {
			return m, e[:strings.Index(e, `variable "recommendation_schedule"`)], c
		}, false, dimDefault},
		"commented-out env declaration": {cr, "true", func(m, e, c string) (string, string, string) {
			return m, strings.Replace(e, `variable "recommendation_schedule" {`, `# variable "recommendation_schedule" {`, 1), c
		}, false, dimDefault},
		"ternary is not a name-identical pass-through": {cr, "true", func(m, e, c string) (string, string, string) {
			return m, e, strings.Replace(c, "= var.recommendation_schedule", "= var.recommendation_schedule ? 1 : 0", 1)
		}, false, dimPass},
		"duplicated module block": {cr, "true", func(m, e, c string) (string, string, string) {
			return m, e, c + c
		}, true, ""},
		"not passed by the module block": {cr, "true", func(m, e, c string) (string, string, string) {
			return m, e, strings.Replace(c, "recommendation_schedule = var.recommendation_schedule", "", 1)
		}, false, dimPass},
	} {
		m, e, c := f.edit(envFixture(f.task, f.def))
		problems, gaps := envProblems(m, e, c, "m", map[string]bool{f.task: true})
		wantGaps := 0
		if f.wantGapDim != "" {
			wantGaps = 1
		}
		if (len(problems) > 0) != f.wantProb || len(gaps) != wantGaps || (wantGaps == 1 && !slices.Equal(gaps[f.wantGapDim], []string{f.task})) {
			t.Errorf("%s: problems = %v, gaps = %v", name, problems, gaps)
		}
	}
	if p, _ := envProblems("", "", "", "m", map[string]bool{cr: true}); len(p) == 0 {
		t.Error("missing module block must be a problem")
	}
	if envDefaultProblem("finalize_revocations", "false") == "" || envDefaultProblem("cleanup", "true") == "" ||
		envDefaultProblem("finalize_revocations", "true") != "" || envDefaultProblem("cleanup", "false") != "" {
		t.Error("default-on / opt-in policy not enforced")
	}
	if got := tfvarsProblems("a.tfvars", "enable_finalize_revocations_schedule = false\nenable_fire_scheduled_purchases_schedule = false\n# enable_scheduled_tasks = false\n"); len(got) != 1 || !strings.Contains(got[0], "finalize_revocations") {
		t.Errorf("tfvarsProblems = %v", got)
	}
	if len(tfvarsProblems("a.tfvars.json", "{}")) != 1 {
		t.Error("JSON tfvars must fail loudly")
	}
}

func TestSweepCrossWiring(t *testing.T) {
	const target = "resource \"aws_cloudwatch_event_target\" \"sweep\" {\n  rule%s= aws_cloudwatch_event_rule.sweep[%s].name\n}\n"
	const perm = "resource \"aws_lambda_permission\" \"sweep\" {\n  source_arn%s= aws_cloudwatch_event_rule.sweep[%s].arn\n}\n"
	good, goodPerm := fmt.Sprintf(target, " ", "each.key"), fmt.Sprintf(perm, " ", "each.key")
	for name, c := range map[string]struct {
		src      string
		lambda   bool
		wantProb bool
	}{
		"aligned":               {fmt.Sprintf(target, "      ", "each.key") + fmt.Sprintf(perm, "   ", "each.key"), true, false},
		"unaligned":             {good + goodPerm, true, false},
		"fargate needs no perm": {good, false, false},
		"wrong key":             {fmt.Sprintf(target, " ", "each.value.rule") + goodPerm, true, true},
		"wrong resource type":   {strings.Replace(good, "event_rule", "event_target", 1) + goodPerm, true, true},
		"missing permission":    {good, true, true},
		"missing target":        {goodPerm, true, true},
	} {
		got, err := sweepProblems(writeFiles(t, map[string]string{"sweeps.tf": c.src}), c.lambda)
		if err != nil || (len(got) > 0) != c.wantProb {
			t.Errorf("%s: problems = %v, err = %v", name, got, err)
		}
	}
	if got, err := sweepProblems(t.TempDir(), true); err != nil || got != nil {
		t.Errorf("absent sweeps.tf must pass: problems = %v, err = %v", got, err)
	}
}
