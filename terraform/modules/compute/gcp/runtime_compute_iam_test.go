// Regression test for issue #323 (A13c-022): the Cloud Run runtime SA held
// project-wide roles/compute.viewer, which reads every Compute Engine resource
// in the host project (instance metadata and startup scripts included). The
// runtime only needs the reads the pinned cloud-commitments-go GCP provider
// makes, so it must instead hold a project custom role with exactly those
// permissions. The helpers (codeLines, customRoleBlockRe, ...) live in
// runtime_recommender_iam_test.go.
package gcp_test

import (
	"os"
	"regexp"
	"slices"
	"testing"
)

const computeReaderRoleID = "cudlyComputeReader"

// computeReaderPermissions maps each permission to the provider call that
// needs it (providers/gcp at the version pinned in go.mod).
var computeReaderPermissions = []string{
	"compute.commitments.get",      // services/computeengine: RegionCommitments.Get when adopting a re-driven purchase
	"compute.commitments.list",     // services/computeengine: RegionCommitments.List (existing CUDs)
	"compute.machineTypes.list",    // services/computeengine: MachineTypes.List (purchase validation)
	"compute.regionOperations.get", // op.Wait after RegionCommitments.Insert polls RegionOperations.Get
	"compute.regions.list",         // provider.go / recommendations.go: Regions.List
	"resourcemanager.projects.get", // ValidateCredentials GetProject, and the ambient default-project lookup via projects.list
}

// Any predefined Compute Engine role (roles/compute.viewer, .admin, ...).
var predefinedComputeRe = regexp.MustCompile(`roles/compute\.`)

func TestCloudRunComputeGrantIsNarrow(t *testing.T) {
	const file = "cloud-run/main.tf"
	const sa = "google_service_account.cloud_run.email"

	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}
	code := codeLines(string(data))

	if loc := predefinedComputeRe.FindStringIndex(code); loc != nil {
		t.Errorf("%s must not grant a predefined roles/compute.* role (issue #323); found %q", file, code[loc[0]:min(loc[0]+40, len(code))])
	}

	var roleResource string
	for _, m := range customRoleBlockRe.FindAllStringSubmatch(code, -1) {
		id := roleIDRe.FindStringSubmatch(m[2])
		if id == nil || id[1] != computeReaderRoleID {
			continue
		}
		if roleResource != "" {
			t.Fatalf("%s defines %s more than once", file, computeReaderRoleID)
		}
		roleResource = m[1]
		perms := permissionsRe.FindStringSubmatch(m[2])
		if perms == nil {
			t.Fatalf("%s: custom role %s has no permissions list", file, computeReaderRoleID)
		}
		quoted := quotedRe.FindAllStringSubmatch(perms[1], -1)
		got := make([]string, 0, len(quoted))
		for _, q := range quoted {
			got = append(got, q[1])
		}
		slices.Sort(got)
		if !slices.Equal(got, computeReaderPermissions) {
			t.Errorf("%s: custom role %s permissions = %v, want exactly %v", file, computeReaderRoleID, got, computeReaderPermissions)
		}
	}
	if roleResource == "" {
		t.Fatalf("%s must define google_project_iam_custom_role with role_id %q (issue #323)", file, computeReaderRoleID)
	}

	roleRef := regexp.MustCompile(`(?m)^\s*role\s*=\s*google_project_iam_custom_role\.` + regexp.QuoteMeta(roleResource) + `\.id\s*$`)
	memberRef := regexp.MustCompile(`(?m)^\s*member\s*=\s*"serviceAccount:\$\{` + regexp.QuoteMeta(sa) + `\}"\s*$`)
	projectRef := regexp.MustCompile(`(?m)^\s*project\s*=\s*var\.project_id\s*$`)
	bound := 0
	for _, m := range iamMemberBlockRe.FindAllStringSubmatch(code, -1) {
		if !roleRef.MatchString(m[1]) {
			continue
		}
		bound++
		if !memberRef.MatchString(m[1]) {
			t.Errorf("%s: the %s binding must target the runtime SA %s", file, computeReaderRoleID, sa)
		}
		if !projectRef.MatchString(m[1]) {
			t.Errorf("%s: the %s binding must be project-scoped (project = var.project_id)", file, computeReaderRoleID)
		}
	}
	if bound != 1 {
		t.Errorf("%s: want exactly one google_project_iam_member binding %s, got %d", file, computeReaderRoleID, bound)
	}
}
