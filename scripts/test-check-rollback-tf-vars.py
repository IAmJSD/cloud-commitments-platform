#!/usr/bin/env python3
"""Self-test for check-rollback-tf-vars.py: each drift probe is applied to a copy
of the real workflows and must fail the check; the unmodified copy must pass."""
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parent.parent
CHECK = ROOT / "scripts" / "check-rollback-tf-vars.py"
SRC = ROOT / ".github" / "workflows"
NAMES = ["rollback.yml", "deploy-aws-lambda.yml", "deploy-aws-fargate.yml", "deploy-gcp.yml", "deploy-azure.yml"]


def apply_step(rollback, job):
    return next(s for s in rollback["jobs"][job]["steps"] if "terraform apply" in str(s.get("run")))


def probe_source(rb):
    apply_step(rb, "rollback-aws-lambda")["env"]["TF_VAR_from_email"] = "${{ vars.FROM_EMAIL }}"


def probe_extra(rb):
    apply_step(rb, "rollback-aws-fargate")["env"]["TF_VAR_extra"] = "x"


def probe_comment(rb):
    s = apply_step(rb, "rollback-aws-lambda")
    del s["env"]["TF_VAR_from_email"]
    s["run"] = "# TF_VAR_from_email: ${{ secrets.FROM_EMAIL }}\n" + s["run"]


def probe_moved(rb):
    steps = rb["jobs"]["rollback-aws-lambda"]["steps"]
    i = steps.index(apply_step(rb, "rollback-aws-lambda"))
    steps[i - 1].setdefault("env", {})["TF_VAR_dashboard_url"] = steps[i]["env"].pop("TF_VAR_dashboard_url")


def probe_no_project_id(rb):
    s = apply_step(rb, "rollback-gcp")
    s["run"] = "\n".join(l for l in s["run"].splitlines() if "project_id" not in l)


def probe_location_def(rb):
    apply_step(rb, "rollback-azure")["env"]["AZURE_LOCATION"] = "eastus"


def probe_no_location(rb):
    s = apply_step(rb, "rollback-azure")
    s["run"] = "\n".join(l for l in s["run"].splitlines() if "location=" not in l)


def probe_no_apply(rb):
    apply_step(rb, "rollback-gcp")["run"] = "terraform destroy"


def probe_no_job(rb):
    rb["jobs"]["rollback-gcp-renamed"] = rb["jobs"].pop("rollback-gcp")


def strip_lines(job, needle):
    def probe(rb):
        s = apply_step(rb, job)
        s["run"] = "\n".join(l for l in s["run"].splitlines() if needle not in l)
    return probe


def probe_varfile_hardcoded(rb):
    s = apply_step(rb, "rollback-aws-lambda")
    s["run"] = s["run"].replace('github-${ENVIRONMENT}.tfvars', "github-prod.tfvars")


def probe_varfile_template(rb):
    s = apply_step(rb, "rollback-gcp")
    s["run"] = s["run"].replace('github-${ENVIRONMENT}.tfvars', "${ENVIRONMENT}.tfvars")


def probe_job_level(rb):
    rb["jobs"]["rollback-aws-lambda"].setdefault("env", {})["TF_VAR_extra"] = "x"


def probe_workflow_level(rb):
    rb.setdefault("env", {})["TF_VAR_extra"] = "x"


def probe_step_to_job_same(rb):
    s = apply_step(rb, "rollback-aws-lambda")
    v = s["env"].pop("TF_VAR_dashboard_url")
    rb["jobs"]["rollback-aws-lambda"].setdefault("env", {})["TF_VAR_dashboard_url"] = v


def probe_step_to_job_diff(rb):
    probe_step_to_job_same(rb)
    rb["jobs"]["rollback-aws-lambda"]["env"]["TF_VAR_dashboard_url"] = "other"


PASSES = {"TF_VAR_ moved step -> job, same value": probe_step_to_job_same}

MUTATIONS = {
    "TF_VAR_ at job level": probe_job_level,
    "TF_VAR_ at workflow level": probe_workflow_level,
    "TF_VAR_ moved step -> job, new value": probe_step_to_job_diff,
    "-var-file dropped": strip_lines("rollback-aws-fargate", "-var-file"),
    "-var-file hardcoded to prod": probe_varfile_hardcoded,
    "-var-file template changed": probe_varfile_template,
    "image_uri removed": strip_lines("rollback-azure", "image_uri"),
    "TF_VAR_ source secrets. -> vars.": probe_source,
    "rollback-only TF_VAR_extra": probe_extra,
    "deploy var only in a comment": probe_comment,
    "var moved to another step": probe_moved,
    "-var project_id deleted (gcp)": probe_no_project_id,
    "AZURE_LOCATION defined differently": probe_location_def,
    "-var location deleted (azure)": probe_no_location,
    "missing apply step": probe_no_apply,
    "missing rollback job": probe_no_job,
}


def run(mutate=None, drop=None):
    with tempfile.TemporaryDirectory() as d:
        for n in NAMES:
            shutil.copy(SRC / n, d)
        if drop:
            (Path(d) / drop).unlink()
        if mutate:
            p = Path(d) / "rollback.yml"
            doc = yaml.safe_load(p.read_text())
            mutate(doc)
            p.write_text(yaml.safe_dump(doc))
        r = subprocess.run([sys.executable, str(CHECK), d], capture_output=True, text=True)
        # A crash also exits 1; only a reported FAIL counts as the check failing.
        if r.returncode == 1 and not r.stderr.startswith("FAIL:"):
            raise SystemExit(f"check crashed instead of reporting: {r.stderr}")
        if VERBOSE and r.returncode:
            print(f"  [{mutate.__name__ if mutate else drop}] {r.stderr.strip()}")
        return r.returncode


VERBOSE = "-v" in sys.argv
failed = []
if run() != 0:
    failed.append("real tree must pass")
for label, fn in MUTATIONS.items():
    if run(fn) != 1:
        failed.append(f"must fail: {label}")
for label, fn in PASSES.items():
    if run(fn) != 0:
        failed.append(f"must pass: {label}")
for n in NAMES[1:]:
    if run(drop=n) != 1:
        failed.append(f"must fail: missing {n}")
for f in failed:
    print(f"FAIL: {f}", file=sys.stderr)
if failed:
    sys.exit(1)
print("check-rollback-tf-vars self-test passed")
