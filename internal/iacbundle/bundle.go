// Package iacbundle writes the Terraform part of a customer federation bundle.
// Both bundle builders use it, the API (GET /api/federation/iac?format=bundle)
// and scripts/generate-federation-iac.go --format bundle, so every bundle ships
// the out-of-tree modules its Terraform calls and `terraform init` works from
// the unzipped files (#1431, #1442).
package iacbundle

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"

	cudlytf "github.com/LeanerCloud/cloud-commitments-platform/terraform"
)

// Module is a Terraform module that a federation bundle calls from outside its
// own Terraform directory. The bundle ships a copy of it under
// terraform/<ZipDir> and points the call at that copy.
type Module struct {
	RepoSource string // the module source as written in the repo's .tf files
	FS         fs.FS  // where the module's files are
	Dir        string // the module's directory in FS
	ZipDir     string // its directory in the zip, relative to terraform/
}

// ExtraModules returns the out-of-tree modules the target's bundle calls. Only
// the Azure bundle has one: the custom reservation-purchaser role, shared with
// the host side and checked by scripts/check-azure-role-parity.sh.
func ExtraModules(target string) []Module {
	if target != "azure" {
		return nil
	}
	return []Module{{
		RepoSource: "../../../../terraform/modules/iam/azure/cudly-reservation-role",
		FS:         cudlytf.ReservationRoleModule,
		Dir:        cudlytf.ReservationRoleModuleDir,
		ZipDir:     "modules/cudly-reservation-role",
	}}
}

// AddTerraformDir writes srcDir in fsys to the zip under terraform/, followed
// by every module in mods, with each module call rewritten to the shipped
// copy. Hidden files and directories (.terraform.lock.hcl, a local
// .terraform/) are left out, as go:embed leaves them out of the API's copy. A
// module that no .tf file calls is an error: the rewrite would otherwise
// silently stop applying after the call is moved.
func AddTerraformDir(zw *zip.Writer, fsys fs.FS, srcDir string, mods []Module) error {
	used := make([]bool, len(mods))
	err := walkVisible(fsys, srcDir, func(p string, b []byte) error {
		if strings.HasSuffix(p, ".tf") {
			b = rewriteModuleSources(b, mods, used)
		}
		return addFile(zw, "terraform/"+strings.TrimPrefix(p, srcDir+"/"), b)
	})
	if err != nil {
		return err
	}
	for i, m := range mods {
		if !used[i] {
			return fmt.Errorf("no module under %s calls %q", srcDir, m.RepoSource)
		}
		err := walkVisible(m.FS, m.Dir, func(p string, b []byte) error {
			return addFile(zw, "terraform/"+m.ZipDir+"/"+strings.TrimPrefix(p, m.Dir+"/"), b)
		})
		if err != nil {
			return fmt.Errorf("module %s: %w", m.ZipDir, err)
		}
	}
	return nil
}

// walkVisible calls fn with the path and content of every regular file under
// dir in fsys, skipping hidden files and directories.
func walkVisible(fsys fs.FS, dir string, fn func(p string, b []byte) error) error {
	return fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != dir && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		return fn(p, b)
	})
}

// rewriteModuleSources points each quoted RepoSource in src at the module's
// copy in the zip, and marks the modules it rewrote in used.
func rewriteModuleSources(src []byte, mods []Module, used []bool) []byte {
	for i, m := range mods {
		quoted := []byte(`"` + m.RepoSource + `"`)
		if bytes.Contains(src, quoted) {
			src = bytes.ReplaceAll(src, quoted, []byte(`"./`+m.ZipDir+`"`))
			used[i] = true
		}
	}
	return src
}

func addFile(zw *zip.Writer, name string, content []byte) error {
	f, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = f.Write(content)
	return err
}

// localModuleSourceRe matches a module source that is a local path. Registry
// and provider sources never start with "./" or "../".
var localModuleSourceRe = regexp.MustCompile(`(?m)^\s*source\s*=\s*"(\.\.?/[^"]+)"`)

// UnresolvedLocalModules returns "file: source" for every local module source
// in a bundle's .tf files (keyed by their path in the zip) that does not point
// at a directory of .tf files inside the bundle. terraform init fails on each
// of them with "Unreadable module directory" (#1431). Both bundle builders'
// tests run their output through it.
func UnresolvedLocalModules(files map[string][]byte) []string {
	tfDirs := map[string]bool{}
	for name := range files {
		if strings.HasSuffix(name, ".tf") {
			tfDirs[path.Dir(name)] = true
		}
	}
	var bad []string
	for name, content := range files {
		if !strings.HasSuffix(name, ".tf") {
			continue
		}
		for _, m := range localModuleSourceRe.FindAllSubmatch(content, -1) {
			if !tfDirs[path.Join(path.Dir(name), string(m[1]))] {
				bad = append(bad, name+": "+string(m[1]))
			}
		}
	}
	sort.Strings(bad)
	return bad
}
