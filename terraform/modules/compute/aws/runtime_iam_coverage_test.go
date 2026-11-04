// Detects CE/EC2 action names missing from runtime IaC, including gaps shared
// by both runtime flavors that check-aws-iam-parity.sh cannot detect (#1967/#1968).
// This is source/action-presence coverage, not IAM evaluation: Effect, resource
// scope, role attachment, and inline comments require separate review. Unused
// grants (#1322) and other service namespaces are outside this guard's scope.
package aws_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// sdkServiceToIAMPrefix maps the CE/EC2 aws-sdk-go-v2 service package names to
// the IAM action prefixes they authorize against. Cost Explorer's package is
// costexplorer but its actions are ce:*. sts is deliberately absent:
// GetCallerIdentity requires no IAM permission and must never enter the set.
var sdkServiceToIAMPrefix = map[string]string{
	"costexplorer": "ce",
	"ec2":          "ec2",
}

// requiredDerivedActions is the floor from #1967/#1968: actions the code is
// known to call that no runtime flavor grants today. Asserting the scanner
// finds these (independent of whether any file grants them) catches a broken
// walker directly, rather than letting it pass by inspecting nothing.
var requiredDerivedActions = []string{
	"ce:GetCostAndUsage",
	"ec2:CreateReservedInstancesListing",
	"ec2:DescribeReservedInstancesListings",
	"ec2:CancelReservedInstancesListing",
	"ec2:DescribeInstanceTypes",
	"ec2:CreateTags",
}

// runtimeIAMFiles are the IaC files that grant the runtime role's IAM
// actions, relative to this package's directory (the two flavors compared
// by check-aws-iam-parity.sh comparison 1).
var runtimeIAMFiles = []string{
	filepath.Join("lambda", "main.tf"),
	filepath.Join("fargate", "main.tf"),
}

// calledAction records where a derived action was first seen, so a failure
// message names a place the reader can open.
type calledAction struct {
	file string
	line int
}

func TestRuntimeGrantsEveryCalledAction(t *testing.T) {
	called := deriveCalledActions(t)

	for _, action := range requiredDerivedActions {
		if _, ok := called[action]; !ok {
			t.Errorf("scanner did not derive %s from runtime source and dependencies; it is a known SDK call the runtime role must be granted (#1967/#1968) and its absence here means the walker is broken, not that the call went away", action)
		}
	}

	for _, rel := range runtimeIAMFiles {
		t.Run(rel, func(t *testing.T) {
			granted := grantedActions(t, rel)
			for action, site := range called {
				if !granted[action] {
					t.Errorf("%s does not grant %s, called at %s:%d", rel, action, site.file, site.line)
				}
			}
		})
	}
}

// deriveCalledActions walks runtime source and returns every IAM action implied by
// an SDK *Input request literal, keyed by action.
func deriveCalledActions(t *testing.T) map[string]calledAction {
	t.Helper()

	root := repoRoot(t)
	// Scan the selected library versions, including workspace replacements.
	cmd := exec.CommandContext(t.Context(), "go", "list", "-m", "-f", "{{.Dir}}",
		"github.com/LeanerCloud/cloud-commitments-go/providers/aws",
		"github.com/LeanerCloud/cloud-commitments-go/pkg")
	cmd.Dir = root
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("locating runtime dependencies: %v", err)
	}
	moduleDirs := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(moduleDirs) != 2 {
		t.Fatalf("expected two runtime dependency directories, got %q", output)
	}
	scanRoots := make([]string, 0, 1+len(moduleDirs))
	scanRoots = append(scanRoots, filepath.Join(root, "internal"))
	for _, dir := range moduleDirs {
		if !filepath.IsAbs(dir) {
			t.Fatalf("runtime dependency directory is not absolute: %q", dir)
		}
		scanRoots = append(scanRoots, dir)
	}
	fset := token.NewFileSet()
	actions := map[string]calledAction{}
	filesScanned := 0

	for _, dir := range scanRoots {
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			filesScanned++
			return scanFileForActions(t, fset, root, path, actions)
		})
		if err != nil {
			t.Fatalf("walking %s: %v", dir, err)
		}
	}

	if filesScanned == 0 {
		t.Fatalf("walked %v and parsed zero Go files; every assertion below would pass by inspecting nothing", scanRoots)
	}
	if len(actions) == 0 {
		t.Fatalf("derived zero SDK actions from %v; a broken import or literal match would pass every assertion below by inspecting nothing", scanRoots)
	}
	return actions
}

