// Guard for issue #1435: every committed .terraform.lock.hcl must record an
// h1: package hash for each platform that runs Terraform here (linux_amd64
// and linux_arm64 in CI and deploy jobs, darwin_arm64 and darwin_amd64 for
// developers). With one platform's hash only, `terraform init
// -lockfile=readonly` on another platform fails with "the cached package ...
// does not match any of the checksums recorded in the dependency lock file",
// and a plain init quietly rewrites the lock file.
//
// A lock file does not say which platform an h1: hash belongs to, so this
// test checks the count. The platform check itself is CI's readonly init on
// Linux. Regenerate a lock file with:
//
//	terraform providers lock -platform=linux_amd64 -platform=linux_arm64 \
//	  -platform=darwin_arm64 -platform=darwin_amd64
package terraform_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// lockPlatforms is how many platforms every lock file must carry hashes for.
const lockPlatforms = 4

var lockProviderRe = regexp.MustCompile(`(?ms)^provider\s+"([^"]+)"\s*\{(.*?)^\}`)

// lockFiles returns the repo-relative paths of every lock file under scanRoots.
func lockFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, root := range scanRoots {
		err := filepath.WalkDir(filepath.Join(repoRoot, root), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && d.Name() == ".terraform" {
				return filepath.SkipDir
			}
			if d.IsDir() || d.Name() != ".terraform.lock.hcl" {
				return nil
			}
			out = append(out, p)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	return out
}

// shortLockedProviders returns "provider (n h1: hashes)" for each provider in
// a lock file that records fewer than lockPlatforms h1: hashes.
func shortLockedProviders(src string) []string {
	var short []string
	for _, m := range lockProviderRe.FindAllStringSubmatch(src, -1) {
		if n := strings.Count(m[2], `"h1:`); n < lockPlatforms {
			short = append(short, fmt.Sprintf("%s (%d h1: hashes)", m[1], n))
		}
	}
	return short
}

func TestLockFilesRecordEveryPlatform(t *testing.T) {
	files := lockFiles(t)
	if len(files) == 0 {
		t.Fatal("no .terraform.lock.hcl found under the scan roots")
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if !lockProviderRe.Match(raw) {
			t.Errorf("%s: no provider blocks found", f)
		}
		for _, p := range shortLockedProviders(string(raw)) {
			t.Errorf("%s: %s, want %d (linux_amd64, linux_arm64, darwin_arm64, darwin_amd64)", f, p, lockPlatforms)
		}
	}
}
