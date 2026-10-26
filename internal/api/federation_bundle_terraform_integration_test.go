//go:build integration

package api

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// registryOptInEnv opts in to the part of the test below that downloads
// providers from the public Terraform registry.
const registryOptInEnv = "CUDLY_TEST_TERRAFORM_REGISTRY"

// TestFederationBundles_TerraformInitFromUnzippedBundle builds every federation
// bundle through the real API path, writes it to an empty directory, and runs
// terraform there as a customer would (#1431). Nothing else from the repo is
// reachable from that directory. Skips when terraform is not on PATH.
//
// `terraform get` installs the modules, which is where a module call that
// leaves the bundle fails ("Unreadable module directory"). It needs no
// network, so it runs under this package's no-network guard (#855).
//
// With CUDLY_TEST_TERRAFORM_REGISTRY=1 the test also runs `terraform init
// -backend=false` and `terraform validate`, which download the providers from
// the public registry. Those runs get the environment without the guard's
// proxy variables. Nothing is planned or applied.
func TestFederationBundles_TerraformInitFromUnzippedBundle(t *testing.T) {
	tf, err := exec.LookPath("terraform")
	if err != nil {
		t.Skip("terraform not on PATH")
	}
	registry := os.Getenv(registryOptInEnv) == "1"
	pluginCache := os.Getenv("TF_PLUGIN_CACHE_DIR")
	if pluginCache == "" {
		pluginCache = t.TempDir()
	}
	for _, c := range federationBundleCombos {
		t.Run(c.module, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range renderBundleFiles(t, c.target, c.source, c.sourceCloud) {
				dst := filepath.Join(dir, filepath.FromSlash(name))
				require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o755))
				require.NoError(t, os.WriteFile(dst, content, 0o600))
			}
			tfDir := filepath.Join(dir, "terraform")
			runTerraform(t, tf, tfDir, os.Environ(), "get", "-no-color")
			if !registry {
				return
			}
			env := append(withoutProxy(os.Environ()), "TF_PLUGIN_CACHE_DIR="+pluginCache)
			runTerraform(t, tf, tfDir, env, "init", "-backend=false", "-input=false", "-no-color")
			runTerraform(t, tf, tfDir, env, "validate", "-no-color")
		})
	}
	if !registry {
		t.Logf("set %s=1 to also run terraform init and validate against the public registry", registryOptInEnv)
	}
}

func runTerraform(t *testing.T, tf, dir string, env []string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, tf, args...)
	cmd.Dir = dir
	cmd.Env = append(env, "TF_IN_AUTOMATION=1", "CHECKPOINT_DISABLE=1")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "terraform %v in %s:\n%s", args, dir, out)
}

// withoutProxy drops the proxy variables the no-network guard sets.
func withoutProxy(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(k) {
		case "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY":
			continue
		}
		out = append(out, kv)
	}
	return out
}
