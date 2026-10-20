#!/bin/bash
# Self-test for the git-secrets allowlist (#1972). Exercises the real
# scripts/setup-git-secrets.sh and .gitallowed in a throwaway repo, both
# directly via `git secrets --scan --cached` and through the installed
# pre-commit hook, in both directions: fixtures that must be caught, and
# fixtures that must scan clean.
#
# Fixture safety: every positive fixture below is a secret-shaped string
# that this script itself, and the repo's detect-private-key / git-secrets
# pre-commit hooks, would flag if it appeared as a contiguous literal. Each
# is assembled at runtime from adjacent string literals so this file's own
# source never contains a scannable token. Where a fixture shows \n below,
# it is the two literal characters backslash and n, as in a Go string
# literal, never an actual newline.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if ! command -v git-secrets >/dev/null 2>&1; then
    echo "git-secrets not found on PATH; install it before running this test" >&2
    exit 1
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/home"
# Isolate HOME: --register-aws reads ~/.aws/credentials, and git-secrets
# echoes the whole joined pattern list on a regex error, which would print
# real credentials into this log.
export HOME="$tmp/home"

git init -q "$tmp/repo"
mkdir -p "$tmp/repo/scripts"
cp "$REPO_ROOT/scripts/setup-git-secrets.sh" "$tmp/repo/scripts/setup-git-secrets.sh"
cp "$REPO_ROOT/.gitallowed" "$tmp/repo/.gitallowed"
cd "$tmp/repo"

# A clone set up before #813 still has the old negated-class secret-key pattern
# registered. Seed it, so the check below proves a setup run removes it (#295).
LEGACY_SECRET_KEY='[^A-Za-z0-9/+=]{40}[^A-Za-z0-9/+=]'
git config --add secrets.patterns "$LEGACY_SECRET_KEY"

if ! bash scripts/setup-git-secrets.sh > "$tmp/setup.log" 2>&1; then
    echo "scripts/setup-git-secrets.sh failed in the throwaway repo:" >&2
    cat "$tmp/setup.log" >&2
    exit 1
fi
legacy_left=$(git config --get-all secrets.patterns | grep -cxF -- "$LEGACY_SECRET_KEY" || true)
# A second run must succeed and register nothing twice: `git secrets --add`
# exits 1 on an already-registered pattern, which used to abort a re-run
# half-configured.
patterns_before=$(git config --get-all secrets.patterns | wc -l)
if ! bash scripts/setup-git-secrets.sh > "$tmp/setup2.log" 2>&1; then
    echo "scripts/setup-git-secrets.sh is not idempotent; second run failed:" >&2
    cat "$tmp/setup2.log" >&2
    exit 1
fi
patterns_after=$(git config --get-all secrets.patterns | wc -l)
if [ "$patterns_before" -ne "$patterns_after" ]; then
    echo "second setup run changed the pattern count: $patterns_before -> $patterns_after" >&2
    exit 1
fi
# Same HOME-isolation reason: drop the provider before any further scan.
git config --unset-all secrets.providers

# The pre-commit hook ignores the args git would pass it and recomputes its
# own file list from the index (diff against HEAD, or the empty tree if
# there is no HEAD yet, which is always true in this never-committed
# throwaway repo). So it scans every currently staged path, not just
# $relpath -- calling it with no args, as git itself does, is correct.
HOOK_DIR="$(git rev-parse --git-dir)/hooks"

pass=0
fail=0

# Records one pass/fail against $expected, printing a FAIL line labeled
# $label on mismatch. Shared by both scan paths in run_case/run_staged_case
# so a direct-scan/hook disagreement surfaces as its own labeled failure
# instead of being silently reconciled.
check_result() {
    local label=$1 expected=$2 actual=$3
    if [ "$actual" -eq "$expected" ]; then
        pass=$((pass + 1))
    else
        fail=$((fail + 1))
        echo "FAIL: $label (expected exit $expected, got $actual)" >&2
    fi
}

# Writes $content to $relpath, stages it, scans it two ways -- directly via
# `git secrets --scan --cached` (exact file argument) and via the installed
# pre-commit hook (the real path a developer's commit takes) -- compares
# each exit code to $expected, then unstages and removes it.
run_case() {
    local label=$1 expected=$2 relpath=$3 content=$4
    mkdir -p "$(dirname "$relpath")"
    printf '%s\n' "$content" >"$relpath"
    git add "$relpath"
    local actual=0
    git secrets --scan --cached "$relpath" >/dev/null 2>&1 || actual=$?
    check_result "$label (direct scan)" "$expected" "$actual"
    local hook_actual=0
    "$HOOK_DIR/pre-commit" >/dev/null 2>&1 || hook_actual=$?
    check_result "$label (installed hook)" "$expected" "$hook_actual"
    git rm -q --cached "$relpath"
    rm -f "$relpath"
}

