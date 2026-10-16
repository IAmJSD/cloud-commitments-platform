// Regression test for issue #198: the AKS module created
// azurerm_user_assigned_identity.workload and granted it Key Vault Secrets
// User, but never enabled Microsoft Entra Workload ID, so no pod could obtain
// a token for that identity. Every resource was individually valid and
// `terraform validate` passed, so only a check that all the pieces are present
// together, and agree on the Kubernetes identity, catches the gap.
package azure_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// stripHCLComments drops whole-line comments so a commented-out setting (or
// prose mentioning one) can't satisfy the checks below.
func stripHCLComments(src string) string {
	lines := strings.Split(src, "\n")
	out := lines[:0]
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "#") || strings.HasPrefix(t, "//") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// hclBlock returns the body of the first block whose header matches header
// (which must end just before the opening brace), using brace matching so
// nested blocks and "${...}" interpolations are handled. ok is false when no
// such block exists.
func hclBlock(src, header string) (body string, ok bool) {
	loc := regexp.MustCompile(header + `\s*\{`).FindStringIndex(src)
	if loc == nil {
		return "", false
	}
	depth := 0
	for i := loc[1] - 1; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[loc[1]:i], true
			}
		}
	}
	return "", false
}

// workloadIdentityProblems returns every way main.tf fails to wire the
// workload UAI to the pods that are supposed to assume it.
func workloadIdentityProblems(raw string) []string {
	src := stripHCLComments(raw)
	var problems []string
	fail := func(msg string) { problems = append(problems, msg) }
	has := func(body, re string) bool { return regexp.MustCompile(re).MatchString(body) }

	locals, ok := hclBlock(src, `(?m)^locals`)
	if !ok {
		fail("no locals block")
	}
	if !has(locals, `\bworkload_namespace\s*=\s*var\.environment\b`) {
		fail("local.workload_namespace must be var.environment, the namespace the module deploys into")
	}
	if !has(locals, `\bworkload_service_account_name\s*=\s*"\$\{var\.project_name\}-api"`) {
		fail(`local.workload_service_account_name must be "${var.project_name}-api", the service account the module deploys`)
	}

	cluster, ok := hclBlock(src, `resource\s+"azurerm_kubernetes_cluster"\s+"main"`)
	if !ok {
		fail("no azurerm_kubernetes_cluster.main")
	}
	if !has(cluster, `\boidc_issuer_enabled\s*=\s*true\b`) {
		fail("azurerm_kubernetes_cluster.main must set oidc_issuer_enabled = true (defaults to false); without an issuer Entra cannot validate a service-account token")
	}
	if !has(cluster, `\bworkload_identity_enabled\s*=\s*true\b`) {
		fail("azurerm_kubernetes_cluster.main must set workload_identity_enabled = true (defaults to false); without it no webhook injects AZURE_CLIENT_ID / AZURE_FEDERATED_TOKEN_FILE")
	}

	fic, ok := hclBlock(src, `resource\s+"azurerm_federated_identity_credential"\s+"workload"`)
	if !ok {
		fail("no azurerm_federated_identity_credential.workload; nothing lets a service-account token be exchanged for the workload UAI")
	}
	if !has(fic, `\bparent_id\s*=\s*azurerm_user_assigned_identity\.workload\.id\b`) {
		fail("the federated credential must be attached to azurerm_user_assigned_identity.workload")
	}
	if !has(fic, `\bissuer\s*=\s*azurerm_kubernetes_cluster\.main\.oidc_issuer_url\b`) {
		fail("the federated credential's issuer must be azurerm_kubernetes_cluster.main.oidc_issuer_url")
	}
	if !has(fic, `\baudience\s*=\s*\[\s*"api://AzureADTokenExchange"\s*\]`) {
		fail(`the federated credential's audience must be exactly ["api://AzureADTokenExchange"]`)
	}
	if !has(fic, `\bsubject\s*=\s*"system:serviceaccount:\$\{local\.workload_namespace\}:\$\{local\.workload_service_account_name\}"`) {
		fail("the federated credential's subject must be system:serviceaccount:${local.workload_namespace}:${local.workload_service_account_name}, the identity the deployment actually runs as")
	}

	// The subject is built from the locals; the Kubernetes objects must be
	// built from the same locals, or the token's subject won't match.
	ns, _ := hclBlock(src, `resource\s+"kubernetes_namespace"\s+"app"`)
	if !has(ns, `\bname\s*=\s*local\.workload_namespace\b`) {
		fail("kubernetes_namespace.app must be named local.workload_namespace, the namespace in the federated subject")
	}
	sa, _ := hclBlock(src, `resource\s+"kubernetes_service_account"\s+"app"`)
	if !has(sa, `\bname\s*=\s*local\.workload_service_account_name\b`) {
		fail("kubernetes_service_account.app must be named local.workload_service_account_name, the service account in the federated subject")
	}
	if !has(sa, `"azure\.workload\.identity/client-id"\s*=\s*azurerm_user_assigned_identity\.workload\.client_id\b`) {
		fail("kubernetes_service_account.app must carry the azure.workload.identity/client-id annotation for the workload UAI")
	}

	deploy, _ := hclBlock(src, `resource\s+"kubernetes_deployment"\s+"app"`)
	if !has(deploy, `\bservice_account_name\s*=\s*kubernetes_service_account\.app\[0\]\.metadata\[0\]\.name\b`) {
		fail("kubernetes_deployment.app must run as kubernetes_service_account.app")
	}
	if !has(deploy, `(?s)\bdepends_on\s*=\s*\[[^\]]*\bazurerm_federated_identity_credential\.workload\b`) {
		fail("kubernetes_deployment.app must depend on azurerm_federated_identity_credential.workload; nothing in the deployment references it, so without the dependency pods can start before token exchange works")
	}
	tmpl, _ := hclBlock(deploy, `\btemplate`)
	tmplMeta, _ := hclBlock(tmpl, `\bmetadata`)
	podLabels, _ := hclBlock(tmplMeta, `\blabels\s*=`)
	if !has(podLabels, `"azure\.workload\.identity/use"\s*=\s*"true"`) {
		fail(`the deployment's pod template must be labeled azure.workload.identity/use = "true"; the webhook ignores unlabeled pods`)
	}

	return problems
}