// scanFileForActions parses one Go file and records every action its SDK
// *Input literals imply into actions, keyed by action so the first call site
// wins.
func scanFileForActions(t *testing.T, fset *token.FileSet, root, path string, actions map[string]calledAction) error {
	t.Helper()

	file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if parseErr != nil {
		t.Fatalf("parsing %s: %v", path, parseErr)
	}

	aliasToPrefix := sdkImportAliases(file)
	if len(aliasToPrefix) == 0 {
		return nil
	}

	relPath, relErr := filepath.Rel(root, path)
	if relErr != nil {
		return relErr
	}

	ast.Inspect(file, func(n ast.Node) bool {
		action, ok := actionFromCompositeLit(n, aliasToPrefix)
		if !ok {
			return true
		}
		if _, exists := actions[action]; exists {
			return true
		}
		actions[action] = calledAction{file: relPath, line: fset.Position(n.Pos()).Line}
		return true
	})
	return nil
}

// actionFromCompositeLit reports the IAM action implied by n, if n is a
// composite literal of an SDK *Input request type whose package alias
// resolves through aliasToPrefix (e.g. &ec2.CreateTagsInput{...} -> "ec2:CreateTags").
func actionFromCompositeLit(n ast.Node, aliasToPrefix map[string]string) (string, bool) {
	cl, ok := n.(*ast.CompositeLit)
	if !ok {
		return "", false
	}
	sel, ok := cl.Type.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", false
	}
	prefix, ok := aliasToPrefix[ident.Name]
	if !ok {
		return "", false
	}
	typeName := sel.Sel.Name
	op := strings.TrimSuffix(typeName, "Input")
	if op == "" || op == typeName {
		return "", false
	}
	return prefix + ":" + op, true
}

// sdkImportAliases returns, for one file, the map from the local identifier
// an aws-sdk-go-v2 service package is used under to the IAM prefix it
// authorizes against. A subpackage import such as .../service/ec2/types is
// deliberately excluded: it defines the enum and shape types the *Input
// structs embed, not the *Input structs themselves, so its alias must never
// stand in for the service package's.
func sdkImportAliases(file *ast.File) map[string]string {
	const svcPrefix = "github.com/aws/aws-sdk-go-v2/service/"

	aliases := map[string]string{}
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		rest := strings.TrimPrefix(path, svcPrefix)
		if rest == path || strings.Contains(rest, "/") {
			continue
		}
		prefix, ok := sdkServiceToIAMPrefix[rest]
		if !ok {
			continue
		}
		alias := rest
		if imp.Name != nil {
			if imp.Name.Name == "_" || imp.Name.Name == "." {
				continue
			}
			alias = imp.Name.Name
		}
		aliases[alias] = prefix
	}
	return aliases
}

// grantedActionPattern is the CE/EC2 subset of check-aws-iam-parity.sh's
// extraction regex, rewritten with a capture group so the match includes only
// the action itself, not the boundary byte before it.
var grantedActionPattern = regexp.MustCompile(`(?:^|[^A-Za-z])((?:ce|ec2):[A-Z][A-Za-z]+)`)

// hashCommentLinePattern matches a whole line whose first non-blank byte is
// #, the comment style Terraform uses here.
var hashCommentLinePattern = regexp.MustCompile(`(?m)^[ \t]*#.*$`)

// grantedActions extracts action names from path after stripping whole-line
// # comments. It does not parse policy semantics or other comment forms.
func grantedActions(t *testing.T, path string) map[string]bool {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	content := hashCommentLinePattern.ReplaceAllString(string(data), "")

	granted := map[string]bool{}
	for _, m := range grantedActionPattern.FindAllStringSubmatch(content, -1) {
		granted[m[1]] = true
	}
	return granted
}
