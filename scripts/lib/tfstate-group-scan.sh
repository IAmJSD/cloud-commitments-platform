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
  # init reads unless it migrates the state to the backend it configures:
  # -migrate-state copies it, and -force-copy answers that copy with yes. Flags
  # continued onto the next line cannot be read, so init then counts as a write.
  function tf_init_write(toks, n, i,   j, f) {
    for (j = i + 1; j <= n; j++) {
      if (toks[j] == "\\") return "terraform init (unparsed)"
      if (toks[j] ~ /^-(migrate-state|force-copy)(=true)?$/) {
        f = toks[j]
        sub(/=.*$/, "", f)
        return ("terraform init " f)
      }
      if (toks[j] !~ /^-/) break
    }
    return ""
  }
  function tf_write_cmd(line,   n, i, toks, a, b, f) {
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
      if (a == "init") {
        f = tf_init_write(toks, n, i)
        if (f != "") return f
        continue
      }
      if (!tf_readonly(a, b)) return ("terraform " a ((b == "") ? "" : (" " b)))
    }
    return ""
  }
'

# mutating_scripts DIR prints " a.sh lib/b.sh " -- every *.sh under DIR, as a
# path relative to DIR, that runs a state-writing terraform command, directly
# or through another script under DIR at any depth. A workflow job that calls
# one of them is a writer even with no terraform line of its own.
#
# A script calling another is matched by the callee's file name at a word
# boundary (`"${SCRIPT_DIR}/lib/b.sh"`, `./b.sh`, `bash b.sh`), not by
# resolving the path, so two scripts sharing a file name both count. That errs
# towards calling a job a writer, the fail-closed direction. Repeated until no
# script is added, which takes one pass per level of nesting.
mutating_scripts() {
  local dir="$1"
  local found prev
  build_swept_scripts "$dir"
  if [[ ${#SWEPT_SCRIPTS[@]} -eq 0 ]]; then
    echo " "
    return
  fi
  found="$(awk -v SQ="'" -v root="${dir}/" "${AWK_CODE_FUNCS}${TF_WRITE_FUNCS}"'
    BEGIN { printf " " }
    !(FILENAME in seen) && tf_write_cmd($0) != "" {
      seen[FILENAME] = 1
      printf "%s ", substr(FILENAME, length(root) + 1)
    }
  ' "${SWEPT_SCRIPTS[@]}")"
  prev=""
  while [[ "$found" != "$prev" ]]; do
    prev="$found"
    found="$(awk -v SQ="'" -v root="${dir}/" -v known="$found" "${AWK_CODE_FUNCS}"'
      # Positions are kept absolute in the padded line, so the character before
      # a match always exists (substr from 0 differs between awks).
      function calls(line,   name, off, k, pos, before, after) {
        line = " " code_of(line) " "
        for (name in callee) {
          off = 1
          while ((k = index(substr(line, off), name)) > 0) {
            pos = off + k - 1
            before = substr(line, pos - 1, 1)
            after = substr(line, pos + length(name), 1)
            if (before !~ /[A-Za-z0-9_.-]/ && after !~ /[A-Za-z0-9_.-]/) return 1
            off = pos + 1
          }
        }
        return 0
      }
      BEGIN {
        n = split(known, rel, " ")
        for (i = 1; i <= n; i++) {
          is_known[rel[i]] = 1
          b = rel[i]
          sub(/^.*\//, "", b)
          callee[b] = 1
        }
        printf "%s", known
      }
      {
        f = substr(FILENAME, length(root) + 1)
        if (!(f in is_known) && calls($0)) {
          is_known[f] = 1
          printf "%s ", f
        }
      }
    ' "${SWEPT_SCRIPTS[@]}")"
  done
  echo "$found"
}

# --- The scanner -------------------------------------------------------------
#
# scan_state_groups MODE WORKFLOW_DIR SCRIPTS_DIR
#
# Sites are top-level YAML jobs (a line at exactly two spaces of indent holding
# a bare or quoted identifier and nothing else), as in the AWS guard: the
# key, the terraform call and the concurrency group live in different steps or
# at job level, and a job is the smallest unit holding all three.
#
# MODE=violations prints one line per writer whose group is missing, in another
# family, or not derived from its key, plus a line of its own when the scanned
# set holds no writer at all. No output means the scanned set is clean.
#
# A writer's group must also not cancel a run in progress: `cancel-in-progress`
# on its job-level group, and on a workflow-level group if the file has one,
# must be absent or `false`. A newer run queued on the group would otherwise
# cancel a terraform write mid-run and leave a held lease or half-applied
# state. An expression counts as not false, since it can evaluate to true.
#
# MODE=census prints `file|job|state|key-env|reads-or-writes|group|cancel` for
# every job that touches Azure or GCP state, whatever the verdict (cancel is the
# job-level cancel-in-progress value, or UNSET). It is what the
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
    # argument at the position of that conversion in the format.
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
      # Every conversion before the key takes an argument, not only %s; %% takes
      # none, and a * width or precision takes one more, so it is not traced.
      fmt = substr(fmt, 1, k - 1)
      gsub(/%%/, "", fmt)
      if (fmt ~ /%[^A-Za-z]*[*]/) return "?"
      idx = gsub(/%[-+ #0-9.]*[A-Za-z]/, "", fmt) + 1
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

    function cancel_ok(v) { v = unquote(v); return (v == "" || v == "false") }

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
          printf "%s|%s|%s|%s|%s|%s|%s\n", basename(site_file), site, fam, ((kenv == "") ? "NONE" : kenv), ((wcmd == "") ? "reads" : "writes"), ((grp == "") ? "NONE" : grp), ((jcancel == "") ? "UNSET" : jcancel)
        } else if (fam == "MIXED") {
          printf "%s: %s: touches both the Azure and the GCP Terraform state; one job can carry only one concurrency group\n", basename(site_file), label()
        } else if (wcmd != "") {
          report()
        }
      }
      fam = ""; kenv = ""; wcmd = ""; grp = ""; gfam = ""; gsuf = ""; jcancel = ""
      in_env = 0; in_conc = 0; npending = 0; want_step = 0; step_ind = -1
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
      if (grp != "" && !cancel_ok(jcancel))
        printf "%s, but its concurrency group sets cancel-in-progress: %s; a newer run queued on the group would cancel the write mid-run (expected false or absent)\n", where, jcancel
      if (!cancel_ok(wcancel))
        printf "%s, but the workflow-level concurrency sets cancel-in-progress: %s; a newer run of the workflow would cancel the write mid-run (expected false or absent)\n", where, wcancel
    }

    BEGIN {
      step_ind = -1
      AZKEY = "github-[^\"" SQ "/[:space:]]*[.]terraform[.]tfstate"
      # A job id, bare or quoted: a quoted one is still a job, and missing it
      # would merge its lines into the job before it, group included.
      JOBRE = "^  ([A-Za-z0-9_.-]+|\"[A-Za-z0-9_.-]+\"|" SQ "[A-Za-z0-9_.-]+" SQ "):[[:space:]]*$"
    }

    FNR == 1 { if (NR > 1) finish(); site_file = FILENAME; site = ""; wcancel = ""; in_wconc = 0 }

    {
      code = squeeze_exprs(code_of($0))
      if (code ~ /^[[:space:]]*$/) next
      ind = indent_of(code)

      # The job boundary is tested on the comment-stripped line so a header
      # carrying a trailing comment still ends the previous job.
      if (code ~ JOBRE) {
        finish()
        site = code
        sub(/^[[:space:]]*/, "", site)
        sub(/:[[:space:]]*$/, "", site)
        site = unquote(site)
        next
      }

      if (in_env && ind <= env_ind) in_env = 0
      if (in_conc && ind <= 4) in_conc = 0
      if (in_wconc && ind == 0) in_wconc = 0
      # A step starts at a `- ` at the indent of the first item under `steps:`,
      # which is 6 here but may be 4 in another YAML style. Missing a boundary
      # would let the env: of one step reach the key of the next.
      if (code ~ /^    steps:[[:space:]]*$/) want_step = 1
      else if (want_step && code ~ /^[[:space:]]*- /) { step_ind = ind; want_step = 0 }
      if (ind == step_ind && code ~ /^[[:space:]]*- /) flush_step()

      if (in_env && code ~ /^[[:space:]]*[A-Za-z_][A-Za-z0-9_]*:/) {
        k = code; sub(/^[[:space:]]*/, "", k); sub(/:.*$/, "", k)
        v = code; sub(/^[^:]*:[[:space:]]*/, "", v); sub(/[[:space:]]*$/, "", v)
        if (env_job) jobenv[k] = v
        else stepenv[k] = v
      }
      # env_ind is the indent of the env: key itself, past any `- `, so the
      # step key after it (run:, at the same indent) ends the block.
      if (code ~ /^[[:space:]]*(- )?env:[[:space:]]*$/) {
        in_env = 1
        env_job = (code ~ /^    env:/)
        env_ind = ind + ((code ~ /^[[:space:]]*- /) ? 2 : 0)
      }

      if (code ~ /^    concurrency:[[:space:]]*$/) in_conc = 1
      else if (code ~ /^    concurrency:[[:space:]]*[^[:space:]]/) {
        v = code; sub(/^    concurrency:[[:space:]]*/, "", v); sub(/[[:space:]]*$/, "", v)
        note_group(v)
      }
      if (in_conc && code ~ /^[[:space:]]+group:/) {
        v = code; sub(/^[[:space:]]*group:[[:space:]]*/, "", v); sub(/[[:space:]]*$/, "", v)
        note_group(v)
      }
      # cancel-in-progress, for the job-level group and the workflow-level one.
      if (code ~ /^concurrency:[[:space:]]*$/) in_wconc = 1
      if ((in_conc || in_wconc) && code ~ /^[[:space:]]+cancel-in-progress:/) {
        v = code; sub(/^[[:space:]]*cancel-in-progress:[[:space:]]*/, "", v); sub(/[[:space:]]*$/, "", v)
        if (in_conc) jcancel = v
        else wcancel = v
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