# Same as run_case, for a file already on disk (the copied setup script and
# .gitallowed), left staged afterward like any other tracked file would be.
run_staged_case() {
    local label=$1 expected=$2 relpath=$3
    git add "$relpath"
    local actual=0
    git secrets --scan --cached "$relpath" >/dev/null 2>&1 || actual=$?
    check_result "$label (direct scan)" "$expected" "$actual"
    local hook_actual=0
    "$HOOK_DIR/pre-commit" >/dev/null 2>&1 || hook_actual=$?
    check_result "$label (installed hook)" "$expected" "$hook_actual"
}

# Fixtures, assembled from adjacent literals so this file scans clean.
KEY="AKIA""0123456789ABCDEF"
PEM="-----BEGIN ""PRIVATE KEY-----"
PW="pass""word = \"hunter2hunter2\""
AIZA="AIza$(head -c 35 /dev/zero | tr '\0' X)"

# Must be caught (exit 1): the #1972 hole and the coverage this PR adds.
run_case "terraform resource with a real-shaped key" 1 a.tf \
    'resource "aws_iam_access_key" "x" { key = "'"$KEY"'" }'
run_case "terraform var reference alongside a key" 1 b.tf \
    'secret = var.x # '"$KEY"
run_case "go test const with a key" 1 c_test.go \
    'const k = "'"$KEY"'"'
run_case "testdata fixture with a key" 1 testdata/creds.txt \
    'key='"$KEY"
run_case "placeholder line with a key" 1 d.txt \
    'placeholder '"$KEY"
run_case "untruncated PKCS8 body" 1 e.json \
    '"private_key": "'"$PEM"'\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC\n"'
run_case "corrected GCP API key range" 1 f.txt \
    "$AIZA"
run_case "password with real spaces around the separator" 1 n.txt \
    "$PW"
run_case "secret before an otherwise-allowed truncated PEM, same line" 1 l.go \
    'k := "'"$KEY"'"; PrivateKey: "'"$PEM"'\n...",'
run_case "secret after an otherwise-allowed truncated PEM, same line" 1 m.go \
    '"private_key": "'"$PEM"'\nMIIEvQIBADANBg...\n'"$KEY"'"'

check_result "setup removes the legacy negated secret-key pattern" 0 "$legacy_left"

# Bare AWS secret access keys (#295): 40 characters with no name in front that
# --register-aws or the "AWS Credentials" pattern could anchor on. Each key is the
# filler below with one upper-case/digit pair written over it, so this file never
# holds a key-shaped literal. The offsets reach every window the detector is
# split into, including the first and last character a pair can start at.
FILL=$(head -c 50 /dev/zero | tr '\0' x)
# key_with LENGTH OFFSET PAIR: LENGTH filler characters with PAIR at OFFSET.
key_with() {
    printf '%s%s%s' "${FILL:0:$2}" "$3" "${FILL:0:$(($1 - $2 - ${#3}))}"
}
for offset in 0 4 5 9 10 14 15 19 20 24 25 29 30 34 35 38; do
    run_case "bare key, Q7 pair at offset $offset" 1 bare.txt "$(key_with 40 "$offset" Q7)"
done
run_case "bare key, digit then upper-case" 1 bare.txt "$(key_with 40 20 7Q)"
run_case "bare key, pair one character apart" 1 bare.txt "$(key_with 40 37 Q+7)"
run_case "bare key, reversed pair one character apart" 1 bare.txt "$(key_with 40 0 7/Q)"
run_case "unquoted YAML value" 1 creds.yaml "credential: $(key_with 40 12 Q7)"
run_case "env-style assignment" 1 creds.env "TOKEN=$(key_with 40 12 Q7)"
run_case "quoted JSON value" 1 creds.json "{\"token\": \"$(key_with 40 12 Q7)\"}"
run_case "key followed by a comma" 1 keys.go "keys := []string{\"$(key_with 40 30 Q7)\", other}"
run_case "key followed by a word after a dash" 1 keys.txt "$(key_with 40 30 Q7)-old"

# Key-shaped runs the detector must leave alone: the box-drawing and dash banner
# lines the legacy pattern flagged, commit SHAs, identifiers, base64 padding, and
# runs one character too short or too long to be a key.
SHA="3c0f8ac94048a2c3""6fce5ccddee54e6c4849a5cd"
run_case "commit SHA" 0 sha.yml "      - uses: actions/checkout@$SHA # v4"
run_case "box-drawing banner" 0 banner.sh "echo \"$(printf '\342\224\201%.0s' $(seq 41))\""
run_case "dash banner" 0 banner.txt "$(head -c 41 /dev/zero | tr '\0' -)"
run_case "long CamelCase test name" 0 names_test.go \
    "// TestSummarizeRecommendationsWithCoverage checks the summary."
