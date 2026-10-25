#!/usr/bin/env bash
# test-integration-test-selection.sh
#
# Self-test for integration-test-selection.sh, against fixture packages built
# in a temp dir. Both directions matter: a test the selector misses is never
# run by CI at all (the Unit Tests job skips integration files), and a test it
# over-selects is run twice. Needs Go on PATH; the fixtures are stdlib-only.
#
# Exits 0 when all cases pass; exits 1 on any failure.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SELECT="${SCRIPT_DIR}/integration-test-selection.sh"
# shellcheck source=scripts/integration-test-selection.sh
source "$SELECT"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
export GOWORK=off GOFLAGS=-mod=mod GOTOOLCHAIN=local

pass=0
fail=0
ok() { echo "PASS: $1"; (( pass++ )) || true; }
bad() { echo "FAIL: $1"; (( fail++ )) || true; }

check_eq() { # label expected actual
  if [[ "$2" == "$3" ]]; then ok "$1"; else
    bad "$1"
    diff <(printf '%s\n' "$2") <(printf '%s\n' "$3") | sed 's/^/    /' || true
  fi
}

M=example.com/fx
FX="$WORK/fx"
w() { mkdir -p "$(dirname "$FX/$1")"; cat >"$FX/$1"; }

w go.mod <<<"module $M

go 1.22"

# --- tagged: integration-only files (in-package and external), next to an
# untagged file whose tests must not be selected.
w tagged/tagged.go <<<"package tagged"
w tagged/integration_test.go <<'EOF'
//go:build integration

package tagged

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) { os.Exit(m.Run()) }

func TestTaggedA(t *testing.T) {}

func TestTaggedB(t *testing.T) {
	t.Run("sub", func(t *testing.T) {})
}

func Test_Underscore(t *testing.T) {}

func TestÜber(t *testing.T) {}

func TestdataDir() string { return "testdata" } // not a test: lower-case after Test

func tagsHelper(t *testing.T) {} // not a test: no Test prefix

func BenchmarkTagged(b *testing.B) {}
EOF
w tagged/external_integration_test.go <<'EOF'
//go:build integration

package tagged_test

import "testing"

func TestExternal(t *testing.T) {}
EOF
w tagged/unit_test.go <<'EOF'
package tagged

import "testing"

// Shares the TestTaggedA prefix: the anchored regex must not select it.
func TestTaggedAB(t *testing.T) {}

func TestUnit(t *testing.T) {}
EOF

# --- exprs: constraint expressions. Only what compiles under exactly
# -tags=integration, and not without it, is integration-only.
w exprs/exprs.go <<<"package exprs"
w exprs/andfoo_test.go <<'EOF'
//go:build integration && foo

package exprs

import "testing"

func TestAndFoo(t *testing.T) {}
EOF
w exprs/orfoo_test.go <<'EOF'
//go:build integration || foo

package exprs

import "testing"

func TestOrFoo(t *testing.T) {}
EOF
w exprs/andnotfoo_test.go <<'EOF'
//go:build (integration && !foo) || ignore

package exprs

import "testing"

func TestAndNotFoo(t *testing.T) {}
EOF
w exprs/legacy_test.go <<'EOF'
// +build integration

package exprs

import "testing"

func TestLegacyPlusBuild(t *testing.T) {}
EOF

# --- negated: a !integration file never compiles in the integration job.
# TestSameName is defined in both halves; the integration half is selected.
w negated/negated.go <<<"package negated"
w negated/not_integration_test.go <<'EOF'
//go:build !integration

package negated

import "testing"

func TestNotIntegration(t *testing.T) {}

func TestSameName(t *testing.T) {}
EOF
w negated/integration_test.go <<'EOF'
//go:build integration

package negated

import "testing"

func TestSameName(t *testing.T) {}
EOF

# --- short: untagged tests gated on testing.Short(), directly, through a
# local helper (two levels deep), and through testutil.SkipIfShort.
w testutil/testutil.go <<'EOF'
package testutil

import "testing"

func SkipIfShort(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
}
EOF
w short/short.go <<<"package short"
w short/short_test.go <<'EOF'
package short

import (
	"testing"

	"example.com/fx/testutil"
)

func TestShortDirect(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
}

func TestShortViaHelper(t *testing.T) {
	requireLong(t)
}

func TestShortViaTestutil(t *testing.T) {
	testutil.SkipIfShort(t)
}

func TestShortInClosure(t *testing.T) {
	t.Run("sub", func(t *testing.T) {
		if testing.Short() {
			t.Skip("short")
		}
	})
}

// TestNotShort mentions testing.Short() only in this comment and a string.
func TestNotShort(t *testing.T) {
	_ = "testing.Short()" // testing.Short()
}

func TestPlain(t *testing.T) {}
EOF
w short/helpers_test.go <<'EOF'
package short

import "testing"

func requireLong(t *testing.T) { reallyRequireLong(t) }

func reallyRequireLong(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
}

// A method calling testing.Short() does not make its callers Short-gated by
// name: requireLong above is a plain function, this is not.
type fixture struct{}

func (fixture) requireLongToo(t *testing.T) bool { return testing.Short() }
EOF

# --- dup: one name in both the in-package and the external test package,
# once with the tagged half in-package and once the other way round. -run
# matches names, so both halves run: the count says 4.
w dup/dup.go <<<"package dup"
w dup/internal_test.go <<'EOF'
package dup

import "testing"

