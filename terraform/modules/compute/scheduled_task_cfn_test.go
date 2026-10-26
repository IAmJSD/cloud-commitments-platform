// CloudFormation checks for issue #712: the CUDly stack must schedule every
// required server task with one EventBridge rule that invokes the stack's
// Lambda with {"action": "<task>"}, an invoke permission gated exactly like
// its rule, and an enable parameter whose default follows the opt-in policy
// of the coverage test. The coverage test only finds the task names; this
// one parses the template.
package compute_test

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/server"
)

const (
	cfnTemplatePath  = "../../../cloudformation/stacks/CUDly/template.yaml"
	lambdaModuleVars = "aws/lambda/variables.tf"
	// cfnLongestSuffix is the name suffix of the oldest rule. New rules must not
	// be longer, so they fit the 64-char EventBridge limit for every stack name
	// that rule already accepts.
	cfnLongestSuffix = "-ScheduledExecution"
)

// cfnAlwaysOn are the rules that predate #712 and have no enable parameter.
// Turning them into opt-in switches would change running stacks, so they stay.
var cfnAlwaysOn = map[string]bool{
	"collect_recommendations":     true,
	"process_scheduled_purchases": true,
}

// cfnDefaultExceptions are tasks whose CloudFormation default is the opposite
// of the opt-in policy, with the reason.
var cfnDefaultExceptions = map[string]string{
	"process_scheduled_purchases": "the daily PurchaseCheck rule predates the opt-in policy; removing it would stop purchases on existing stacks",
	"reap_stuck_purchases": "PurchaseCheck runs process only daily, so the reaper's 1h20m approved floor (#894) would still fail " +
		"stranded approvals before process re-drives them",
}

// cfnStack is a parsed template: Resources, Parameters and Conditions by
// logical id. Short-form tags are rewritten to their long form (!Ref to Ref,
// !X to Fn::X, !GetAtt "A.B" to Fn::GetAtt [A, B]).
type cfnStack struct {
	Resources, Parameters, Conditions map[string]map[string]any
}

func cfnValue(n *yaml.Node) any {
	var v any
	switch n.Kind {
	case yaml.DocumentNode:
		return cfnValue(n.Content[0])
	case yaml.AliasNode:
		return cfnValue(n.Alias)
	case yaml.MappingNode:
		m := map[string]any{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			m[n.Content[i].Value] = cfnValue(n.Content[i+1])
		}
		v = m
	case yaml.SequenceNode:
		s := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			s = append(s, cfnValue(c))
		}
		v = s
	default:
		v = n.Value
	}
	if !strings.HasPrefix(n.Tag, "!") || strings.HasPrefix(n.Tag, "!!") {
		return v
	}
	if n.Tag == "!Ref" {
		return map[string]any{"Ref": v}
	}
	return map[string]any{"Fn::" + n.Tag[1:]: v}
}

func parseCFN(src []byte) (*cfnStack, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(src, &root); err != nil {
		return nil, err
	}
	top, ok := cfnValue(&root).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("template is not a mapping")
	}
	section := func(name string) map[string]map[string]any {
		out := map[string]map[string]any{}
		raw, _ := top[name].(map[string]any)
		for id, v := range raw {
			out[id], _ = v.(map[string]any)
		}
		return out
	}
	return &cfnStack{section("Resources"), section("Parameters"), section("Conditions")}, nil
}

func props(res map[string]any) map[string]any {
	p, _ := res["Properties"].(map[string]any)
	return p
}

// ref returns the logical id of {Ref: id}.
func ref(v any) string {
	m, _ := v.(map[string]any)
	s, _ := m["Ref"].(string)
	return s
}