func readAKSMain(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("aks/main.tf")
	if err != nil {
		t.Fatalf("reading aks/main.tf: %v", err)
	}
	return string(data)
}

func TestAKSWorkloadIdentityIsWiredEndToEnd(t *testing.T) {
	for _, p := range workloadIdentityProblems(readAKSMain(t)) {
		t.Errorf("issue #198: %s", p)
	}
}

// TestAKSWorkloadIdentityGuardDetectsEachMissingPiece proves the guard is not
// vacuous: removing or breaking any single piece of the real module must be
// reported.
func TestAKSWorkloadIdentityGuardDetectsEachMissingPiece(t *testing.T) {
	src := readAKSMain(t)
	mutations := []struct{ name, re, repl string }{
		{"oidc issuer disabled", `oidc_issuer_enabled\s*=\s*true`, `oidc_issuer_enabled = false`},
		{"workload identity disabled", `workload_identity_enabled\s*=\s*true`, `workload_identity_enabled = false`},
		{"federated credential removed", `resource\s+"azurerm_federated_identity_credential"\s+"workload"`, `resource "azurerm_federated_identity_credential" "renamed"`},
		{"federated credential dependency removed", `(?m)^[ \t]*azurerm_federated_identity_credential\.workload,[ \t]*\n`, ``},
		{"wrong issuer", `azurerm_kubernetes_cluster\.main\.oidc_issuer_url`, `"https://example.invalid/"`},
		{"wrong audience", `"api://AzureADTokenExchange"`, `"api://other"`},
		{"subject namespace hardcoded", `system:serviceaccount:\$\{local\.workload_namespace\}`, `system:serviceaccount:default`},
		{"subject service account hardcoded", `:\$\{local\.workload_service_account_name\}"`, `:other-sa"`},
		{"namespace name drifts", `name = local\.workload_namespace`, `name = "other"`},
		{"service account name drifts", `name\s*=\s*local\.workload_service_account_name`, `name = "other-sa"`},
		{"namespace local changed", `workload_namespace\s*=\s*var\.environment`, `workload_namespace = "default"`},
		{"service account local changed", `workload_service_account_name\s*=\s*"\$\{var\.project_name\}-api"`, `workload_service_account_name = "x"`},
		{"pod label removed", `"azure\.workload\.identity/use"\s*=\s*"true"`, `"other" = "true"`},
		{"client-id annotation removed", `"azure\.workload\.identity/client-id"`, `"other/client-id"`},
		{"settings only in comments", `(?m)^(\s*)(oidc_issuer_enabled\s*=\s*true)`, `$1# $2`},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			re := regexp.MustCompile(m.re)
			if !re.MatchString(src) {
				t.Fatalf("mutation pattern %q no longer matches aks/main.tf; update the test", m.re)
			}
			if got := workloadIdentityProblems(re.ReplaceAllString(src, m.repl)); len(got) == 0 {
				t.Errorf("guard did not detect mutation %q", m.name)
			}
		})
	}
}
