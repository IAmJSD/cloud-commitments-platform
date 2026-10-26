// Guard for issue #12: every provider version constraint in the repo's
// Terraform must carry a ceiling that stops the next major version, and the
// modules we deploy ourselves must also name a tested minor floor, so a
// deleted lock file or an unreviewed `terraform init -upgrade` cannot pull in
// a provider release nobody has run.
//
// The federation modules under iac/federation (and the modules they call) are
// zipped and served to customers without their .terraform.lock.hcl (go:embed
// skips dot files), so they run with whatever provider version the customer
// resolves. Those only get the ceiling, not a minor floor: raising the floor
// would break customers on an older minor for no gain.
package terraform_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// repoRoot is where the scan roots live, relative to this package directory.
const repoRoot = ".."

// scanRoots are the trees holding every Terraform root and module in the repo.
var scanRoots = []string{"terraform", "iac/federation"}

// customerRoot holds the modules served to customers.
const customerRoot = "iac/federation"

// customerReleasedMajor names providers whose newest released major is ahead
// of the one our lock files resolve. Customers who onboarded since that major
// came out already run it (the bundle carries no lock), so a customer module's
// ceiling may admit it rather than force a downgrade; it must still stop the
// major after it. Each entry was validated against that major's latest release.
var customerReleasedMajor = map[string]int{
	"hashicorp/google": 8, // locks resolve 7.27.0; 8.7.0 validated for #12
}

type version struct{ major, minor, patch int }

func (v version) less(o version) bool {
	if v.major != o.major {
		return v.major < o.major
	}
	if v.minor != o.minor {
		return v.minor < o.minor
	}
	return v.patch < o.patch
}

func (v version) String() string { return fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch) }

// policy is what a module's constraints must meet against its lock.
type policy struct {
	minorFloor   bool // the floor must name the locked minor line, not just the major
	ceilingMajor int  // the highest major the ceiling may admit
}

// bounds is a constraint reduced to its lowest allowed version and its
// exclusive ceiling.
type bounds struct {
	lower, upper       version
	hasLower, hasUpper bool
}

type requirement struct {
	file, name, source, constraint string
}

type module struct {
	dir      string // repo-relative, slash separated
	reqs     []requirement
	children []string // repo-relative dirs of local module calls
	locks    map[string]version
}

var (
	versionRe   = regexp.MustCompile(`^(\d+)(?:\.(\d+))?(?:\.(\d+))?$`)
	entryRe     = regexp.MustCompile(`(?s)([A-Za-z0-9_-]+)\s*=\s*\{([^{}]*)\}`)
	sourceRe    = regexp.MustCompile(`\bsource\s*=\s*"([^"]+)"`)
	versionAttr = regexp.MustCompile(`\bversion\s*=\s*"([^"]+)"`)
	moduleSrcRe = regexp.MustCompile(`(?m)^\s*source\s*=\s*"(\.\.?/[^"]+)"`)
	lockRe      = regexp.MustCompile(`(?s)provider\s+"registry\.terraform\.io/([^"]+)"\s*\{\s*version\s*=\s*"([^"]+)"`)
	reqBlockRe  = regexp.MustCompile(`\brequired_providers\s*\{`)
)

func parseVersion(s string) (version, error) {
	m := versionRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return version{}, fmt.Errorf("unparsable version %q", s)
	}
	var v version
	for i, dst := range []*int{&v.major, &v.minor, &v.patch} {
		if m[i+1] != "" {
			*dst, _ = strconv.Atoi(m[i+1])
		}
	}
	return v, nil
}

// pessimisticCeiling is the exclusive ceiling of `~> s`: it bumps the
// second-to-last component written. `~> 5` has no ceiling at all.
func pessimisticCeiling(s string, v version) (version, bool) {
	switch strings.Count(strings.TrimSpace(s), ".") {
	case 0:
		return version{}, false
	case 1:
		return version{major: v.major + 1}, true
	default:
		return version{major: v.major, minor: v.minor + 1}, true
	}
}