func TestDupA(t *testing.T) {}
EOF
w dup/external_integration_test.go <<'EOF'
//go:build integration

package dup_test

import "testing"

func TestDupA(t *testing.T) {}
EOF
w dup/internal_integration_test.go <<'EOF'
//go:build integration

package dup

import "testing"

func TestDupB(t *testing.T) {}
EOF
w dup/external_test.go <<'EOF'
package dup_test

import "testing"

func TestDupB(t *testing.T) {}
EOF

# --- none: unit tests only, and a package without any test file.
w none/none.go <<<"package none"
w none/none_test.go <<'EOF'
package none

import "testing"

func TestNone(t *testing.T) {}
EOF
w notests/notests.go <<<"package notests"

# ---------------------------------------------------------------------------
# Selection.

expected="$(printf '%s\n' \
  "$M/dup	^(TestDupA|TestDupB)\$	4" \
  "$M/exprs	^(TestAndNotFoo|TestLegacyPlusBuild|TestOrFoo)\$	3" \
  "$M/negated	^(TestSameName)\$	1" \
  "$M/short	^(TestShortDirect|TestShortInClosure|TestShortViaHelper|TestShortViaTestutil)\$	4" \
  "$M/tagged	^(TestExternal|TestTaggedA|TestTaggedB|Test_Underscore|TestÜber)\$	5")"
actual="$(LC_ALL=C bash "$SELECT" -C "$FX" ./... 2>&1)" || true
check_eq "selection over the fixture module" "$expected" "$actual"

if [[ "$actual" != *"$M/none"* && "$actual" != *"$M/notests"* && "$actual" != *"$M/testutil"* ]]; then
  ok "packages with no integration tests are skipped"
else
  bad "packages with no integration tests are skipped"
fi

check_eq "explicit package list limits the output" \
  "$M/negated	^(TestSameName)\$	1" \
  "$(LC_ALL=C bash "$SELECT" -C "$FX" ./negated ./none 2>&1 || true)"

# What go test actually runs with the regex: the authority on anchoring and
# escaping, not a string comparison.
listed="$(cd "$FX" && go test -tags integration -list \
  "$(awk -F'\t' -v p="$M/tagged" '$1 == p { print $2 }' <<<"$actual")" ./tagged 2>&1 |
  grep -v '^ok' | LC_ALL=C sort || true)"
check_eq "go test -run selects exactly the tagged tests (not TestTaggedAB)" \
  "$(printf '%s\n' TestExternal TestTaggedA TestTaggedB Test_Underscore TestÜber)" "$listed"

listed="$(cd "$FX" && go test -list \
  "$(awk -F'\t' -v p="$M/short" '$1 == p { print $2 }' <<<"$actual")" ./short 2>&1 |
  grep -v '^ok' | LC_ALL=C sort || true)"
check_eq "go test -run selects exactly the Short-gated tests" \
  "$(printf '%s\n' TestShortDirect TestShortInClosure TestShortViaHelper TestShortViaTestutil)" "$listed"

listed="$(cd "$FX" && go test -tags integration -list \
  "$(awk -F'\t' -v p="$M/dup" '$1 == p { print $2 }' <<<"$actual")" ./dup 2>&1 |
  grep -v '^ok' | LC_ALL=C sort || true)"
check_eq "the count matches what go test runs for a name in both test packages" \
  "$(printf '%s\n' TestDupA TestDupA TestDupB TestDupB)" "$listed"

rc=0
bash "$SELECT" -C "$FX" ./doesnotexist >/dev/null 2>&1 || rc=$?
if [[ "$rc" -ne 0 ]]; then ok "a package go list cannot load fails the script"; else bad "a package go list cannot load fails the script"; fi

# ---------------------------------------------------------------------------
# Regex escaping. Go identifiers cannot hold metacharacters today, so this is
# tested on the function directly, then checked against Go's own regexp.

# shellcheck disable=SC2016 # literal $ and \ on purpose
meta='a.b*c+d?e(f)g[h]i{j}k|l^m$n\o/p'
escaped="$(regex_escape "$meta")"
# shellcheck disable=SC2016 # literal $ and \ on purpose
check_eq "regex_escape escapes every metacharacter" \
  'a\.b\*c\+d\?e\(f\)g\[h\]i\{j\}k\|l\^m\$n\\o\/p' "$escaped"
check_eq "names_to_regex anchors, sorts and de-duplicates" \
  '^(Test\.C|TestA|TestB)$' "$(printf '%s\n' TestB TestA Test.C TestA | LC_ALL=C names_to_regex)"

cat >"$WORK/re_test.go" <<'EOF'
package re

import (
	"os"
	"regexp"
	"testing"
)

func TestEscapedIsLiteral(t *testing.T) {
	re := regexp.MustCompile("^(" + os.Getenv("ESCAPED") + ")$")
	if !re.MatchString(os.Getenv("META")) {
		t.Fatalf("%s does not match %q", re, os.Getenv("META"))
	}
	if re.MatchString("aXb*c+d?e(f)g[h]i{j}k|l^m$n\\o/p") {
		t.Fatal(". was not escaped")
	}
}
EOF
printf 'module re\n\ngo 1.22\n' >"$WORK/go.mod"
if (cd "$WORK" && META="$meta" ESCAPED="$escaped" go test ./re_test.go >/dev/null 2>&1); then
  ok "escaped text is a literal match in Go's regexp"
else
  bad "escaped text is a literal match in Go's regexp"
fi

echo "$pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
