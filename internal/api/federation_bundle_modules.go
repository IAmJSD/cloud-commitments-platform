package api

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io/fs"
	"strings"

	cudlyiac "github.com/LeanerCloud/cloud-commitments-platform/iac"
	cudlytf "github.com/LeanerCloud/cloud-commitments-platform/terraform"
)

// bundledModule is a Terraform module that a federation bundle calls from
// outside its own embedded directory. The bundle ships a copy of it under
// terraform/<zipDir> and points the call at that copy, so `terraform init`
// works from the unzipped bundle (#1431).
type bundledModule struct {
	repoSource string        // the module source as written in the repo's .tf files
	fsys       fs.ReadFileFS // where the module's files are embedded
	dir        string        // the module's directory in fsys
	zipDir     string        // its directory in the zip, relative to terraform/
}

// bundleExtraModules returns the out-of-tree modules a bundle's Terraform
// calls. Only the Azure bundle has one: the custom reservation-purchaser role,
// shared with the host side and checked by scripts/check-azure-role-parity.sh.
func bundleExtraModules(target string) []bundledModule {
	if target != "azure" {
		return nil
	}
	return []bundledModule{{
		repoSource: "../../../../terraform/modules/iam/azure/cudly-reservation-role",
		fsys:       cudlytf.ReservationRoleModule,
		dir:        cudlytf.ReservationRoleModuleDir,
		zipDir:     "modules/cudly-reservation-role",
	}}
}

// addBundleTerraformDir writes the embedded Terraform directory srcDir to the
// zip under terraform/, followed by every module in mods, with each module
// call rewritten to the shipped copy. A module whose repoSource no .tf file
// calls is an error: the rewrite would otherwise silently stop applying after
// the call is moved.
func addBundleTerraformDir(zw *zip.Writer, srcDir string, mods []bundledModule) error {
	used := make([]bool, len(mods))
	err := fs.WalkDir(cudlyiac.Modules, srcDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := cudlyiac.Modules.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.HasSuffix(p, ".tf") {
			b = rewriteModuleSources(b, mods, used)
		}
		return addBytesToZip(zw, "terraform/"+strings.TrimPrefix(p, srcDir+"/"), b)
	})
	if err != nil {
		return err
	}
	for i, m := range mods {
		if !used[i] {
			return fmt.Errorf("no module under %s calls %q", srcDir, m.repoSource)
		}
		if err := addDirToZip(zw, m.fsys, m.dir, "terraform/"+m.zipDir); err != nil {
			return fmt.Errorf("module %s: %w", m.zipDir, err)
		}
	}
	return nil
}

// rewriteModuleSources points each quoted repoSource in src at the module's
// copy in the zip, and marks the modules it rewrote in used.
func rewriteModuleSources(src []byte, mods []bundledModule, used []bool) []byte {
	for i, m := range mods {
		quoted := []byte(`"` + m.repoSource + `"`)
		if bytes.Contains(src, quoted) {
			src = bytes.ReplaceAll(src, quoted, []byte(`"./`+m.zipDir+`"`))
			used[i] = true
		}
	}
	return src
}
