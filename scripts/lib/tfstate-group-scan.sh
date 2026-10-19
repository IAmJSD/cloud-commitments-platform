#!/usr/bin/env bash
# tfstate-group-scan.sh
#
# The scanner behind scripts/test-tfstate-concurrency-group.sh (#195): finds
# every workflow job that touches the Azure or GCP Terraform state, decides
# whether it writes that state, and checks its concurrency group against the
# state key. Sourced, not executed; it expects scripts/lib/code-scan-awk.sh to
# have been sourced first (for AWK_CODE_FUNCS and build_swept_scripts) and
# defines TF_WRITE_FUNCS, mutating_scripts and scan_state_groups.
#
# Kept apart from the suite so each file stays readable on its own: this one is
# the rule, the suite is the evidence that the rule sees what it claims to.

# tf_write_cmd(line) returns the first state-writing terraform command the line
# runs ("terraform apply", "terraform state rm", ...), or "" when it runs none.
#
# Quotes are deleted rather than their contents emptied (unlike invokes()): a
# call inside "$(terraform state rm X)" is still a call, and the cost of the
# fail-closed direction is only that `echo "terraform apply failed"` in a job
# makes it a writer. Global options (-chdir=...) are skipped. A subcommand that
# cannot be read, such as a line ending `terraform \`, counts as a write.
#
# shellcheck disable=SC2016  # awk source, not shell expansions
TF_WRITE_FUNCS='
  function tf_readonly(a, b) {
    if (a == "state") return (b == "list" || b == "show" || b == "pull")
    if (a == "workspace") return (b == "list" || b == "show")
    return (a ~ /^(init|output|plan|show|validate|fmt|version|providers|graph|get|test|console|metadata)$/)
  }
  function tf_write_cmd(line,   n, i, toks, a, b) {
    line = code_of(line)
    gsub(/"/, "", line)
    gsub(SQ, "", line)
    gsub(/[$][(]|[`(;&|]/, " ", line)
    n = split(line, toks, /[[:space:]]+/)
    for (i = 1; i <= n; i++) {
      if (toks[i] != "terraform") continue
      a = ""; b = ""
      while (++i <= n) if (toks[i] !~ /^-/) { a = toks[i]; break }
      if (a == "state" || a == "workspace")
        while (++i <= n) if (toks[i] !~ /^-/) { b = toks[i]; break }
      if (a == "" || a == "\\") return "terraform (unparsed)"
      if (!tf_readonly(a, b)) return ("terraform " a ((b == "") ? "" : (" " b)))
    }
    return ""
  }
'

# mutating_scripts DIR prints " a.sh lib/b.sh " -- every *.sh under DIR, as a
# path relative to DIR, that runs a state-writing terraform command. A workflow
# job that calls one of them is a writer even with no terraform line of its own.
mutating_scripts() {
  local dir="$1"
  build_swept_scripts "$dir"
  if [[ ${#SWEPT_SCRIPTS[@]} -eq 0 ]]; then
    echo " "
    return
  fi
  awk -v SQ="'" -v root="${dir}/" "${AWK_CODE_FUNCS}${TF_WRITE_FUNCS}"'
    BEGIN { printf " " }
    !(FILENAME in seen) && tf_write_cmd($0) != "" {
      seen[FILENAME] = 1
      printf "%s ", substr(FILENAME, length(root) + 1)
    }
  ' "${SWEPT_SCRIPTS[@]}"
}

# --- The scanner -------------------------------------------------------------
#
# scan_state_groups MODE WORKFLOW_DIR SCRIPTS_DIR
#
# Sites are top-level YAML jobs (a line at exactly two spaces of indent holding
# an identifier and nothing else), the same delimiter the AWS guard uses: the
# key, the terraform call and the concurrency group live in different steps or
# at job level, and a job is the smallest unit holding all three.
#
# MODE=violations prints one line per writer whose group is missing, in another
# family, or not derived from its key, plus a line of its own when the scanned
# set holds no writer at all. No output means the scanned set is clean.
#
# MODE=census prints `file|job|state|key-env|reads-or-writes|group` for every
# job that touches Azure or GCP state, whatever the verdict. It is what the
# positive assertions read, so "clean" cannot mean "recognized nothing".
#
# A site is reported from site_file rather than FILENAME: a site running to the
# end of its file is flushed by the next file's first line, by which point
# FILENAME has advanced and the report would name an innocent file.
scan_state_groups() {
  local mode="$1"
  local dir="$2"
  local scripts_dir="$3"
  local files=()
  local mutating

  shopt -s nullglob
  files=("${dir}"/*.yml "${dir}"/*.yaml)
  shopt -u nullglob

  if [[ ${#files[@]} -eq 0 ]]; then
    echo "no workflow files found under ${dir}"
    return
  fi

  mutating="$(mutating_scripts "$scripts_dir")"

  # Ternaries are parenthesized wherever they appear in an argument list or a
  # `return`, for BWK awk (macOS); `[$]` rather than `\$` for mawk (CI).
  awk -v SQ="'" -v mode="$mode" -v mutating="$mutating" \
    "${AWK_CODE_FUNCS}${TF_WRITE_FUNCS}"'
    function basename(p) { sub(/^.*\//, "", p); return p }
    function label() { return ((site == "") ? "workflow level" : ("job \"" site "\"")) }
    function indent_of(s) { match(s, /^ */); return RLENGTH }
    function unquote(v) {
      if (v ~ /^".*"$/ || v ~ ("^" SQ ".*" SQ "$")) v = substr(v, 2, length(v) - 2)
      return v
    }

    # Whitespace inside ${{ }} is removed up front, so an expression is one
    # word everywhere below and two spellings of it compare equal.
    function squeeze_exprs(s,   out, m) {
      out = ""
      while (match(s, /[$][{][{][^}]*[}][}]/)) {
        m = substr(s, RSTART, RLENGTH)
        gsub(/[[:space:]]/, "", m)
        out = out substr(s, 1, RSTART - 1) m
        s = substr(s, RSTART + RLENGTH)
      }
      return out s
    }

    # lit:<value>, expr:<expression>, or "?" when it is neither.
    function classify(v,   inner) {
      v = unquote(v)
      if (v ~ /^[$][{][{].*[}][}]$/) {
        inner = substr(v, 4, length(v) - 5)
        if (inner ~ /[{}]/ || inner == "") return "?"
        return ("expr:" inner)
      }
      if (v ~ /^[A-Za-z0-9_.-]+$/) return ("lit:" v)
      return "?"
    }

    # A shell word traced through the step env, then the job env.
    function resolve(v, depth,   name) {
      v = unquote(v)
      if (depth > 4) return "?"
      if (v ~ /^[$][A-Za-z_][A-Za-z0-9_]*$/ || v ~ /^[$][{][A-Za-z_][A-Za-z0-9_]*[}]$/) {
        name = v
        gsub(/[${}]/, "", name)
        if (name in stepenv) return resolve(stepenv[name], depth + 1)
        if (name in jobenv) return resolve(jobenv[name], depth + 1)
        return "?"
      }
      return classify(v)
    }

    # The word that fills a key whose environment is a printf `%s`: the
    # argument at the position of that `%s` in the format.
    function printf_arg(code,   rest, q, k, fmt, idx, n, args, i) {
      if (!match(code, /printf[[:space:]]+/)) return "?"
      rest = substr(code, RSTART + RLENGTH)
      q = substr(rest, 1, 1)
      if (q != SQ && q != "\"") return "?"
      rest = substr(rest, 2)
      k = index(rest, q)
      if (k == 0) return "?"
      fmt = substr(rest, 1, k - 1)
      rest = substr(rest, k + 1)
      k = index(fmt, "github-%s")
      if (k == 0) return "?"
      fmt = substr(fmt, 1, k - 1)
      idx = gsub(/%s/, "", fmt) + 1
      n = split(rest, args, /[[:space:]]+/)
      k = 0
      for (i = 1; i <= n; i++) {
        if (args[i] == "") continue
        if (args[i] ~ /^[>|;&]/) break
        if (++k == idx) return args[i]
      }
      return "?"
    }

    function note_key(f, tok, code) {
      note_fam(f)
      pending[++npending] = ((tok == "%s") ? printf_arg(code) : ((tok ~ /%/) ? "?" : tok))
    }
    function note_fam(f) { fam = ((fam == "" || fam == f) ? f : "MIXED") }
    function note_kenv(v) { kenv = ((kenv == "" || kenv == v) ? v : "MIXED") }

    # Pending key words are resolved when their step closes, so a step that
    # declares `env:` after `run:` still resolves.
    function flush_step(   i) {
      for (i = 1; i <= npending; i++) note_kenv(resolve(pending[i], 0))
      npending = 0
      split("", stepenv)
    }

    function note_group(v,   s) {
      grp = unquote(v)
      gfam = "other"; gsuf = ""
      if (grp ~ /^azure-tfstate-/) gfam = "azure"
      else if (grp ~ /^gcp-tfstate-/) gfam = "gcp"
      if (gfam != "other") {
        s = grp
        sub(/^[a-z]+-tfstate-/, "", s)
        gsuf = classify(s)
      }
    }

    function gcp_prefix(code,   r, c) {
      if (!match(code, /prefix[[:space:]]*=[[:space:]]*/)) return ""
      r = substr(code, RSTART + RLENGTH)
      sub(/^[\\]?"?/, "", r)
      if (substr(r, 1, 7) != "github-") return ""
      r = substr(r, 8)
      for (c = 1; c <= length(r); c++)
        if (index("\"\\/ \t" SQ, substr(r, c, 1))) break
      return ("=" substr(r, 1, c - 1))
    }

    function names_root(code, r) {
      return (code ~ ("terraform/environments/" r "/?$") || code ~ ("terraform/environments/" r "/?[^A-Za-z0-9_./-]"))
    }

    function finish() {
      flush_step()
      if (fam != "") {
        total++
        if (wcmd != "") writers++
        if (mode == "census") {
          printf "%s|%s|%s|%s|%s|%s\n", basename(site_file), site, fam, ((kenv == "") ? "NONE" : kenv), ((wcmd == "") ? "reads" : "writes"), ((grp == "") ? "NONE" : grp)
        } else if (fam == "MIXED") {
          printf "%s: %s: touches both the Azure and the GCP Terraform state; one job can carry only one concurrency group\n", basename(site_file), label()
        } else if (wcmd != "") {
          report()
        }
      }
      fam = ""; kenv = ""; wcmd = ""; grp = ""; gfam = ""; gsuf = ""
      in_env = 0; in_conc = 0; npending = 0
      split("", jobenv); split("", stepenv)
    }

    function report(   where) {
      where = basename(site_file) ": " label() ": runs " wcmd " against the " fam " Terraform state"
      if (grp == "")
        printf "%s with no concurrency group; expected %s-tfstate-<environment>\n", where, fam
      else if (gfam != fam)
        printf "%s, but its concurrency group %s is not in the %s-tfstate-* family\n", where, grp, fam
      else if (kenv == "")
        printf "%s but builds no %s state key in the job, so its group cannot be tied to the state it writes\n", where, fam
      else if (kenv == "MIXED")
        printf "%s and builds keys for more than one environment\n", where
      else if (kenv == "?")
        printf "%s, and the environment in its state key cannot be traced to a literal or a ${{ }} expression\n", where
      else if (gsuf != kenv)
        printf "%s, but its concurrency group %s is not derived from the same value as the state key (%s)\n", where, grp, kenv
    }

    BEGIN {
      AZKEY = "github-[^\"" SQ "/[:space:]]*[.]terraform[.]tfstate"
    }

    FNR == 1 { if (NR > 1) finish(); site_file = FILENAME; site = "" }

    {
      code = squeeze_exprs(code_of($0))
      if (code ~ /^[[:space:]]*$/) next
      ind = indent_of(code)

      # The job boundary is tested on the comment-stripped line so a header
      # carrying a trailing comment still ends the previous job.
      if (code ~ /^  [A-Za-z0-9_.-]+:[[:space:]]*$/) {
        finish()
        site = code
        sub(/^[[:space:]]*/, "", site)
        sub(/:[[:space:]]*$/, "", site)
        next
      }

      if (in_env && ind <= env_ind) in_env = 0
      if (in_conc && ind <= 4) in_conc = 0
      if (code ~ /^      - /) flush_step()

      if (in_env && code ~ /^[[:space:]]*[A-Za-z_][A-Za-z0-9_]*:/) {
        k = code; sub(/^[[:space:]]*/, "", k); sub(/:.*$/, "", k)
        v = code; sub(/^[^:]*:[[:space:]]*/, "", v); sub(/[[:space:]]*$/, "", v)
        if (env_ind == 4) jobenv[k] = v
        else stepenv[k] = v
      }
      if (code ~ /^[[:space:]]*(- )?env:[[:space:]]*$/) { in_env = 1; env_ind = ind }

      if (code ~ /^    concurrency:[[:space:]]*$/) in_conc = 1
      else if (code ~ /^    concurrency:[[:space:]]*[^[:space:]]/) {
        v = code; sub(/^    concurrency:[[:space:]]*/, "", v); sub(/[[:space:]]*$/, "", v)
        note_group(v)
      }
      if (in_conc && code ~ /^[[:space:]]+group:/) {
        v = code; sub(/^[[:space:]]*group:[[:space:]]*/, "", v); sub(/[[:space:]]*$/, "", v)
        note_group(v)
      }

      # A shell assignment in a run block, scoped to its step like env: is.
      if (code ~ /^[[:space:]]*(export[[:space:]]+)?[A-Za-z_][A-Za-z0-9_]*=/) {
        k = code; sub(/^[[:space:]]*(export[[:space:]]+)?/, "", k)
        v = k; sub(/=.*$/, "", k); sub(/^[^=]*=/, "", v); sub(/[[:space:]].*$/, "", v)
        stepenv[k] = v
      }

      if (match(code, AZKEY)) note_key("azure", substr(code, RSTART + 7, RLENGTH - 25), code)
      p = gcp_prefix(code)
      if (p != "") note_key("gcp", substr(p, 2), code)
      if (names_root(code, "azure")) note_fam("azure")
      if (names_root(code, "gcp")) note_fam("gcp")

      w = tf_write_cmd($0)
      if (w != "" && wcmd == "") wcmd = w
      rest = code
      while (wcmd == "" && match(rest, /scripts\/[A-Za-z0-9_.\/-]*[.]sh/)) {
        s = substr(rest, RSTART + 8, RLENGTH - 8)
        if (index(mutating, " " s " ")) wcmd = "scripts/" s
        rest = substr(rest, RSTART + RLENGTH)
      }
    }

    END {
      finish()
      if (writers == 0 && mode != "census")
        print "no job writing the Azure or GCP Terraform state found at all"
    }
  ' "${files[@]}"
}
