// Coverage test for issue #712: every task in the server roster must be wired
// to a scheduler on every platform, or be listed in knownGaps with the reason
// it is not yet. The contract test next to it checks the other direction
// (every scheduler reference names a real task).
package compute_test

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/server"
)

// exemptTasks are roster tasks no platform has to schedule.
var exemptTasks = map[string]string{
	"analytics_refresh":   "analytics_collect already refreshes the views",
	"ri_exchange_reshape": "opt-in feature, off by default",
	"ladder_run":          "opt-in feature, off by default",
}

// optInRequired tasks must be wired but default to off (no live purchasing or
// retention change by default). Flipping cleanup to default-on later means
// removing it from this set.
var optInRequired = map[string]bool{
	"fire_scheduled_purchases":    true,
	"process_scheduled_purchases": true,
	"send_notifications":          true,
	"cleanup":                     true,
}

const dimTask = "missingTask"

var (
	validDims = []string{dimTask}
	allEight  = []string{"analytics_collect", "cleanup", "collect_recommendations", "finalize_revocations",
		"fire_scheduled_purchases", "process_scheduled_purchases", "reap_stuck_purchases", "send_notifications"}
	sevenGCP = []string{"analytics_collect", "cleanup", "finalize_revocations", "fire_scheduled_purchases",
		"process_scheduled_purchases", "reap_stuck_purchases", "send_notifications"}
)

// knownGaps documents what is not wired yet, per platform and dimension. Each
// PR that wires a task removes it here; the test fails when actual != documented.
// aws/cleanup-lambda and gcp/cleanup-function run cleanup outside the server
// task and are not scanned, so cleanup shows as a gap on AWS and GCP.
var knownGaps = map[string]map[string][]string{
	"aws-lambda": {dimTask: {"cleanup", "finalize_revocations", "process_scheduled_purchases", "send_notifications"}},
	"aws-fargate": {dimTask: {"analytics_collect", "cleanup", "finalize_revocations",
		"process_scheduled_purchases", "reap_stuck_purchases", "send_notifications"}},
	"cloudformation": {dimTask: {"analytics_collect", "cleanup", "finalize_revocations",
		"fire_scheduled_purchases", "reap_stuck_purchases", "send_notifications"}},
	"gcp-cloud-run": {dimTask: sevenGCP},
	"gcp-gke":       {dimTask: sevenGCP},
	"azure-container-apps": {dimTask: {"analytics_collect", "finalize_revocations", "fire_scheduled_purchases",
		"process_scheduled_purchases", "reap_stuck_purchases", "send_notifications"}},
	"azure-aks": {dimTask: allEight}, // no scheduler resources at all: #893
}

type platform struct{ name, dir string }

var platforms = []platform{
	{name: "aws-lambda", dir: "aws/lambda"},
	{name: "aws-fargate", dir: "aws/fargate"},
	{name: "cloudformation", dir: "../../../cloudformation/stacks/CUDly"},
	{name: "gcp-cloud-run", dir: "gcp/cloud-run"},
	{name: "gcp-gke", dir: "gcp/gke"},
	{name: "azure-container-apps", dir: "azure/container-apps"},
	{name: "azure-aks", dir: "azure/aks"},
}

// Copied from aws/cross_account_sts_discovery_test.go (commentLinePattern):
// whole-line `#` and `//` comments only. A /* */ pass would read the `/*` in
// an ARN glob or the `*/` in `0 */6 * * *` as a delimiter and swallow real
// declarations; the comment on the original explains the history.
var commentLinePattern = regexp.MustCompile(`(?m)^[ \t]*(?:#|//).*$`)

// stripComments blanks whole-line comments, then trailing `#` and `//`
// comments that start outside a quoted string (so `https://` and ARN globs in
// strings survive). Heredocs and quotes nested inside ${} are not handled.
func stripComments(s string) string {
	lines := strings.Split(commentLinePattern.ReplaceAllString(s, ""), "\n")
	for n, line := range lines {
		var quote byte
		for i := 0; i < len(line); i++ {
			switch c := line[i]; {
			case quote != 0 && c == '\\':
				i++
			case quote != 0:
				if c == quote {
					quote = 0
				}
			case c == '"' || c == '\'':
				quote = c
			case c == '#' || strings.HasPrefix(line[i:], "//"):
				lines[n] = line[:i]
				i = len(line)
			}
		}
	}
	return strings.Join(lines, "\n")
}

// schedulerHeaders match the resources that actually send a task to the
// server. A task name elsewhere (locals, env vars, permissions) is not a
// schedule. A literal anywhere in the body counts, even in a description.
// cfnRule scopes a CloudFormation AWS::Events::Rule resource.
var (
	schedulerHeaders = []string{
		`resource\s+"aws_cloudwatch_event_target"\s+"[^"]+"\s*\{`,
		`resource\s+"google_cloud_scheduler_job"\s+"[^"]+"\s*\{`,
		`resource\s+"azurerm_logic_app_action_custom"\s+"[^"]+"\s*\{`,
	}
	cfnRule = regexp.MustCompile(`(?s)Type:\s*AWS::Events::Rule\b.*?(?:\n  \S|\z)`)
	// Only a local's first-line value resolves, with no local-to-local recursion;
	// an unresolved local leaves the task unwired (an undocumented gap).
	localRef = regexp.MustCompile(`\blocal\.(\w+)`)
	// literalRes are the task-name patterns of the contract test (schedulerRefs)
	// plus the CloudFormation Input JSON.
	literalRes = func() []*regexp.Regexp {
		res := make([]*regexp.Regexp, 1, 1+len(schedulerRefs))
		res[0] = regexp.MustCompile(`"action":\s*"([^"]+)"`)
		for _, ref := range schedulerRefs {
			res = append(res, ref.re)
		}
		return res
	}()
)

