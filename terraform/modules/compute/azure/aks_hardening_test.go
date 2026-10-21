// Regression test for issue #121: the AKS module left the API server public
// (no private_cluster_enabled / api_server_access_profile), left the static,
// non-expiring, non-Entra-bound local admin credential enabled (no
// local_account_disabled), and set role_based_access_control_enabled = true
// (Kubernetes RBAC only) with a comment claiming "RBAC and Azure AD
// integration" while no azure_active_directory_role_based_access_control
// block existed anywhere in the module.
package azure_test

import (
	"os"
	"regexp"
	"testing"
)

func TestAKSClusterIsPrivateWithEntraRBAC(t *testing.T) {
	data, err := os.ReadFile("aks/main.tf")
	if err != nil {
		t.Fatalf("reading aks/main.tf: %v", err)
	}
	content := string(data)

	if !regexp.MustCompile(`\bprivate_cluster_enabled\s*=\s*var\.private_cluster_enabled\b`).MatchString(content) {
		t.Error(`azurerm_kubernetes_cluster.main must set private_cluster_enabled from a module variable (issue #121); the API server was reachable from 0.0.0.0/0 with neither private_cluster_enabled nor api_server_access_profile set`)
	}

	if !regexp.MustCompile(`\blocal_account_disabled\s*=\s*var\.local_account_disabled\b`).MatchString(content) {
		t.Error(`azurerm_kubernetes_cluster.main must set local_account_disabled from a module variable (issue #121); the static, non-expiring, non-Entra-bound cluster-admin credential was left enabled by default`)
	}

	if !regexp.MustCompile(`azure_active_directory_role_based_access_control\s*\{[^}]*azure_rbac_enabled\s*=\s*true`).MatchString(content) {
		t.Error(`azurerm_kubernetes_cluster.main must have an azure_active_directory_role_based_access_control block with azure_rbac_enabled = true (issue #121); role_based_access_control_enabled = true alone is Kubernetes-native RBAC only and binds no Entra identity`)
	}

	if !regexp.MustCompile(`azure_active_directory_role_based_access_control\s*\{[^}]*managed\s*=\s*true`).MatchString(content) {
		t.Error(`azurerm_kubernetes_cluster.main's azure_active_directory_role_based_access_control block must set managed = true; on azurerm ~> 3.0, managed defaults to false (the deprecated legacy AAD integration), which makes apply fail demanding client_app_id/server_app_id/server_app_secret even though azure_rbac_enabled is set -- validate passes but apply does not`)
	}

	varsData, err := os.ReadFile("aks/variables.tf")
	if err != nil {
		t.Fatalf("reading aks/variables.tf: %v", err)
	}
	vars := string(varsData)

	if !regexp.MustCompile(`variable\s+"private_cluster_enabled"\s*\{[^}]*default\s*=\s*true`).MatchString(vars) {
		t.Error(`var.private_cluster_enabled must default to true, so a caller who does not override it gets a private cluster`)
	}

	if !regexp.MustCompile(`variable\s+"local_account_disabled"\s*\{[^}]*default\s*=\s*true`).MatchString(vars) {
		t.Error(`var.local_account_disabled must default to true, so a caller who does not override it has the local admin credential disabled`)
	}

	if !regexp.MustCompile(`contains\(var\.authorized_ip_ranges,\s*"0\.0\.0\.0/0"\)`).MatchString(vars) {
		t.Error(`var.authorized_ip_ranges must validate against "0.0.0.0/0", so disabling the private cluster can't silently reopen the API server to the whole internet`)
	}
}

// Issue #1115: DB_PASSWORD_SECRET reads a secret_key_ref key from the
// database Kubernetes Secret; a key renamed in the Secret's data map but not
// in the env reference makes the pod fail with CreateContainerConfigError.
func TestAKSDatabaseSecretKeyRefsExistInSecretData(t *testing.T) {
	data, err := os.ReadFile("aks/main.tf")
	if err != nil {
		t.Fatalf("reading aks/main.tf: %v", err)
	}
	content := string(data)

	secret := regexp.MustCompile(`(?s)resource\s+"kubernetes_secret"\s+"database"\s*\{.*?\n\s*data\s*=\s*\{(.*?)\n\s*\}`).FindStringSubmatch(content)
	if secret == nil {
		t.Fatal(`could not isolate the kubernetes_secret.database data map`)
	}
	keys := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=`).FindAllStringSubmatch(secret[1], -1) {
		keys[m[1]] = true
	}

	// A key is either a literal on the secret_key_ref itself or, since the
	// Deployment and the scheduled-task CronJobs (#893) render their env from
	// local.app_env, the secret_ref of an app_env entry.
	refs := regexp.MustCompile(`secret_key_ref\s*\{\s*name\s*=\s*kubernetes_secret\.database\[0\]\.metadata\[0\]\.name\s*key\s*=\s*"([^"]+)"`).FindAllStringSubmatch(content, -1)
	refs = append(refs, regexp.MustCompile(`\bsecret_ref\s*=\s*"([^"]+)"`).FindAllStringSubmatch(content, -1)...)
	if len(refs) == 0 {
		t.Fatal(`found no secret_key_ref or app_env secret_ref naming a kubernetes_secret.database key`)
	}
	pw := regexp.MustCompile(`\{\s*name\s*=\s*"DB_PASSWORD_SECRET"\s*,\s*value\s*=\s*null\s*,\s*secret_ref\s*=\s*"([^"]+)"\s*\}`).FindAllStringSubmatch(content, -1)
	if len(pw) != 1 || pw[0][1] != "password_secret_name" {
		t.Error(`local.app_env must read DB_PASSWORD_SECRET from the database Secret's "password_secret_name" key (#1115)`)
	}
	for _, r := range refs {
		if !keys[r[1]] {
			t.Errorf("secret_key_ref key %q is not defined in kubernetes_secret.database data (keys: %v)", r[1], keys)
		}
	}
}
