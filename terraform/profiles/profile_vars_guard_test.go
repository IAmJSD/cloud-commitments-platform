// Guard for issue #58: the profile examples and the snippets in README.md
// must only set variables that the matching root module
// (terraform/environments/<provider>) declares. Terraform only warns about an
// undeclared variable in a .tfvars file and then ignores it, so a stale name
// (README.md used to document skip_docker_build, skip_docker_push and
// enable_frontend_build, none of which exist at root) silently does nothing
// while looking like it took effect.
//
// The same examples also published database_engine_version = "16.6", the
// full-minor pin that terraform/environments/aws/variables.tf warns breaks
// when RDS auto-upgrades the minor version; that is checked here too.
package profiles_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var clouds = []string{"aws", "azure", "gcp"}

var (
	// variable "name" { at the start of a line in a root *.tf file.
	declRe = regexp.MustCompile(`(?m)^variable\s+"([A-Za-z0-9_]+)"`)
	// A top-level assignment, including a commented-out one ("# name = x"):
	// commented examples are there to be uncommented, so they must be real
	// variables too. Indented lines (map keys inside tags = { ... }) are
	// not top-level and are skipped.
	assignRe = regexp.MustCompile(`(?m)^(?:#[ \t]*)?([a-z][a-z0-9_]*)[ \t]*=`)
	// ```hcl fenced blocks in README.md.
	hclFenceRe = regexp.MustCompile("(?ms)^```hcl\n(.*?)^```")
	// The first line of every README hcl block names the profile it belongs to.
	profilePathRe = regexp.MustCompile(`^#\s*profiles/(aws|azure|gcp)/\S+\.tfvars\s*$`)
	// cat > profiles/<cloud>/x.tfvars <<EOF ... EOF heredocs in bash blocks.
	heredocRe = regexp.MustCompile(`(?ms)cat > profiles/(aws|azure|gcp)/\S+\.tfvars <<EOF\n(.*?)^EOF`)
	// export TF_VAR_name=... references.
	tfVarRe     = regexp.MustCompile(`\bTF_VAR_([A-Za-z0-9_]+)`)
	engineRe    = regexp.MustCompile(`(?m)^(?:#[ \t]*)?database_engine_version[ \t]*=[ \t]*"([^"]*)"`)
	majorOnlyRe = regexp.MustCompile(`^[0-9]+$`)
)

// rootVariables returns the variables declared by every *.tf file in
// terraform/environments/<cloud>, keyed by cloud.
func rootVariables(t *testing.T) map[string]map[string]bool {
	t.Helper()
	out := map[string]map[string]bool{}
	for _, cloud := range clouds {
		files, err := filepath.Glob(filepath.Join("..", "environments", cloud, "*.tf"))
		if err != nil {
			t.Fatal(err)
		}
		decl := map[string]bool{}
		for _, f := range files {
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range declRe.FindAllStringSubmatch(string(src), -1) {
				decl[m[1]] = true
			}
		}
		// Sanity floor so a broken glob or regex cannot make every check
		// below pass vacuously.
		if len(decl) < 20 {
			t.Fatalf("found only %d root variables for %s; the declaration scan is broken", len(decl), cloud)
		}
		out[cloud] = decl
	}
	return out
}

// assignments returns the top-level variable names a tfvars snippet sets.
func assignments(src string) []string {
	matches := assignRe.FindAllStringSubmatch(src, -1)
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, m[1])
	}
	return names
}

// undeclared returns the names src sets that decl does not declare.
func undeclared(src string, decl map[string]bool) []string {
	var bad []string
	for _, name := range assignments(src) {
		if !decl[name] {
			bad = append(bad, name)
		}
	}
	return bad
}

// engineProblems reports database_engine_version values that pin a minor.
func engineProblems(src string) []string {
	var bad []string
	for _, m := range engineRe.FindAllStringSubmatch(src, -1) {
		if !majorOnlyRe.MatchString(m[1]) {
			bad = append(bad, `database_engine_version = "`+m[1]+`" pins a minor version; use the major only (e.g. "16")`)
		}
	}
	return bad
}