run_case "key-shaped name after an underscore" 0 fn_test.go \
    "// TestX_$(key_with 40 12 Q7) checks it."
run_case "key-shaped function name" 0 fn_test.go "func $(key_with 40 12 Q7)(t *testing.T) {"
run_case "40-character run followed by base64 padding" 0 go.sum \
    "example.com/mod v1.0.0 h1:$(key_with 40 14 Q7)="
run_case "39-character run" 0 short.txt "$(key_with 39 12 Q7)"
run_case "41-character run" 0 long.txt "$(key_with 41 12 Q7)"

# Negative controls for the setup-script allowlist entries below, each
# closing one instance of the same class of hole (#1972 reintroduced at
# the scale of one file) that a narrower-but-still-wrong anchor leaves
# open. Both mutate the tracked copy of the setup script in place at its
# real path, since the allowlist entries are anchored on that path, and
# restore the pristine copy afterward so the later self-scan case below
# sees it intact.

# A secret appended to a DIFFERENT git-secrets registration line in that
# file (the GCP one, not one of the three that legitimately self-match)
# must still be caught. A prefix-only entry anchored on just "git secrets
# --add '" would hide this.
run_case "secret appended to an unrelated registration line stays caught" 1 \
    scripts/setup-git-secrets.sh \
    "$(sed "s/# GCP API Key\$/# GCP API Key ${KEY}/" "$REPO_ROOT/scripts/setup-git-secrets.sh")"
cp "$REPO_ROOT/scripts/setup-git-secrets.sh" scripts/setup-git-secrets.sh

# A secret appended to the END of one of the three self-matching lines
# THEMSELVES must also still be caught. An entry anchored on the path and
# pattern but missing a trailing $ would stop matching at the end of the
# pattern it names and let anything appended after it through, which is a
# narrower version of the same hole the entry above closes.
run_case "secret appended to a self-matching line itself stays caught" 1 \
    scripts/setup-git-secrets.sh \
    "$(sed "s/# PostgreSQL\$/# PostgreSQL ${KEY}/" "$REPO_ROOT/scripts/setup-git-secrets.sh")"
cp "$REPO_ROOT/scripts/setup-git-secrets.sh" scripts/setup-git-secrets.sh

# Must scan clean (exit 0): what the tree contains, and what the allowlist
# must keep clean.
run_staged_case "setup script's own self-matching lines" 0 scripts/setup-git-secrets.sh
run_case "GCP type marker alone, no key material" 0 g.json \
    '{"type": "service_account", "project_id": "p"}'
run_case "truncated PEM in a struct literal" 0 i.go \
    'PrivateKey: "'"$PEM"'\n...",'
run_case "truncated PEM in a JSON literal" 0 j.go \
    '"private_key": "'"$PEM"'\nMIIEvQIBADANBg...\n",'
run_case "PEM word fragments are not a real header" 0 k.txt \
    'this is PRIVATE data
algo EC here
-----BEGIN CERTIFICATE-----'
run_staged_case ".gitallowed self-scan" 0 .gitallowed

# Structural check (#804): every active .gitallowed entry must be anchored
# on the scanner's "path:line:content" output with ^ and $, so no entry can
# whitelist a whole line by containing a bare token.
unanchored=$(grep -vE '^[[:space:]]*(#|$)' "$REPO_ROOT/.gitallowed" | grep -vE '^\^.*\$$' || true)
if [ -n "$unanchored" ]; then
    fail=$((fail + 1))
    echo "FAIL: unanchored .gitallowed entries:" >&2
    echo "$unanchored" >&2
else
    pass=$((pass + 1))
fi

# Placeholder-shaped account IDs next to a real-shaped key must be caught.
ACCT="1234""56789012"
run_case "account-id placeholder with a key on the same line" 1 o.go \
    'accountId := "'"$ACCT"'"; k := "'"$KEY"'"'
run_case "UUID placeholder with a password on the same line" 1 p.go \
    'id := "11111111-1111-1111-1111-111111111111"; '"$PW"

# CI-equivalent check: the pre-commit framework hands git-secrets explicit
# file names (plain grep), unlike a no-argument scan (git grep), which
# differ on regex features such as \s. Scan every tracked file of the real
# repo that way, under the same registered patterns.
git -C "$REPO_ROOT" ls-files -z | tar -C "$REPO_ROOT" --null -T - -cf - | tar -C "$tmp/repo" -xf -
file_scan=0
git ls-files -z --others --cached | xargs -0 git secrets --scan >"$tmp/file-scan.log" 2>&1 || file_scan=$?
[ "$file_scan" -eq 0 ] || grep -E "^[^ ]+:[0-9]+:" "$tmp/file-scan.log" | cut -c1-200 >&2
check_result "file-argument scan of all tracked files" 0 "$file_scan"

echo "$pass PASS, $fail FAIL"
[ "$fail" -eq 0 ]
