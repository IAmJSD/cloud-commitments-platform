package iac

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// These tests read the ARM template customers deploy into every onboarded
// Azure subscription, and the shipped commands that deploy it, from the repo
// tree (arm/ is not embedded; setup.sh and known-issues.md point customers at
// the file itself).

const crossSubscriptionTemplate = "../arm/CUDly-CrossSubscription/template.json"

// crossSubscriptionDeployCommandSources are the shipped files that tell a
// customer how to run `az deployment sub create` against the template.
var crossSubscriptionDeployCommandSources = []string{
	"../arm/CUDly-CrossSubscription/setup.sh",
	"../known-issues.md",
}

// readCrossSubscriptionTemplate returns the template's declared parameters and
// the rest of the template (everything but the parameters section) as JSON
// text, so a parameter's own declaration cannot count as a use of it.
func readCrossSubscriptionTemplate(t *testing.T) (map[string]json.RawMessage, string) {
	t.Helper()
	raw, err := os.ReadFile(crossSubscriptionTemplate)
	if err != nil {
		t.Fatalf("read %s: %v", crossSubscriptionTemplate, err)
	}
	var doc map[string]json.RawMessage
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", crossSubscriptionTemplate, err)
	}
	var params map[string]json.RawMessage
	if err = json.Unmarshal(doc["parameters"], &params); err != nil {
		t.Fatalf("parse %s parameters: %v", crossSubscriptionTemplate, err)
	}
	if len(params) == 0 {
		t.Fatalf("%s declares no parameters; expected at least servicePrincipalObjectId", crossSubscriptionTemplate)
	}
	delete(doc, "parameters")
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("re-marshal %s: %v", crossSubscriptionTemplate, err)
	}
	return params, string(body)
}

// TestCrossSubscriptionTemplateReadsEveryParameter guards issue #143: the
// template declared roleAssignmentGuidPrefix, described as the base GUID for
// deterministic role assignment names, and nothing read it. A parameter the
// template ignores promises the customer a control that does nothing.
func TestCrossSubscriptionTemplateReadsEveryParameter(t *testing.T) {
	params, body := readCrossSubscriptionTemplate(t)
	for name := range params {
		// ARM parameter names are case-insensitive, as is the function name.
		ref := regexp.MustCompile(`(?i)parameters\(\s*'` + regexp.QuoteMeta(name) + `'\s*\)`)
		if !ref.MatchString(body) {
			t.Errorf("parameter %q is declared but no variable, resource or output reads parameters('%s')", name, name)
		}
	}
}

// TestCrossSubscriptionTemplateHasNoNewGuidDefault keeps role assignment and
// role definition names deterministic. A [newGuid()] default yields a fresh
// value on every deployment, so feeding it into a guid(...) name seed would
// make each redeploy try to create a second assignment of the same role to the
// same principal at the same scope, which Azure refuses (RoleAssignmentExists).
func TestCrossSubscriptionTemplateHasNoNewGuidDefault(t *testing.T) {
	params, _ := readCrossSubscriptionTemplate(t)
	newGUID := regexp.MustCompile(`(?i)newGuid\s*\(`)
	for name, decl := range params {
		if newGUID.Match(decl) {
			t.Errorf("parameter %q has a newGuid() default: %s", name, decl)
		}
	}
}

// TestCrossSubscriptionDeployCommandsPassOnlyDeclaredParameters checks the
// other side of removing a parameter: ARM fails a deployment that supplies a
// parameter the template does not declare, so every `--parameters key=value`
// in a shipped deploy command for this template must name a declared one.
func TestCrossSubscriptionDeployCommandsPassOnlyDeclaredParameters(t *testing.T) {
	params, _ := readCrossSubscriptionTemplate(t)
	declared := make(map[string]bool, len(params))
	for name := range params {
		declared[strings.ToLower(name)] = true
	}
	paramArg := regexp.MustCompile(`--parameters\s+"?([A-Za-z0-9_]+)=`)

	for _, src := range crossSubscriptionDeployCommandSources {
		cmds := crossSubscriptionDeployCommands(t, src)
		if len(cmds) == 0 {
			t.Errorf("%s: found no `az deployment sub create` command for the cross-subscription template; update crossSubscriptionDeployCommandSources", src)
			continue
		}
		for _, cmd := range cmds {
			for _, m := range paramArg.FindAllStringSubmatch(cmd, -1) {
				if !declared[strings.ToLower(m[1])] {
					t.Errorf("%s: deploy command passes undeclared parameter %q, which ARM rejects:\n%s", src, m[1], cmd)
				}
			}
		}
	}
}

// crossSubscriptionDeployCommands returns each `az deployment sub create`
// command in path that deploys the cross-subscription template, with its
// backslash continuation lines joined.
func crossSubscriptionDeployCommands(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	joined := regexp.MustCompile(`\\\r?\n`).ReplaceAllString(string(raw), " ")
	var cmds []string
	for _, line := range strings.Split(joined, "\n") {
		if !strings.Contains(line, "az deployment sub create") {
			continue
		}
		if strings.Contains(line, "CUDly-CrossSubscription/template.json") ||
			strings.Contains(line, "${SCRIPT_DIR}/template.json") {
			cmds = append(cmds, line)
		}
	}
	return cmds
}