// readmeProblems checks every hcl block, tfvars heredoc and TF_VAR_
// reference in the README against the root variables.
func readmeProblems(readme string, decls map[string]map[string]bool) []string {
	var problems []string
	blocks := hclFenceRe.FindAllStringSubmatch(readme, -1)
	if len(blocks) == 0 {
		problems = append(problems, "no ```hcl blocks found; the README scan is broken")
	}
	for i, b := range blocks {
		body := b[1]
		first, _, _ := strings.Cut(body, "\n")
		m := profilePathRe.FindStringSubmatch(first)
		if m == nil {
			problems = append(problems, "hcl block "+strconv.Itoa(i+1)+" must start with a \"# profiles/<provider>/<name>.tfvars\" comment, got "+first)
			continue
		}
		for _, name := range undeclared(body, decls[m[1]]) {
			problems = append(problems, "hcl block "+strconv.Itoa(i+1)+" ("+m[1]+"): "+name+" is not a variable in terraform/environments/"+m[1])
		}
		problems = append(problems, engineProblems(body)...)
	}
	for _, h := range heredocRe.FindAllStringSubmatch(readme, -1) {
		for _, name := range undeclared(h[2], decls[h[1]]) {
			problems = append(problems, "heredoc for profiles/"+h[1]+": "+name+" is not a variable in terraform/environments/"+h[1])
		}
	}
	// The environment-variable example is provider-agnostic, so every
	// environment must declare the names it exports.
	for _, m := range tfVarRe.FindAllStringSubmatch(readme, -1) {
		for _, cloud := range clouds {
			if !decls[cloud][m[1]] {
				problems = append(problems, "TF_VAR_"+m[1]+" is not a variable in terraform/environments/"+cloud)
			}
		}
	}
	return problems
}

func TestProfileExamplesSetOnlyRootVariables(t *testing.T) {
	decls := rootVariables(t)
	for _, cloud := range clouds {
		files, err := filepath.Glob(filepath.Join(cloud, "*.tfvars.example"))
		if err != nil {
			t.Fatal(err)
		}
		if len(files) == 0 {
			t.Errorf("no profile examples found under %s/", cloud)
		}
		for _, f := range files {
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if n := len(assignments(string(src))); n < 5 {
				t.Errorf("%s: found only %d assignments; the tfvars scan is broken", f, n)
			}
			for _, name := range undeclared(string(src), decls[cloud]) {
				t.Errorf("%s sets %s, which terraform/environments/%s does not declare (terraform ignores it)", f, name, cloud)
			}
			for _, p := range engineProblems(string(src)) {
				t.Errorf("%s: %s", f, p)
			}
		}
	}
}

func TestReadmeSnippetsSetOnlyRootVariables(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	problems := readmeProblems(string(readme), rootVariables(t))
	sort.Strings(problems)
	for _, p := range problems {
		t.Error("README.md: " + p)
	}
}

// TestGuardFlagsStaleSnippets proves the checks above can fail: each fixture
// carries one of the defects this guard exists to catch.
func TestGuardFlagsStaleSnippets(t *testing.T) {
	decl := map[string]bool{"enable_docker_build": true, "tags": true, "database_engine_version": true}
	decls := map[string]map[string]bool{"aws": decl, "azure": decl, "gcp": decl}

	cases := []struct {
		name, readme string
		want         string // substring of a reported problem; "" means clean
	}{
		{"clean", "```hcl\n# profiles/aws/dev.tfvars\nenable_docker_build = true\ntags = {\n  Project = \"x\"\n}\n```\n", ""},
		{"undeclared", "```hcl\n# profiles/aws/dev.tfvars\nenable_frontend_build = true\n```\n", "enable_frontend_build is not a variable"},
		{"commented undeclared", "```hcl\n# profiles/aws/dev.tfvars\n# skip_docker_build = false\n```\n", "skip_docker_build is not a variable"},
		{"no profile path", "```hcl\nenable_docker_build = true\n```\n", "must start with"},
		{"minor engine pin", "```hcl\n# profiles/aws/dev.tfvars\ndatabase_engine_version = \"16.6\"\n```\n", "pins a minor version"},
		{"heredoc", "```hcl\n# profiles/aws/dev.tfvars\ntags = {}\n```\n```bash\ncat > profiles/aws/s.tfvars <<EOF\ndatabase_password = \"x\"\nEOF\n```\n", "database_password is not a variable"},
		{"tf var", "```hcl\n# profiles/aws/dev.tfvars\ntags = {}\n```\nexport TF_VAR_database_password=x\n", "TF_VAR_database_password"},
		{"no blocks", "nothing here\n", "no ```hcl blocks"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			problems := strings.Join(readmeProblems(tc.readme, decls), "\n")
			if tc.want == "" && problems != "" {
				t.Fatalf("expected no problems, got:\n%s", problems)
			}
			if !strings.Contains(problems, tc.want) {
				t.Fatalf("expected a problem containing %q, got:\n%s", tc.want, problems)
			}
		})
	}
}
