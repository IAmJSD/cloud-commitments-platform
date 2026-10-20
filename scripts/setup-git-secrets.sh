#!/bin/bash
# Setup git-secrets to prevent committing sensitive information

set -e

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

echo "========================================"
echo "Git Secrets Setup"
echo "========================================"
echo ""

# Check if git-secrets is installed
if ! command -v git-secrets &> /dev/null; then
    echo -e "${RED}✗ git-secrets not installed${NC}"
    echo ""
    echo "Installation instructions:"
    echo "  macOS: brew install git-secrets"
    echo "  Linux: git clone https://github.com/awslabs/git-secrets.git && cd git-secrets && make install"
    echo ""
    exit 1
fi

echo -e "${GREEN}✓ git-secrets is installed${NC}"
echo ""

# Install git hooks
#
# git-secrets 1.3.0's install_hook() writes and chmods the hook file, then
# reports success via a `say` call. `say` was never its own function:
# git-secrets sources git's git-sh-setup and relied on `say` being defined
# there (introduced in git-sh-setup.sh in 2009). Git removed it in commit
# 5b893f7d81 ("git-sh-setup.sh: remove 'say' function, change last users"),
# first shipped in Git 2.38 (2022), because it was undocumented and unused
# within git's own tree, breaking git-secrets as an unintended side effect
# of a git upgrade rather than a git-secrets regression. On macOS the bare
# `say` call resolves to /usr/bin/say (the text-to-speech binary) instead
# and exits 0 by accident; on Linux (or on a new-enough git anywhere) there
# is no such fallback, so it's "command not found" and `git secrets
# --install -f` returns non-zero even though every hook file was already
# written correctly. Define `say` as a no-op here and export it so the
# exported function is visible in the git-secrets child process on both
# platforms, making the real hook-writing exit status the one that reaches
# the check below.
say() { :; }
export -f say

echo "Installing git-secrets hooks..."
if git secrets --install -f; then
    echo -e "${GREEN}✓ Git hooks installed${NC}"
else
    echo -e "${RED}✗ Failed to install git hooks${NC}"
    exit 1
fi

# The no-op say() above means "git secrets --install -f" returning success no
# longer proves anything: with say() removed the real signal was its exit
# status, but say() itself is a no-op that returns success regardless of
# whether install_hook actually wrote the file (e.g. a permissions error on
# chmod). Verify what the command was supposed to do instead: each hook file
# it writes exists, is executable, and contains the git-secrets invocation.
git_dir="$(git rev-parse --git-dir)"
hooks_ok=1
for hook_spec in "commit-msg:commit_msg_hook" "pre-commit:pre_commit_hook" "prepare-commit-msg:prepare_commit_msg_hook"; do
    hook_name="${hook_spec%%:*}"
    hook_cmd="${hook_spec##*:}"
    hook_path="${git_dir}/hooks/${hook_name}"
    if [ -d "${git_dir}/hooks/${hook_name}.d" ]; then
        hook_path="${git_dir}/hooks/${hook_name}.d/git-secrets"
    fi
    if [ ! -x "${hook_path}" ] || ! grep -qF "git secrets --${hook_cmd} -- \"\$@\"" "${hook_path}"; then
        echo -e "${RED}✗ ${hook_name} hook missing, not executable, or doesn't invoke git-secrets: ${hook_path}${NC}"
        hooks_ok=0
    fi
done
if [ "${hooks_ok}" -ne 1 ]; then
    echo -e "${RED}✗ Hook verification failed; git-secrets would not actually run on commit${NC}"
    exit 1
fi
echo -e "${GREEN}✓ Verified all three hooks are installed and executable${NC}"

# Register AWS secret patterns
echo ""
echo "Registering AWS secret patterns..."
git secrets --register-aws

# Add custom patterns
echo ""
echo "Adding custom secret patterns..."

# git-secrets scans with a hard-coded `grep -w` (git grep -nwHEI, or grep -nwHEI
# for file arguments), so a match only counts if it starts and ends on a word
# boundary. Most detectors below don't on real text: a DSN detector's match ends
# at the "@", right before the first letter of the host, and "password" in
# db_password starts after "_". Those lines scanned clean (#328). So each
# detector is registered with the word characters around it taken in, which
# always reach a boundary: the registered form fires wherever the detector
# itself matches.
#
# A pattern written as (^|...)...(...|$) spells out its own bounds, so it is
# registered as is: wrapping it would match the same lines, but the extra
# leading [A-Za-z0-9_]* makes macOS's regex engine noticeably slower.
word_chars='[A-Za-z0-9_]*'
# `git secrets --add` exits 1 when the pattern is already registered, which under
# `set -e` aborted a re-run at the first custom pattern and skipped the rest.
# Skip patterns already present so the script is idempotent; a real failure
# (for example an invalid regex) still stops it. A clone set up before #328 has
# the bare detector registered; drop it, since the wrapped form covers it.
add_pattern() {
    local pattern="${word_chars}($1)${word_chars}"
    case "$1" in
        '(^|'*'|$)') pattern="$1" ;;
    esac
    if [ "${pattern}" != "$1" ] \
        && git config --local --get-all secrets.patterns | grep -qxF -- "$1"; then
        git config --local --unset-all --fixed-value secrets.patterns "$1"
    fi
    if git config --get-all secrets.patterns | grep -qxF -- "${pattern}"; then
        return 0
    fi
    git secrets --add "${pattern}"
}

# AWS patterns
add_pattern 'AKIA[0-9A-Z]{16}'                                    # AWS Access Key ID

