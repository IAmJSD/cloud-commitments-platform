#!/usr/bin/env bash
# integration-test-selection.sh
#
# Prints, for each Go package that has integration tests, the package import
# path, a `go test -run` regex naming exactly those tests, and how many
# top-level tests that regex runs in the package:
#
#   <import path><TAB>^(TestA|TestB|...)$<TAB><count>
#
# The count can exceed the number of names: the in-package and the external
# (package foo_test) test files may each define a test of the same name, and
# -run, which matches names only, runs both. internal/analytics does today.
#
# Packages with none are not printed, so the caller skips them, compile
# included. CI's Integration Tests job uses this so it runs only the tests the
# Unit Tests job (`go test -race -short`) does not; before, it re-ran every
# unit test a second time under -tags=integration.
#
# A test is selected when either:
#
#   1. It is a top-level Test function in a *_test.go file that only compiles
#      with the `integration` tag. Build constraints are evaluated by the Go
#      toolchain, not parsed here: the file is in `go list -tags integration`'s
#      test files and not in untagged `go list`'s. That handles `&&`, `||`, `!`
#      and parentheses exactly as `go test` does. A file constrained to
#      `integration && foo` is excluded, as CI never sets foo; a
#      `!integration` file is excluded, as CI never compiles it.
#   2. It is in a file that compiles either way, and calls testing.Short(),
#      directly or through a function in the package's test files that does,
#      or through testutil.SkipIfShort. The Unit Tests job skips those
#      under -short, so this is the only job that runs them.
#
# The function scan relies on gofmt layout (the pre-commit go-fmt hook): a
# top-level declaration starts at column 0 and its body ends at a `}` in
# column 0.
#
# Usage:
#   integration-test-selection.sh [-C dir] [package pattern ...]
#       Packages default to ./... in dir (default: current directory).
#   integration-test-selection.sh --shard NAME SELECTION_FILE
#       Reads the shard spec from $INTEGRATION_SHARDS, checks that every
#       package in SELECTION_FILE (output of the first form) lands in exactly
#       one shard, then prints SELECTION_FILE's lines for shard NAME. The
#       spec has one shard per line, a name then package import paths
#       relative to the module path in $INTEGRATION_MODULE; `*` stands for
#       every selected package no other shard names:
#
#         migrations internal/database/postgres/migrations
#         rest internal/auth *
#
# Sourcing the script defines its functions without running anything (used by
# scripts/test-integration-test-selection.sh).

set -euo pipefail

# Escape regexp metacharacters. Go identifiers cannot contain any today; this
# keeps the output a literal match if that ever changes.
regex_escape() {
  printf '%s' "$1" | sed 's/[][\\.^$*+?(){}|/]/\\&/g'
}

# Builds ^(a|b|...)$ from names on stdin, one per line, byte-sorted and
# de-duplicated, so the output does not depend on the locale.
names_to_regex() {
  local names
  names="$(LC_ALL=C sort -u | while IFS= read -r n; do
    [[ -n "$n" ]] && regex_escape "$n" && echo
  done | paste -sd'|' -)"
  [[ -n "$names" ]] && printf '^(%s)$\n' "$names"
}

