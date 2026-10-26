package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
)

// permissionsTSRelPath is the hand-written frontend file whose Action and
// Resource unions mirror auth.AllActions / auth.AllResources. The test only
// reads it.
var permissionsTSRelPath = filepath.Join("..", "..", "frontend", "src", "permissions.ts")

var tsUnionMember = regexp.MustCompile(`[=|]\s*(?:'([^']*)'|"([^"]*)")`)

// stripTSComments removes // and /* */ comments from src, leaving string
// literals (which may contain comment markers) intact. Template literals
// are not handled; permissions.ts has none in its unions.
func stripTSComments(src string) string {
	var b strings.Builder
	for i := 0; i < len(src); i++ {
		switch {
		case src[i] == '\'' || src[i] == '"':
			end := i + 1
			for end < len(src) && src[end] != src[i] && src[end] != '\n' {
				if src[end] == '\\' {
					end++
				}
				end++
			}
			end = min(end, len(src)-1)
			b.WriteString(src[i : end+1])
			i = end
		case strings.HasPrefix(src[i:], "//"):
			nl := strings.IndexByte(src[i:], '\n')
			if nl < 0 {
				return b.String()
			}
			i += nl - 1
		case strings.HasPrefix(src[i:], "/*"):
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return b.String()
			}
			i += end + 3
		default:
			b.WriteByte(src[i])
		}
	}
	return b.String()
}

// tsUnionMembers returns the string-literal members of
// `export type <name> = 'a' | 'b' ...;` in src, or nil if the declaration
// is absent.
func tsUnionMembers(src, name string) []string {
	src = stripTSComments(src)
	start := regexp.MustCompile(`export\s+type\s+` + regexp.QuoteMeta(name) + `\s*=`).FindStringIndex(src)
	if start == nil {
		return nil
	}
	body := src[start[1]-1:] // keep the '=' so the first member matches
	if end := strings.IndexByte(body, ';'); end >= 0 {
		body = body[:end]
	}
	var out []string
	for _, m := range tsUnionMember.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1]+m[2])
	}
	return out
}

func diffSets(want, got []string) (missing, extra []string) {
	for _, v := range want {
		if !slices.Contains(got, v) {
			missing = append(missing, v)
		}
	}
	for _, v := range got {
		if !slices.Contains(want, v) {
			extra = append(extra, v)
		}
	}
	return missing, extra
}

// TestFrontendPermissionUnionsMatchGo fails when the frontend's Action or
// Resource union drifts from the Go constants (issue #166). The frontend's
// Record<Action, true> check keeps ALL_ACTIONS equal to the union; this
// keeps the union equal to Go, so drift fails CI instead of silently
// dropping or widening group permissions on save (cli#1629).
func TestFrontendPermissionUnionsMatchGo(t *testing.T) {
	raw, err := os.ReadFile(permissionsTSRelPath)
	if err != nil {
		t.Fatalf("read %s: %v", permissionsTSRelPath, err)
	}
	src := string(raw)
	for _, tc := range []struct {
		union string
		want  []string
	}{
		{"Action", auth.AllActions()},
		{"Resource", auth.AllResources()},
	} {
		got := tsUnionMembers(src, tc.union)
		if len(got) == 0 {
			t.Fatalf("no `export type %s = ...` union found in %s", tc.union, permissionsTSRelPath)
		}
		missing, extra := diffSets(tc.want, got)
		if len(missing) > 0 {
			t.Errorf("%s union in %s lacks Go value(s) %q: add them to the union and its exhaustiveness record",
				tc.union, permissionsTSRelPath, missing)
		}
		if len(extra) > 0 {
			t.Errorf("%s union in %s has %q, which auth.All%ss() does not enumerate",
				tc.union, permissionsTSRelPath, extra, tc.union)
		}
	}
}

func TestTSUnionMembers(t *testing.T) {
	src := `
// export type Action = 'commented-out';
export type Action =
  | 'view'
  // a comment with 'quoted' text | 'not-a-member'
  | "double"
  /* | 'blocked' */
  | 'admin';
// a line comment with an opener /* that must not swallow the next union
export type Resource = '*' | 'plans' | '//not-a-comment';
`
	if got, want := tsUnionMembers(src, "Action"), []string{"view", "double", "admin"}; !slices.Equal(got, want) {
		t.Errorf("Action = %q, want %q", got, want)
	}
	if got, want := tsUnionMembers(src, "Resource"), []string{"*", "plans", "//not-a-comment"}; !slices.Equal(got, want) {
		t.Errorf("Resource = %q, want %q", got, want)
	}
	if got := tsUnionMembers(src, "Missing"); got != nil {
		t.Errorf("Missing = %q, want nil", got)
	}
}