# AWS Secret Access Key on its own (#295). --register-aws and the "AWS Credentials"
# pattern below only match a key assigned to an aws/secret-style name, so a bare key
# (a pasted credential, an unquoted YAML value) needs its own detector.
#
# A key is exactly 40 characters from [A-Za-z0-9/+]. Matching just that is too noisy:
# commit SHAs, long CamelCase test names and paths fill this repo with such runs. A
# random key almost always has an upper-case letter next to, or one character away
# from, a digit (about 0.3% of keys do not, and slip through), and those runs rarely
# do, so a match must contain that pair, inside a 3-character "trio". ERE has no
# lookahead, so "40 characters that contain a trio" means spelling out where the trio
# sits. One pattern per possible trio position is slow under BSD grep -w (macOS), so
# each pattern covers a 5-position window: fixed-length key characters before and
# after the window, and inside it the trio at each of the window's offsets. Every
# pattern still matches exactly 40 characters.
#
# The run must be bounded by a non-key character or the line edge. "=" may precede
# it (token=KEY) but not follow it, which keeps padded base64 out; "_" and "(" do not
# count as bounds, so identifiers such as Test_Name(...) never match. Each pattern
# also takes in the word around those bounds, because git-secrets greps with -w and
# would otherwise reject a match whose bound follows a word character (the "=" in
# token=KEY).
key_char='[A-Za-z0-9/+]'
key_pair='([A-Z][0-9]|[0-9][A-Z])'
key_trio="(${key_char}${key_pair}|${key_pair}${key_char}|[A-Z]${key_char}[0-9]|[0-9]${key_char}[A-Z])"
key_start='(^|[A-Za-z0-9_]*[^A-Za-z0-9/+_])'
key_end='([^A-Za-z0-9/+=_(][A-Za-z0-9_]*|$)'
# key_chars N: exactly N key characters (nothing for 0).
key_chars() {
    if [ "$1" -gt 0 ]; then
        printf '%s{%d}' "${key_char}" "$1"
    fi
}
# The trio starts at offset 0 to 37 of the 40 characters.
for window_start in 0 5 10 15 20 25 30 35; do
    window_end=$((window_start + 4))
    if [ "${window_end}" -gt 37 ]; then
        window_end=37
    fi
    span=$((window_end - window_start))
    trio_at=''
    for ((lead = 0; lead <= span; lead++)); do
        trio_at="${trio_at}|$(key_chars "${lead}")${key_trio}$(key_chars $((span - lead)))"
    done
    add_pattern "${key_start}$(key_chars "${window_start}")(${trio_at#|})$(key_chars $((37 - window_end)))${key_end}"
done

# Clones set up before #813 still carry the old negated-class "secret access key"
# pattern, which matched runs of 41 non-key characters (box-drawing and dash banner
# lines) and never a key. Re-running this script removes it from the repo's config,
# where the old version of this script registered it.
legacy_secret_key_pattern='[^A-Za-z0-9/+=]{40}[^A-Za-z0-9/+=]'
if git config --local --get-all secrets.patterns | grep -qxF -- "${legacy_secret_key_pattern}"; then
    git config --local --unset-all --fixed-value secrets.patterns "${legacy_secret_key_pattern}"
fi

add_pattern 'aws(.{0,20})?['\''"][0-9a-zA-Z/+]{40}['\''"]'       # AWS Credentials

# GCP patterns
add_pattern 'AIza[0-9A-Za-z_-]{35}'                              # GCP API Key

# Azure patterns
add_pattern 'DefaultEndpointsProtocol=https'                      # Azure Connection String

# Generic secrets (require quoted values to avoid matching variable declarations)
add_pattern 'password[[:space:]]*[=:][[:space:]]*['\''"][^'\''"]{8,}'             # Password with quoted value
add_pattern 'api[_-]?key[[:space:]]*[=:][[:space:]]*['\''"][^'\''"]{8,}'         # API key with quoted value
add_pattern 'secret[_-]?key[[:space:]]*[=:][[:space:]]*['\''"][^'\''"]{8,}'      # Secret key with quoted value
add_pattern 'BEGIN[[:space:]]((RSA|DSA|EC|OPENSSH|ENCRYPTED)[[:space:]])?PRIVATE[[:space:]]KEY-----'  # PEM private keys

# Database connection strings
add_pattern 'postgres://[^:]+:[^@]+@'                           # PostgreSQL
add_pattern 'mysql://[^:]+:[^@]+@'                              # MySQL
add_pattern 'mongodb(\+srv)?://[^:]+:[^@]+@'                    # MongoDB

# Allowed patterns live in .gitallowed (versioned, applied by every scan, including CI).
# They are matched against the whole "path:line:content" scanner output line, so an entry
# must describe the benign literal or anchor on the path; a bare keyword whitelists
# every line that contains it (#1972).

echo -e "${GREEN}✓ Secret patterns registered${NC}"

# Scan existing repository
echo ""
echo "========================================"
echo "Scanning Existing Repository"
echo "========================================"
echo ""

if git secrets --scan -r; then
    echo ""
    echo -e "${GREEN}✓ No secrets found in repository${NC}"
else
    echo ""
    echo -e "${RED}✗ Secrets detected in repository!${NC}"
    echo "Please remove them before committing."
    exit 1
fi

echo ""
echo "========================================"
echo "Setup Complete"
echo "========================================"
echo ""
echo "Git-secrets is now active and will prevent commits containing secrets."
echo ""
echo "Useful commands:"
echo "  git secrets --scan        - Scan staged files"
echo "  git secrets --scan-history - Scan entire history"
echo "  git secrets --list        - List registered patterns"
echo ""