func parseConstraint(c string) (bounds, error) {
	var b bounds
	for _, part := range strings.Split(c, ",") {
		if err := b.apply(strings.TrimSpace(part)); err != nil {
			return b, err
		}
	}
	return b, nil
}

func (b *bounds) apply(part string) error {
	for _, op := range []string{"~>", ">=", "<=", "!=", ">", "<", "="} {
		if !strings.HasPrefix(part, op) {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(part, op))
		v, err := parseVersion(raw)
		if err != nil {
			return err
		}
		switch op {
		case "~>":
			b.setLower(v)
			if up, ok := pessimisticCeiling(raw, v); ok {
				b.setUpper(up)
			}
		case ">=", ">", "=":
			b.setLower(v)
		case "<", "<=":
			b.setUpper(v)
		}
		return nil
	}
	return fmt.Errorf("unsupported constraint %q", part)
}

func (b *bounds) setLower(v version) {
	if !b.hasLower || b.lower.less(v) {
		b.lower, b.hasLower = v, true
	}
}

func (b *bounds) setUpper(v version) {
	if !b.hasUpper || v.less(b.upper) {
		b.upper, b.hasUpper = v, true
	}
}

// stripComments drops # and // line comments so braces inside them cannot
// throw off the block scan. Version strings never contain either marker.
func stripComments(src string) string {
	lines := strings.Split(src, "\n")
	for i, l := range lines {
		if j := strings.Index(l, "#"); j >= 0 {
			l = l[:j]
		}
		if j := strings.Index(l, "//"); j >= 0 {
			l = l[:j]
		}
		lines[i] = l
	}
	return strings.Join(lines, "\n")
}

// requiredProvidersBody returns the text between the braces of the
// required_providers block, or "" when the file has none.
func requiredProvidersBody(src string) string {
	loc := reqBlockRe.FindStringIndex(src)
	if loc == nil {
		return ""
	}
	depth := 1
	for i := loc[1]; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[loc[1]:i]
			}
		}
	}
	return ""
}

func parseRequirements(file, src string) []requirement {
	matches := entryRe.FindAllStringSubmatch(requiredProvidersBody(src), -1)
	reqs := make([]requirement, 0, len(matches))
	for _, m := range matches {
		r := requirement{file: file, name: m[1]}
		if s := sourceRe.FindStringSubmatch(m[2]); s != nil {
			r.source = s[1]
		}
		if v := versionAttr.FindStringSubmatch(m[2]); v != nil {
			r.constraint = v[1]
		}
		reqs = append(reqs, r)
	}
	return reqs
}

func readLocks(t *testing.T, dir string) map[string]version {
	t.Helper()
	locks := map[string]version{}
	raw, err := os.ReadFile(filepath.Join(repoRoot, dir, ".terraform.lock.hcl"))
	if os.IsNotExist(err) {
		return locks
	}
	if err != nil {
		t.Fatalf("read lock file in %s: %v", dir, err)
	}
	for _, m := range lockRe.FindAllStringSubmatch(string(raw), -1) {
		v, err := parseVersion(m[2])
		if err != nil {
			t.Fatalf("%s/.terraform.lock.hcl: %s: %v", dir, m[1], err)
		}
		locks[m[1]] = v
	}
	return locks
}