// blocks returns the brace-balanced body of every match of header, a pattern
// that ends at the opening brace.
func blocks(src, header string) []string {
	var out []string
	for _, loc := range regexp.MustCompile(header).FindAllStringIndex(src, -1) {
		for i, depth := loc[1]-1, 0; i < len(src); i++ {
			if src[i] == '{' {
				depth++
			} else if src[i] == '}' {
				if depth--; depth == 0 {
					out = append(out, src[loc[1]:i])
					break
				}
			}
		}
	}
	return out
}

// schedulerScopes returns the text of each scheduler resource in src. A
// `local.x` reference inside one (a job whose uri is a local) is resolved by
// appending its definition from the locals blocks of all.
func schedulerScopes(src, all string) []string {
	var scopes []string
	locals := strings.Join(blocks(all, `\blocals\s*\{`), "\n")
	for _, h := range schedulerHeaders {
		for _, body := range blocks(src, h) {
			for _, ref := range localRef.FindAllStringSubmatch(body, -1) {
				if def := regexp.MustCompile(`(?m)^\s*` + ref[1] + `\s*=\s*(.+)$`).FindStringSubmatch(locals); def != nil {
					body += "\n" + def[1]
				}
			}
			scopes = append(scopes, body)
		}
	}
	return append(scopes, cfnRule.FindAllString(src, -1)...)
}

// scanDir reads the non-test *.tf files (or template.yaml) directly in dir.
// Non-recursive on purpose: that skips tests/ and .terraform/.
func scanDir(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	files := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || (filepath.Ext(e.Name()) != ".tf" && e.Name() != "template.yaml") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		files[e.Name()] = string(b)
	}
	return files, nil
}

// wiredFrom returns the tasks the files schedule, counting only names inside
// scheduler resources, and the names the server does not recognize.
func wiredFrom(files map[string]string) (wired map[string]bool, unknown []string) {
	wired = map[string]bool{}
	stripped := make([]string, 0, len(files))
	for _, raw := range files {
		stripped = append(stripped, stripComments(raw))
	}
	all := strings.Join(stripped, "\n")
	for _, src := range stripped {
		for _, scope := range schedulerScopes(src, all) {
			for _, re := range literalRes {
				for _, m := range re.FindAllStringSubmatch(scope, -1) {
					if _, err := server.ParseScheduledTaskType(m[1]); err != nil {
						unknown = append(unknown, m[1])
					} else {
						wired[m[1]] = true
					}
				}
			}
		}
	}
	return wired, unknown
}

func requiredTasks() []string {
	var req []string
	for _, n := range server.ScheduledTaskNames() {
		if _, exempt := exemptTasks[n]; !exempt {
			req = append(req, n)
		}
	}
	return req
}

// checkGaps compares the computed gap with the documented one.
func checkGaps(actual, documented []string) []string {
	var problems []string
	for _, a := range actual {
		if !slices.Contains(documented, a) {
			problems = append(problems, "undocumented gap: "+a)
		}
	}
	for _, d := range documented {
		switch {
		case !slices.Contains(server.ScheduledTaskNames(), d):
			problems = append(problems, "knownGaps names a task outside the roster: "+d)
		case !slices.Contains(actual, d):
			problems = append(problems, "knownGaps lists "+d+" but it is wired")
		}
	}
	return problems
}

func TestEveryServerTaskIsClassified(t *testing.T) {
	roster := server.ScheduledTaskNames()
	for name, reason := range exemptTasks {
		if !slices.Contains(roster, name) || reason == "" || optInRequired[name] {
			t.Errorf("exempt task %q must be in the roster, have a reason and not be opt-in required", name)
		}
	}
	for name := range optInRequired {
		if !slices.Contains(requiredTasks(), name) {
			t.Errorf("opt-in task %q is not a required roster task", name)
		}
	}
	for _, prob := range knownGapsProblems(knownGaps) {
		t.Error(prob)
	}
}

// knownGapsProblems rejects platform and dimension keys that do not exist.
func knownGapsProblems(gaps map[string]map[string][]string) []string {
	var problems []string
	for plat, dims := range gaps {
		if !slices.ContainsFunc(platforms, func(p platform) bool { return p.name == plat }) {
			problems = append(problems, "knownGaps names unknown platform "+plat)
		}
		for dim := range dims {
			if !slices.Contains(validDims, dim) {
				problems = append(problems, "knownGaps["+plat+"] has unknown dimension "+dim)
			}
		}
	}
	return problems
}

