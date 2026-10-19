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

if ! bash scripts/setup-git-secrets.sh > "$tmp/setup.log" 2>&1; then
    echo "scripts/setup-git-secrets.sh failed in the throwaway repo:" >&2
    cat "$tmp/setup.log" >&2
    exit 1
fi
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
# A clone set up before #328 has the bare detectors registered. A re-run must
# replace them with the word-wrapped form, not keep both.
BARE_AKIA='AKIA[0-9A-Z]{16}'
git config --add secrets.patterns "$BARE_AKIA"
if ! bash scripts/setup-git-secrets.sh > "$tmp/setup3.log" 2>&1; then
    echo "scripts/setup-git-secrets.sh failed with a pre-#328 bare detector registered:" >&2
    cat "$tmp/setup3.log" >&2
    exit 1
fi
if git config --get-all secrets.patterns | grep -qxF -- "$BARE_AKIA"; then
    echo "setup run left the pre-#328 bare detector registered: $BARE_AKIA" >&2
    exit 1
fi
if [ "$(git config --get-all secrets.patterns | wc -l)" -ne "$patterns_after" ]; then
    echo "setup run over a pre-#328 bare detector changed the pattern count" >&2
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

# git-secrets greps with -w, so a detector whose match starts or ends inside a
# word never fired (#328): a DSN's match ends right before its host name, and
# a key or keyword glued to other word characters does not start on a
# boundary. Each case below scanned clean before #328.
DSN_PW="hunter2""hunter2"
PG="postg""res://"
run_case "postgres DSN followed by its host" 1 q.env \
    "DATABASE_URL=${PG}app:${DSN_PW}@db.example.com:5432/app"
run_case "quoted postgres DSN with a bare host" 1 r.go \
    "dsn := \"${PG}app:${DSN_PW}@db\""
run_case "mysql DSN followed by its host" 1 s.txt \
    "mys""ql://root:${DSN_PW}@localhost/app"
run_case "mongodb DSN followed by its host" 1 t.txt \
    "mongo""db://app:${DSN_PW}@mongo.example.net:27017/app"
run_case "mongodb+srv DSN followed by its host" 1 u.txt \
    "mongo""db+srv://app:${DSN_PW}@cluster0.example.net/app"
run_case "password keyword after an underscore" 1 v.tf \
    "db_pass""word = \"${DSN_PW}\""
run_case "api key keyword after an underscore" 1 w.yaml \
    "openai_api""_key: \"${DSN_PW}\""
run_case "secret key keyword after an underscore" 1 x.py \
    "aws_secret""_key = '${DSN_PW}'"
run_case "access key glued between word characters" 1 y.txt \
    "x${KEY}x"
run_case "access key glued to an Azure connection string" 1 z.txt \
    "DefaultEndpoints""Protocol=https${KEY}"
run_case "GCP API key after a word character" 1 aa.txt \
    "x${AIZA}"
# Widening the match must not turn credential-less text into a finding.
run_case "postgres DSN with no password" 0 ab.env \
    "DATABASE_URL=${PG}app@db.example.com:5432/app"
run_case "postgres DSN with no user info" 0 ac.env \
    "DATABASE_URL=${PG}db.example.com:5432/app"
run_case "password variable reference, no literal" 0 ad.tf \
    "db_pass""word = var.db_password"

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
