#!/usr/bin/env python3
"""Each rollback job's Terraform apply must pass what its deploy workflow passes.

A rollback apply that omits a deploy TF_VAR_ or -var resets that variable on the
live stack (issue #1744), and one with a different source or an extra variable
changes the stack just as silently. For each (rollback job, deploy workflow)
pair this compares, in both directions and by value, every TF_VAR_* env entry,
-var flag and shell-exported TF_VAR_ of the one step that applies.

The deploy workflows plan in one step (`terraform plan -var ...`) and apply the
saved plan in the next (`terraform apply tfplan`), so the deploy side is the
plan step; a rollback applies directly, so its side is the apply step.

Usage: check-rollback-tf-vars.py [workflows dir]
"""
import re
import sys
from pathlib import Path

import yaml

PAIRS = [
    ("rollback-aws-lambda", "deploy-aws-lambda.yml"),
    ("rollback-aws-fargate", "deploy-aws-fargate.yml"),
    ("rollback-gcp", "deploy-gcp.yml"),
    ("rollback-azure", "deploy-azure.yml"),
]

MISSING_DEPLOY = "extra in rollback"
VALUE = "value differs"
# Keys allowed to differ, with the one difference allowed and why.
EXCEPTIONS = {
    # The rollback pins a previous image; a deploy builds its image elsewhere.
    "-var image_uri": MISSING_DEPLOY,
    # Both read the live function URL, deploy through a step output and the
    # rollback through a shell export of the same script.
    "TF_VAR_lambda_deployed_function_url": VALUE,
}

# Deploys name the environment TARGET_ENVIRONMENT (a prepare-job output) and
# rollbacks name it ENVIRONMENT (inputs.environment); both are the environment
# being deployed. Their definitions legitimately differ, so -var-file text is
# aliased to one name and that name's definitions are not compared.
ENV_ALIASES = {"TARGET_ENVIRONMENT": "ENVIRONMENT"}
ALIASED_NAMES = set(ENV_ALIASES.values())

VAR_FILE = re.compile(r"""-var-file(?:=|\s+)(?:"([^"]*)"|'([^']*)'|(\S+))""")
VAR_FLAG = re.compile(
    r"""-var(?:=|\s+)(?:"([^"=]+)=([^"]*)"|'([^'=]+)=([^']*)'|([^\s"'=]+)=(\S*))"""
)
EXPORT = re.compile(r"^\s*(?:export\s+)?(TF_VAR_\w+)=")
ENV_REF = re.compile(r"\$\{?([A-Za-z_]\w*)\}?")


class CheckError(Exception):
    pass


def code_lines(run):
    return [l for l in str(run or "").splitlines() if not l.lstrip().startswith("#")]


def load(path):
    if not path.is_file():
        raise CheckError(f"missing workflow file {path.name}")
    doc = yaml.safe_load(path.read_text())
    if not isinstance(doc, dict) or not isinstance(doc.get("jobs"), dict):
        raise CheckError(f"{path.name} has no jobs")
    return doc


def find_step(doc, jobs, needle, where):
    hits = []
    for jname in jobs:
        for step in doc["jobs"][jname].get("steps") or []:
            if any(needle in l for l in code_lines(step.get("run"))):
                hits.append((jname, step))
    if len(hits) != 1:
        raise CheckError(f"{where}: want exactly one step running '{needle}', found {len(hits)}")
    return hits[0]


def collect(doc, jname, step):
    """Return ({key: value-or-None}, {env name: definition}) for one step."""
    pairs, files = {}, []
    for line in code_lines(step.get("run")):
        for m in VAR_FLAG.finditer(line):
            g = [x for x in m.groups() if x is not None]
            pairs[f"-var {g[0]}"] = g[1]
        for m in VAR_FILE.finditer(line):
            text = next(x for x in m.groups() if x is not None)
            for old, new in ENV_ALIASES.items():
                text = text.replace("${%s}" % old, "${%s}" % new).replace("$" + old, "$" + new)
            files.append(text)
        m = EXPORT.match(line)
        if m:
            pairs.setdefault(m.group(1), "<shell export>")
    if files:
        pairs["-var-file"] = ",".join(sorted(files))
    # Later scopes override earlier ones, as in GitHub Actions.
    defs = {}
    for scope in (doc.get("env"), doc["jobs"][jname].get("env"), step.get("env")):
        for k, v in (scope or {}).items():
            defs[str(k)] = str(v)
    for k, v in defs.items():
        if k.startswith("TF_VAR_"):
            pairs[k] = v
    return pairs, defs


def compare(job, deploy_file, r, d):
    (rp, rdefs), (dp, ddefs) = r, d
    errs = []
    for key in sorted(set(rp) | set(dp)):
        if key not in dp:
            kind = MISSING_DEPLOY
        elif key not in rp:
            kind = "missing in rollback"
        elif rp[key] != dp[key]:
            kind = VALUE
        else:
            continue
        if EXCEPTIONS.get(key) != kind:
            errs.append(f"{job} vs {deploy_file}: {key}: {kind}")
    for key, rval in sorted(rp.items()):
        if key not in dp or key in EXCEPTIONS:
            continue
        for ref in sorted(set(ENV_REF.findall(rval))):
            if key == "-var-file" and ref in ALIASED_NAMES:
                continue
            if rdefs.get(ref) != ddefs.get(ref):
                errs.append(f"{job} vs {deploy_file}: env {ref} (used by {key}): value differs")
    return errs


def check_pair(wdir, rollback_doc, job, deploy_file):
    if job not in rollback_doc["jobs"]:
        raise CheckError(f"rollback.yml has no job {job}")
    deploy_doc = load(wdir / deploy_file)
    _, rstep = find_step(rollback_doc, [job], "terraform apply", f"rollback.yml {job}")
    find_step(deploy_doc, list(deploy_doc["jobs"]), "terraform apply", deploy_file)
    djob, dstep = find_step(deploy_doc, list(deploy_doc["jobs"]), "terraform plan", deploy_file)
    r = collect(rollback_doc, job, rstep)
    d = collect(deploy_doc, djob, dstep)
    for side, pairs in (("rollback", r[0]), ("deploy", d[0])):
        if "-var-file" not in pairs:
            raise CheckError(f"{job} vs {deploy_file}: {side} passes no -var-file")
    if "-var image_uri" not in r[0]:
        raise CheckError(f"{job}: rollback does not pass -var image_uri")
    if not r[0] or not d[0]:
        raise CheckError(f"{job} vs {deploy_file}: extracted no variables from one side")
    return compare(job, deploy_file, r, d)


def main(argv):
    wdir = Path(argv[1]) if len(argv) > 1 else Path(__file__).resolve().parent.parent / ".github" / "workflows"
    try:
        rollback_doc = load(wdir / "rollback.yml")
        errs = []
        for job, deploy_file in PAIRS:
            errs += check_pair(wdir, rollback_doc, job, deploy_file)
    except CheckError as e:
        print(f"FAIL: {e}", file=sys.stderr)
        return 1
    for e in errs:
        print(f"FAIL: {e}", file=sys.stderr)
    if errs:
        return 1
    print("rollback applies match the deploy applies")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