func TestRequiredTasksWiredPerPlatform(t *testing.T) {
	for _, p := range platforms {
		files, err := scanDir(p.dir)
		if err != nil {
			t.Errorf("%s: %v", p.name, err)
			continue
		}
		wired, unknown := wiredFrom(files)
		for _, u := range unknown {
			t.Errorf("%s: schedules task %q which the server does not recognize", p.name, u)
		}
		var missing []string
		for _, task := range requiredTasks() {
			if !wired[task] {
				missing = append(missing, task)
			}
		}
		for _, prob := range checkGaps(missing, knownGaps[p.name][dimTask]) {
			t.Errorf("%s: %s (missing now: %v)", p.name, prob, missing)
		}
	}
}

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestCheckerNegatives(t *testing.T) {
	const finalize = `input = jsonencode({ action = "finalize_revocations" })`
	const collectURI = `uri = "https://h/api/scheduled/collect_recommendations"`
	const aud = "locals {\n  aud = \"x/api/scheduled/collect_recommendations\"\n}\n"
	job := func(body string) string { return "resource \"google_cloud_scheduler_job\" \"j\" {\n" + body + "\n}\n" }
	target := func(body string) string { return "resource \"aws_cloudwatch_event_target\" \"t\" {\n" + body + "\n}\n" }
	cfn := func(typ string) string {
		return "  R:\n    Type: " + typ + "\n    Properties:\n      Input: '{\"action\": \"finalize_revocations\"}'\n  Other:\n    Type: X\n"
	}
	for name, c := range map[string]struct {
		src     string
		wired   []string
		unknown []string
	}{
		"https uri survives, whole-line comments do not": {job("# \"--task\", \"cleanup\"\n// /api/scheduled/cleanup\n" + collectURI), []string{"collect_recommendations"}, nil},
		"ARN glob and cron do not truncate":              {target("a = \"arn:aws:events:*:*:rule/*\"\n" + finalize + "\nb = \"cron(0 */6 * * ? *)\""), []string{"finalize_revocations"}, nil},
		"trailing comment does not count":                {job("description = \"x\" # " + collectURI + "\nb = 1 // " + collectURI), nil, nil},
		"escaped quote does not end the string":          {job("a = \"q\\\"\" # /api/scheduled/collect_recommendations \"x\""), nil, nil},
		"# inside single quotes is content":              {job("note = 'say # " + collectURI + "'"), []string{"collect_recommendations"}, nil},
		"trailing comment after a real uri":              {job(collectURI + " # note"), []string{"collect_recommendations"}, nil},
		"literal only in locals is not wired":            {aud + job(`uri = "x"`), nil, nil},
		"local name clash with a plain attribute":        {"resource \"x\" \"r\" {\n  name = \"x/api/scheduled/cleanup\"\n}\nlocals {\n  name = \"zzz\"\n}\n" + job("uri = local.name"), nil, nil},
		"job uri resolved through a local":               {aud + job("uri = local.aud"), []string{"collect_recommendations"}, nil},
		"literal outside a scheduler block":              {"resource \"aws_lambda_permission\" \"p\" {\n" + finalize + "\n}\n", nil, nil},
		"unknown task in a scheduler block":              {target(`input = jsonencode({ action = "bogus_task" })`), nil, []string{"bogus_task"}},
		"CloudFormation rule input":                      {cfn("AWS::Events::Rule"), []string{"finalize_revocations"}, nil},
		"CloudFormation literal outside a rule":          {cfn("AWS::Lambda::Function"), nil, nil},
	} {
		wired, unknown := wiredFrom(map[string]string{"a.tf": c.src})
		if !slices.Equal(slices.Sorted(maps.Keys(wired)), c.wired) || !slices.Equal(unknown, c.unknown) {
			t.Errorf("%s: wired = %v, unknown = %v", name, wired, unknown)
		}
	}
	files, err := scanDir(writeFiles(t, map[string]string{
		"tests/x.tftest.hcl": target(finalize), "x.tftest.hcl": target(finalize), ".terraform/m.tf": target(finalize), "notes.md": target(finalize), "dir.tf/y.tf": target(finalize),
	}))
	if wired, _ := wiredFrom(files); err != nil || len(wired) != 0 {
		t.Errorf("tests/, .terraform/, *.tftest.hcl and non-tf files must not count: wired = %v, err = %v", wired, err)
	}
	if len(knownGapsProblems(map[string]map[string][]string{"nope": {dimTask: nil}})) != 1 ||
		len(knownGapsProblems(map[string]map[string][]string{"aws-lambda": {"nope": nil}})) != 1 {
		t.Error("knownGapsProblems must reject unknown platforms and dimensions")
	}
	if len(checkGaps([]string{"cleanup"}, []string{"cleanup"})) != 0 || len(checkGaps(nil, []string{"cleanup"})) == 0 ||
		len(checkGaps(nil, []string{"no_such_task"})) == 0 || len(checkGaps([]string{"cleanup"}, nil)) == 0 {
		t.Error("checkGaps must flag undocumented, stale (wired) and non-roster entries only")
	}
}