# Prints "<name>\t<definitions>" for top-level Test functions defined in a
# file listed in $selectable (space-separated):
#   mode 1: every such Test function.
#   mode 0: only those that reach testing.Short(), see header.
# The files to scan are every test file of the package, so helpers in other
# files are seen, and <definitions> counts every file defining the name.
scan_tests() {
  local mode="$1" selectable="$2"
  shift 2
  awk -v mode="$mode" -v selectable="$selectable" '
    BEGIN { n = split(selectable, s, " "); for (i = 1; i <= n; i++) pick[s[i]] = 1 }
    function close_fn() { cur = "" }
    # A new column-0 declaration or a column-0 closing brace ends the body.
    /^(func|type|var|const|import)[ (]/ { close_fn() }
    /^func / {
      line = $0
      if (line ~ /^func \(/) {          # method: track the body, never select
        sub(/^func \([^)]*\) */, "", line)
        cur = "method:" FNR ":" FILENAME
      } else {
        sub(/^func /, "", line)
        cur = line; sub(/[[(].*/, "", cur)
      }
      # Test<X> where X is not a lower-case letter, taking *testing.T.
      if (cur ~ /^Test/ && cur !~ /^Test[a-z]/ && line ~ /^[^(]*\([A-Za-z0-9_]+ \*testing\.T\)/) {
        defs[cur]++
        # Per definition: the same name may also be defined in a file that
        # is not selectable (in-package vs external test package).
        if (FILENAME in pick) pickable[cur] = 1
      }
    }
    cur != "" {
      code = $0
      sub(/\/\/.*/, "", code)            # drop line comments
      gsub(/"([^"\\]|\\.)*"/, "\"\"", code) # and string literal contents
      body[cur] = body[cur] " " code
    }
    /^}/ { close_fn() }
    END {
      for (f in body) {
        if (body[f] ~ /(^|[^A-Za-z0-9_])testing\.Short\(\)/ || body[f] ~ /(^|[^A-Za-z0-9_])SkipIfShort\(/) short[f] = 1
      }
      # Propagate through plain functions until nothing changes.
      do {
        changed = 0
        for (f in body) {
          if (f in short) continue
          for (g in short) {
            if (g ~ /^method:/) continue
            if (body[f] ~ ("(^|[^A-Za-z0-9_.])" g "\\(")) { short[f] = 1; changed = 1; break }
          }
        }
      } while (changed)
      for (f in pickable) {
        if (mode == 1 || (f in short)) print f "\t" defs[f]
      }
    }
  ' "$@"
}

# Prints "<import path>\t<dir>\t<space-separated test files>" per package.
# Flags first, then packages: list_test_files [go list flag ...] -- pkg ...
list_test_files() {
  local flags=()
  while [[ $# -gt 0 && "$1" != "--" ]]; do flags+=("$1"); shift; done
  shift
  go list "${flags[@]}" -f '{{.ImportPath}}{{"\t"}}{{.Dir}}{{"\t"}}{{join .TestGoFiles " "}} {{join .XTestGoFiles " "}}' -- "$@"
}

select_tests() {
  local dir="."
  if [[ "${1:-}" == "-C" ]]; then
    dir="$2"
    shift 2
  fi
  [[ $# -gt 0 ]] || set -- ./...
  cd "$dir"

  local tagged untagged
  tagged="$(list_test_files -tags integration -- "$@")"
  # -e: a package whose every file is integration-only has no untagged build;
  # it is simply absent here, and the tagged listing above already failed on
  # any real error.
  untagged="$(list_test_files -e -- "$@" 2>/dev/null || true)"

  local pkg pdir files
  while IFS=$'\t' read -r pkg pdir files; do
    [[ -n "$pkg" ]] || continue
    local plain=" " f integ="" common="" all=()
    plain+="$(awk -F'\t' -v p="$pkg" '$1 == p { print $3 }' <<<"$untagged") "
    for f in $files; do
      all+=("$pdir/$f")
      if [[ "$plain" == *" $f "* ]]; then
        common+=" $pdir/$f"
      else
        integ+=" $pdir/$f"
      fi
    done
    [[ ${#all[@]} -gt 0 ]] || continue
    local found regex
    found="$({
      [[ -z "$integ" ]] || scan_tests 1 "$integ" "${all[@]}"
      [[ -z "$common" ]] || scan_tests 0 "$common" "${all[@]}"
    } | LC_ALL=C sort -u)"
    [[ -n "$found" ]] || continue
    regex="$(cut -f1 <<<"$found" | names_to_regex)"
    printf '%s\t%s\t%s\n' "$pkg" "$regex" "$(awk -F'\t' '{ n += $2 } END { print n }' <<<"$found")"
  done <<<"$tagged"
}

# --shard NAME SELECTION_FILE; see the header.
select_shard() {
  local want="$1" selection="$2"
  local spec="${INTEGRATION_SHARDS:?INTEGRATION_SHARDS must hold the shard spec}"
  local module="${INTEGRATION_MODULE:?INTEGRATION_MODULE must hold the module path}"
  if [[ ! "$module" =~ ^[^[:space:]]+$ ]]; then
    echo "INTEGRATION_MODULE must be one module path, got: $module" >&2
    return 2
  fi
  awk -F'\t' -v want="$want" -v module="$module" -v spec="$spec" '
    BEGIN {
      nl = split(spec, lines, "\n")
      for (i = 1; i <= nl; i++) {
        nf = split(lines[i], w, " ")
        if (nf == 0) continue
        if (w[1] in shard_seen) { err = err "\n  shard " w[1] " is defined twice"; continue }
        shard_seen[w[1]] = 1
        if (nf == 1) { err = err "\n  shard " w[1] " names no packages" }
        for (j = 2; j <= nf; j++) {
          if (w[j] == "*") { rest_count++; rest = w[1]; continue }
          p = module "/" w[j]
          if (p in named) { err = err "\n  " p " is in shard " named[p] " and shard " w[1]; continue }
          named[p] = w[1]
        }
      }
      if (rest_count != 1) err = err "\n  the spec needs exactly one * (catch-all) entry, found " rest_count + 0
      if (!(want in shard_seen)) err = err "\n  shard " want " is not in the spec"
    }
    { selected[$1] = $0; order[++n] = $1 }
    END {
      for (p in named) {
        if (!(p in selected)) err = err "\n  " p " (shard " named[p] ") has no integration tests; remove it from the spec"
      }
      for (i = 1; i <= n; i++) {
        p = order[i]
        s = (p in named) ? named[p] : rest
        if (s == "") err = err "\n  " p " has integration tests but no shard"
        count[s]++
        if (s == want) out[++m] = selected[p]
      }
      for (s in shard_seen) if (!(s in count)) err = err "\n  shard " s " has no packages with integration tests"
      if (err != "") { printf "integration shard spec is inconsistent:%s\n", err > "/dev/stderr"; exit 1 }
      for (i = 1; i <= m; i++) print out[i]
    }
  ' "$selection"
}

main() {
  if [[ "${1:-}" == "--shard" ]]; then
    [[ $# -eq 3 ]] || { echo "usage: $0 --shard NAME SELECTION_FILE" >&2; return 2; }
    select_shard "$2" "$3"
  else
    select_tests "$@"
  fi
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