// getAtt returns "Id.Attr" for both GetAtt forms.
func getAtt(v any) string {
	m, _ := v.(map[string]any)
	switch a := m["Fn::GetAtt"].(type) {
	case string:
		return a
	case []any:
		parts := make([]string, 0, len(a))
		for _, p := range a {
			s, _ := p.(string)
			parts = append(parts, s)
		}
		return strings.Join(parts, ".")
	}
	return ""
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// cfnRuleTask returns the task a rule's single target sends, or a problem.
func cfnRuleTask(id string, p map[string]any) (string, string) {
	targets, _ := p["Targets"].([]any)
	if len(targets) != 1 {
		return "", fmt.Sprintf("%s: want exactly one target, got %d", id, len(targets))
	}
	t, _ := targets[0].(map[string]any)
	if getAtt(t["Arn"]) != "LambdaFunction.Arn" {
		return "", id + ": target is not LambdaFunction.Arn"
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(str(t["Input"])), &input); err != nil || len(input) != 1 || str(input["action"]) == "" {
		return "", fmt.Sprintf("%s: target Input must be exactly {\"action\": <task>}, got %q", id, str(t["Input"]))
	}
	return str(input["action"]), ""
}

var cfnNameRe = regexp.MustCompile(`^\$\{AWS::StackName\}(-[A-Za-z]+)$`)

// cfnRules maps each scheduled task to the logical ids of its rules and
// reports malformed rules.
func cfnRules(s *cfnStack) (map[string][]string, []string) {
	tasks := map[string][]string{}
	var problems []string
	names := map[string]string{}
	for id, res := range s.Resources {
		if res["Type"] != "AWS::Events::Rule" {
			continue
		}
		p := props(res)
		task, prob := cfnRuleTask(id, p)
		if prob != "" {
			problems = append(problems, prob)
			continue
		}
		tasks[task] = append(tasks[task], id)
		name, _ := p["Name"].(map[string]any)
		m := cfnNameRe.FindStringSubmatch(str(name["Fn::Sub"]))
		switch {
		case m == nil:
			problems = append(problems, id+`: Name must be !Sub "${AWS::StackName}-<Suffix>"`)
		case len(m[1]) > len(cfnLongestSuffix):
			problems = append(problems, fmt.Sprintf("%s: name suffix %s is longer than %s", id, m[1], cfnLongestSuffix))
		case names[m[1]] != "":
			problems = append(problems, id+": name suffix "+m[1]+" also used by "+names[m[1]])
		default:
			names[m[1]] = id
		}
		if p["State"] != "ENABLED" {
			problems = append(problems, id+": State must be ENABLED; the enable parameter switches it")
		}
	}
	return tasks, problems
}

// cfnPermissionProblems checks that exactly one invoke permission points at
// the rule and that it carries the rule's condition.
func cfnPermissionProblems(s *cfnStack, ruleID string) []string {
	var found []string
	for id, res := range s.Resources {
		if res["Type"] == "AWS::Lambda::Permission" && getAtt(props(res)["SourceArn"]) == ruleID+".Arn" {
			found = append(found, id)
		}
	}
	if len(found) != 1 {
		return []string{fmt.Sprintf("%s: want exactly one Lambda permission with SourceArn %s.Arn, got %v", ruleID, ruleID, found)}
	}
	perm := s.Resources[found[0]]
	p := props(perm)
	var problems []string
	if p["Action"] != "lambda:InvokeFunction" || p["Principal"] != "events.amazonaws.com" || ref(p["FunctionName"]) != "LambdaFunction" {
		problems = append(problems, found[0]+": must grant lambda:InvokeFunction on LambdaFunction to events.amazonaws.com")
	}
	if perm["Condition"] != s.Resources[ruleID]["Condition"] {
		problems = append(problems, fmt.Sprintf("%s: Condition %v differs from its rule's %v", found[0], perm["Condition"], s.Resources[ruleID]["Condition"]))
	}
	return problems
}

// cfnEnableDefault resolves a rule's condition to its enable parameter's
// default: Fn::Equals [Ref: <param>, "true"] on a "true"/"false" parameter.
func cfnEnableDefault(s *cfnStack, ruleID, cond string) (bool, string) {
	eq, _ := s.Conditions[cond]["Fn::Equals"].([]any)
	if len(eq) != 2 || eq[1] != "true" || ref(eq[0]) == "" {
		return false, fmt.Sprintf("%s: condition %s must be Fn::Equals [Ref: <param>, \"true\"]", ruleID, cond)
	}
	param := s.Parameters[ref(eq[0])]
	allowed, _ := param["AllowedValues"].([]any)
	if param == nil || !slices.Equal(allowed, []any{"true", "false"}) {
		return false, fmt.Sprintf("%s: parameter %s must allow exactly \"true\" and \"false\"", ruleID, ref(eq[0]))
	}
	def := str(param["Default"])
	if def != "true" && def != "false" {
		return false, fmt.Sprintf("%s: parameter %s default %q is not \"true\" or \"false\"", ruleID, ref(eq[0]), def)
	}
	return def == "true", ""
}

var tfStringDefaultRe = regexp.MustCompile(`(?m)^\s*default\s*=\s*"([^"]*)"`)

// cfnScheduleProblem checks that a switchable rule's schedule is a parameter
// whose default matches the Lambda module's default for the same task.
func cfnScheduleProblem(s *cfnStack, ruleID, task, lambdaVars string) string {
	param := ref(props(s.Resources[ruleID])["ScheduleExpression"])
	if param == "" || s.Parameters[param] == nil {
		return ruleID + ": ScheduleExpression must be a Ref to a stack parameter"
	}
	body, ok := blockOnce(stripComments(lambdaVars), `variable\s+"`+taskVars[task].schedule+`"\s*\{`)
	m := tfStringDefaultRe.FindStringSubmatch(body)
	if !ok || m == nil {
		return ruleID + ": Lambda module has no string default for " + taskVars[task].schedule
	}
	if got := str(s.Parameters[param]["Default"]); got != m[1] {
		return fmt.Sprintf("%s: %s defaults to %q, the Lambda module's %s to %q", ruleID, param, got, taskVars[task].schedule, m[1])
	}
	return ""
}

// cfnTaskProblems checks one required task's rule, permission, default and
// schedule.
func cfnTaskProblems(s *cfnStack, task string, ruleIDs []string, lambdaVars string) []string {
	if len(ruleIDs) != 1 {
		return []string{fmt.Sprintf("%s: want exactly one rule, got %v", task, ruleIDs)}
	}
	id := ruleIDs[0]
	problems := cfnPermissionProblems(s, id)
	cond := str(s.Resources[id]["Condition"])
	if (cond == "") != cfnAlwaysOn[task] {
		return append(problems, fmt.Sprintf("%s: has condition %q; only the pre-existing rules %v may be unconditional", id, cond, cfnAlwaysOn))
	}
	on := true
	if cond != "" {
		var prob string
		if on, prob = cfnEnableDefault(s, id, cond); prob != "" {
			return append(problems, prob)
		}
		if prob := cfnScheduleProblem(s, id, task, lambdaVars); prob != "" {
			problems = append(problems, prob)
		}
	}
	_, exception := cfnDefaultExceptions[task]
	if want := defaultOn(task) != exception; on != want {
		problems = append(problems, fmt.Sprintf("%s: on by default = %v, want %v (opt-in policy, exception %v)", task, on, want, exception))
	}
	return problems
}

// cfnProblems runs every check on a parsed template.
func cfnProblems(s *cfnStack, lambdaVars string) []string {
	tasks, problems := cfnRules(s)
	for task := range tasks {
		if _, err := server.ParseScheduledTaskType(task); err != nil {
			problems = append(problems, "rule schedules unknown task "+task)
		}
	}
	for _, task := range requiredTasks() {
		if slices.Contains(knownGaps["cloudformation"][dimTask], task) {
			continue // the coverage test holds it to the documented gap
		}
		problems = append(problems, cfnTaskProblems(s, task, tasks[task], lambdaVars)...)
	}
	return problems
}

func readCFN(t *testing.T) (tpl, lambdaVars string) {
	t.Helper()
	a, err := os.ReadFile(cfnTemplatePath)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(lambdaModuleVars)
	if err != nil {
		t.Fatal(err)
	}
	return string(a), string(b)
}

func TestCloudFormationSchedulesEveryRequiredTask(t *testing.T) {
	for task, reason := range cfnDefaultExceptions {
		if !slices.Contains(requiredTasks(), task) || reason == "" {
			t.Errorf("cfnDefaultExceptions[%s] must name a required task and give a reason", task)
		}
	}
	tpl, lambdaVars := readCFN(t)
	s, err := parseCFN([]byte(tpl))
	if err != nil {
		t.Fatal(err)
	}
	for _, prob := range cfnProblems(s, lambdaVars) {
		t.Error(prob)
	}
}

func TestCloudFormationScheduleCheckNegatives(t *testing.T) {
	tpl, lambdaVars := readCFN(t)
	for name, edit := range map[string]func(string) string{
		"rule removed": func(s string) string {
			return strings.Replace(s, `Input: '{"action": "finalize_revocations"}'`, `Input: '{"action": "analytics_collect"}'`, 1)
		},
		"opt-in task on by default": func(s string) string {
			return regexp.MustCompile(`(EnableFireScheduledPurchasesSchedule:\n    Type: String\n    Default: )"false"`).ReplaceAllString(s, `$1"true"`)
		},
		"default-on task off by default": func(s string) string {
			return regexp.MustCompile(`(EnableFinalizeRevocationsSchedule:\n    Type: String\n    Default: )"true"`).ReplaceAllString(s, `$1"false"`)
		},
		"exception flipped to policy": func(s string) string {
			return regexp.MustCompile(`(EnableReapStuckPurchasesSchedule:\n    Type: String\n    Default: )"false"`).ReplaceAllString(s, `$1"true"`)
		},
		"permission points at another rule": func(s string) string {
			return strings.Replace(s, "SourceArn: !GetAtt SendNotificationsRule.Arn", "SourceArn: !GetAtt FinalizeRevocationsRule.Arn", 1)
		},
		"permission ungated": func(s string) string {
			return strings.Replace(s, "Type: AWS::Lambda::Permission\n    Condition: ScheduleAnalyticsCollect\n", "Type: AWS::Lambda::Permission\n", 1)
		},
		"rule ungated": func(s string) string {
			return strings.Replace(s, "    Condition: ScheduleReapStuckPurchases\n    Properties:\n      Name", "    Properties:\n      Name", 1)
		},
		"name too long": func(s string) string { return strings.Replace(s, `-ReapStuck"`, `-ReapStuckPurchasesNow"`, 1) },
		"name reused":   func(s string) string { return strings.Replace(s, `-ReapStuck"`, `-Finalize"`, 1) },
		"literal schedule": func(s string) string {
			return strings.Replace(s, "!Ref FinalizeRevocationsSchedule", `"rate(1 hour)"`, 1)
		},
		"schedule drift": func(s string) string {
			return strings.Replace(s, `Default: "rate(15 minutes)"`, `Default: "rate(1 minute)"`, 1)
		},
		"wrong target": func(s string) string {
			return strings.Replace(s, "Arn: !GetAtt LambdaFunction.Arn\n          Input: '{\"action\": \"send_", "Arn: !Ref NotificationTopic\n          Input: '{\"action\": \"send_", 1)
		},
		"extra input key": func(s string) string {
			return strings.Replace(s, `{"action": "analytics_collect"}`, `{"action": "analytics_collect", "x": 1}`, 1)
		},
		"rule disabled": func(s string) string {
			return strings.Replace(s, "ScheduleExpression: !Ref SendNotificationsSchedule\n      State: ENABLED", "ScheduleExpression: !Ref SendNotificationsSchedule\n      State: DISABLED", 1)
		},
		"unknown task": func(s string) string {
			return strings.Replace(s, `{"action": "fire_scheduled_purchases"}`, `{"action": "fire_purchases"}`, 1)
		},
		"loose enable param": func(s string) string {
			return strings.Replace(s, "      - Ref: EnableSendNotificationsSchedule\n      - \"true\"", "      - Ref: EnableSendNotificationsSchedule\n      - \"yes\"", 1)
		},
		"legacy rule switched": func(s string) string {
			return strings.Replace(s, "  PurchaseCheckRule:\n    Type: AWS::Events::Rule\n", "  PurchaseCheckRule:\n    Type: AWS::Events::Rule\n    Condition: ScheduleSendNotifications\n", 1)
		},
	} {
		mutated := edit(tpl)
		if mutated == tpl {
			t.Errorf("%s: edit did not apply; update the fixture", name)
			continue
		}
		s, err := parseCFN([]byte(mutated))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(cfnProblems(s, lambdaVars)) == 0 {
			t.Errorf("%s: no problem reported", name)
		}
	}
	long, err := parseCFN([]byte("Resources:\n  P:\n    Type: AWS::Lambda::Permission\n    Properties:\n      SourceArn:\n        Fn::GetAtt: [R, Arn]\n"))
	if err != nil || len(cfnPermissionProblems(long, "R")) == 0 || getAtt(props(long.Resources["P"])["SourceArn"]) != "R.Arn" {
		t.Errorf("long-form Fn::GetAtt must resolve to R.Arn and the bare permission must be flagged: err = %v", err)
	}
}
