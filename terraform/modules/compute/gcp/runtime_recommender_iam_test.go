// Regression test for issue #788: the Cloud Run runtime SA held the broad
// predefined roles/recommender.viewer (get/list on every recommender and
// insight type in the project), while the GKE workload SA had no Recommender
// grant at all. The runtime only lists Compute Engine committed-use
// recommendations through the pinned cloud-commitments-go GCP provider, so
// both runtime SAs must instead hold a project custom role containing exactly
// recommender.usageCommitmentRecommendations.list (the contract the CLI setup
// wizard provisions as cudlyRecommendationReader, cloud-commitments-cli#2124).
package gcp_test

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const (
	recommendationReaderRoleID     = "cudlyRecommendationReader"
	recommendationReaderPermission = "recommender.usageCommitmentRecommendations.list"
)

var (
	customRoleBlockRe = regexp.MustCompile(`(?s)resource\s+"google_project_iam_custom_role"\s+"([a-z0-9_]+)"\s*\{(.*?)\n\}`)
	roleIDRe          = regexp.MustCompile(`(?m)^\s*role_id\s*=\s*"([^"]+)"`)
	permissionsRe     = regexp.MustCompile(`(?s)permissions\s*=\s*\[(.*?)\]`)
	quotedRe          = regexp.MustCompile(`"([^"]*)"`)
	iamMemberBlockRe  = regexp.MustCompile(`(?s)resource\s+"google_project_iam_member"\s+"[a-z0-9_]+"\s*\{(.*?)\n\}`)
	// Any predefined Recommender role (roles/recommender.viewer, .admin,
	// .projectCudViewer, ...) on a non-comment line.
	predefinedRecommenderRe = regexp.MustCompile(`roles/recommender\.`)
)

// codeLines drops whole-line comments so explanatory prose that names the
// old role is not mistaken for a grant.
func codeLines(src string) string {
	var out []string
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func TestRuntimeSARecommenderGrantIsNarrow(t *testing.T) {
	for _, tc := range []struct {
		file string
		sa   string // runtime service account resource name in the module
	}{
		{file: "cloud-run/main.tf", sa: "google_service_account.cloud_run.email"},
		{file: "gke/main.tf", sa: "google_service_account.workload.email"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			data, err := os.ReadFile(tc.file)
			if err != nil {
				t.Fatalf("reading %s: %v", tc.file, err)
			}
			code := codeLines(string(data))

			if loc := predefinedRecommenderRe.FindStringIndex(code); loc != nil {
				t.Errorf("%s must not grant a predefined roles/recommender.* role to the runtime SA (issue #788); found %q", tc.file, code[loc[0]:min(loc[0]+40, len(code))])
			}

			var roleResource string
			for _, m := range customRoleBlockRe.FindAllStringSubmatch(code, -1) {
				id := roleIDRe.FindStringSubmatch(m[2])
				if id == nil || id[1] != recommendationReaderRoleID {
					continue
				}
				if roleResource != "" {
					t.Fatalf("%s defines %s more than once", tc.file, recommendationReaderRoleID)
				}
				roleResource = m[1]
				perms := permissionsRe.FindStringSubmatch(m[2])
				if perms == nil {
					t.Fatalf("%s: custom role %s has no permissions list", tc.file, recommendationReaderRoleID)
				}
				quoted := quotedRe.FindAllStringSubmatch(perms[1], -1)
				got := make([]string, 0, len(quoted))
				for _, q := range quoted {
					got = append(got, q[1])
				}
				if want := []string{recommendationReaderPermission}; !slices.Equal(got, want) {
					t.Errorf("%s: custom role %s permissions = %v, want exactly %v", tc.file, recommendationReaderRoleID, got, want)
				}
			}
			if roleResource == "" {
				t.Fatalf("%s must define google_project_iam_custom_role with role_id %q (issue #788)", tc.file, recommendationReaderRoleID)
			}

			roleRef := regexp.MustCompile(`(?m)^\s*role\s*=\s*google_project_iam_custom_role\.` + regexp.QuoteMeta(roleResource) + `\.id\s*$`)
			memberRef := regexp.MustCompile(`(?m)^\s*member\s*=\s*"serviceAccount:\$\{` + regexp.QuoteMeta(tc.sa) + `\}"\s*$`)
			projectRef := regexp.MustCompile(`(?m)^\s*project\s*=\s*var\.project_id\s*$`)
			bound := 0
			for _, m := range iamMemberBlockRe.FindAllStringSubmatch(code, -1) {
				if roleRef.MatchString(m[1]) {
					bound++
					if !memberRef.MatchString(m[1]) {
						t.Errorf("%s: the %s binding must target the runtime SA %s", tc.file, recommendationReaderRoleID, tc.sa)
					}
					if !projectRef.MatchString(m[1]) {
						t.Errorf("%s: the %s binding must be project-scoped (project = var.project_id)", tc.file, recommendationReaderRoleID)
					}
				}
			}
			if bound != 1 {
				t.Errorf("%s: want exactly one google_project_iam_member binding %s, got %d", tc.file, recommendationReaderRoleID, bound)
			}
		})
	}
}