func loadModule(t *testing.T, dir string, tfFiles []string) *module {
	t.Helper()
	mod := &module{dir: dir, locks: readLocks(t, dir)}
	for _, f := range tfFiles {
		raw, err := os.ReadFile(filepath.Join(repoRoot, f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		src := stripComments(string(raw))
		mod.reqs = append(mod.reqs, parseRequirements(f, src)...)
		for _, m := range moduleSrcRe.FindAllStringSubmatch(src, -1) {
			mod.children = append(mod.children, filepath.ToSlash(filepath.Join(dir, m[1])))
		}
	}
	return mod
}

// loadModules returns every directory under scanRoots holding .tf files,
// keyed by repo-relative path.
func loadModules(t *testing.T) map[string]*module {
	t.Helper()
	files := map[string][]string{}
	for _, root := range scanRoots {
		err := filepath.WalkDir(filepath.Join(repoRoot, root), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && d.Name() == ".terraform" {
				return filepath.SkipDir
			}
			if d.IsDir() || filepath.Ext(p) != ".tf" {
				return nil
			}
			rel, err := filepath.Rel(repoRoot, p)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			files[pathDir(rel)] = append(files[pathDir(rel)], rel)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	mods := map[string]*module{}
	for dir, tfFiles := range files {
		sort.Strings(tfFiles)
		mods[dir] = loadModule(t, dir, tfFiles)
	}
	return mods
}

func pathDir(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return "."
}

// customerDirs returns the modules served to customers: everything under
// customerRoot plus every local module those call, transitively.
func customerDirs(mods map[string]*module) map[string]bool {
	out := map[string]bool{}
	var queue []string
	for dir := range mods {
		if strings.HasPrefix(dir, customerRoot+"/") {
			queue = append(queue, dir)
		}
	}
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		if out[dir] {
			continue
		}
		out[dir] = true
		if m := mods[dir]; m != nil {
			queue = append(queue, m.children...)
		}
	}
	return out
}

// checkAgainstLock reports why constraint c is not acceptable against the
// version the lock resolves under policy p.
func checkAgainstLock(c string, locked version, p policy) []string {
	if c == "" {
		return []string{"has no version constraint"}
	}
	b, err := parseConstraint(c)
	if err != nil {
		return []string{err.Error()}
	}
	var problems []string
	nextMajor := version{major: p.ceilingMajor + 1}
	switch {
	case !b.hasUpper:
		problems = append(problems, fmt.Sprintf("has no ceiling; add one that stops %s", nextMajor))
	case nextMajor.less(b.upper):
		problems = append(problems, fmt.Sprintf("ceiling %s admits the next major %s", b.upper, nextMajor))
	case !locked.less(b.upper):
		problems = append(problems, fmt.Sprintf("ceiling %s excludes the locked %s", b.upper, locked))
	}
	switch {
	case !b.hasLower:
		problems = append(problems, "has no floor")
	case locked.less(b.lower):
		problems = append(problems, fmt.Sprintf("floor %s excludes the locked %s", b.lower, locked))
	case p.minorFloor && (b.lower.major != locked.major || (b.lower.minor == 0 && locked.minor != 0)):
		problems = append(problems, fmt.Sprintf("floor %s does not name the locked minor %d.%d", b.lower, locked.major, locked.minor))
	}
	return problems
}

// policyFor returns the rules for provider source locked at locked. Modules we
// deploy must name the locked minor; customer modules only need the ceiling.
func policyFor(source string, locked version, customer bool) policy {
	p := policy{minorFloor: !customer, ceilingMajor: locked.major}
	if m, ok := customerReleasedMajor[source]; customer && ok && m > p.ceilingMajor {
		p.ceilingMajor = m
	}
	return p
}

func checkModule(mod *module, customer bool) []string {
	var problems []string
	for _, r := range mod.reqs {
		locked, ok := mod.locks[r.source]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: %s (%s) is not in %s/.terraform.lock.hcl", r.file, r.name, r.source, mod.dir))
			continue
		}
		for _, p := range checkAgainstLock(r.constraint, locked, policyFor(r.source, locked, customer)) {
			problems = append(problems, fmt.Sprintf("%s: %s %q (locked %s) %s", r.file, r.name, r.constraint, locked, p))
		}
	}
	return problems
}

// checkConsumers reports child module constraints that the calling root's
// own lock does not satisfy; terraform init in that root would then fail.
func checkConsumers(mods map[string]*module, mod *module) []string {
	var problems []string
	for _, childDir := range mod.children {
		child := mods[childDir]
		if child == nil {
			continue
		}
		for _, r := range child.reqs {
			locked, ok := mod.locks[r.source]
			if !ok || r.constraint == "" {
				continue
			}
			b, err := parseConstraint(r.constraint)
			if err != nil || (b.hasLower && locked.less(b.lower)) || (b.hasUpper && !locked.less(b.upper)) {
				problems = append(problems, fmt.Sprintf("%s: %s %q rejects %s, which its caller %s locks", r.file, r.name, r.constraint, locked, mod.dir))
			}
		}
	}
	return problems
}

func TestProviderConstraintsAreBounded(t *testing.T) {
	mods := loadModules(t)
	customer := customerDirs(mods)

	problems := make([]string, 0, len(mods))
	reqCount := 0
	for _, mod := range mods {
		reqCount += len(mod.reqs)
		problems = append(problems, checkModule(mod, customer[mod.dir])...)
		problems = append(problems, checkConsumers(mods, mod)...)
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}

	// Guard against the scan silently matching nothing.
	if reqCount < 70 {
		t.Errorf("found only %d provider requirements; the required_providers scan is broken", reqCount)
	}
	for _, dir := range []string{"iac/federation/aws-target/terraform", "terraform/modules/iam/azure/cudly-reservation-role"} {
		if !customer[dir] {
			t.Errorf("%s is served to customers but was not classified as customer-deployed", dir)
		}
	}
	if customer["terraform/environments/aws"] {
		t.Error("terraform/environments/aws was classified as customer-deployed")
	}
}

func TestCheckAgainstLock(t *testing.T) {
	locked := version{major: 5, minor: 100}
	ours := policy{minorFloor: true, ceilingMajor: 5}
	customer := policy{ceilingMajor: 5}
	customerAhead := policy{ceilingMajor: 6} // a released 6.x customers may run
	cases := []struct {
		constraint string
		p          policy
		ok         bool
	}{
		{"~> 5.100", ours, true},
		{">= 5.100, < 6.0", ours, true},
		{"~> 5.100.0", ours, true},
		{"~> 5.0", ours, false},     // major-only floor
		{"~> 5.0", customer, true},  // fine for customer modules
		{">= 5.0", customer, false}, // no ceiling
		{"~> 5", customer, false},   // `~> 5` has no ceiling
		{">= 5.0, < 7.0", customer, false},
		{">= 4.0, < 6.0", customer, true},
		{">= 5.0, < 7.0", customerAhead, true},
		{">= 5.0, < 8.0", customerAhead, false},
		{"~> 5.101", ours, false}, // floor above the lock
		{"~> 5.100.1", ours, false},
		{"~> 6.0", customer, false}, // excludes the locked 5.x
		{"", customer, false},
	}
	for _, c := range cases {
		got := checkAgainstLock(c.constraint, locked, c.p)
		if (len(got) == 0) != c.ok {
			t.Errorf("checkAgainstLock(%q, %+v) = %v, want ok=%v", c.constraint, c.p, got, c.ok)
		}
	}

	// A lock on x.0 has no higher minor to name.
	if got := checkAgainstLock("~> 6.0", version{major: 6, patch: 3}, policy{minorFloor: true, ceilingMajor: 6}); len(got) != 0 {
		t.Errorf("~> 6.0 against 6.0.3: %v", got)
	}

	// The released-major allowance is for customer modules only.
	if p := policyFor("hashicorp/google", version{major: 7, minor: 27}, false); p.ceilingMajor != 7 {
		t.Errorf("policyFor(google, ours).ceilingMajor = %d, want 7", p.ceilingMajor)
	}
	if p := policyFor("hashicorp/google", version{major: 7, minor: 27}, true); p.ceilingMajor != 8 {
		t.Errorf("policyFor(google, customer).ceilingMajor = %d, want 8", p.ceilingMajor)
	}
}
