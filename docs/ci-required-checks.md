# CI Required Status Checks

This document records the canonical set of required status checks for the
CUDly repository and the admin commands needed to enforce them.

## Context

PR #232 (merged 2026-05-03) added `.github/workflows/frontend-build.yml`,
which runs `npm ci`, `npm run typecheck`, and `npm run build` for PRs
touching `frontend/**`. The workflow shipped, but the corresponding
`Build frontend` check was never added to the branch-protection
required-checks list. A PR that breaks the frontend build can currently be
merged without a block (issue #377).

The workflow targets the `main` branch only. The `feat/multicloud-web-frontend`
trigger was removed in the commit that merged that branch into `main`
(`4f7d429a8`), so `Build frontend` only ever runs for PRs targeting `main`.

> **Note on context naming**: GitHub branch-protection contexts are derived
> from the **job `name:`** field, not the workflow `name:` field. The workflow
> is named `Frontend build (PR)` but its single job is named `Build frontend`,
> so the context GitHub reports (and that must be entered here) is
> `Build frontend`.

### Why the workflow has no `paths:` filter

A required check must report a status on **every** PR. A workflow with a
`paths:` filter reports nothing at all on a PR that touches none of its
paths, so with `Build frontend` required, every backend-only,
Terraform-only or docs-only PR would sit at "Expected - Waiting for status
to be reported" and could never be merged, with no failing check to point
at.

`frontend-build.yml` therefore triggers on every PR to `main`, and the
`Build frontend` job's first step (`Detect frontend changes`) diffs the
PR's test-merge commit against its base. When nothing under `frontend/`
(or the workflow file itself) changed, the job skips its build steps and
passes in seconds; otherwise it runs the full build. If the changed files
cannot be listed, it runs the build rather than skipping it.

Do not "optimize" this back into a `paths:` filter, and do not add a second
job or workflow named `Build frontend` to fill the gap: on a PR that touches
both frontend and other files, both would report under the same name, and
the requirement must never be satisfiable by the stand-in's pass while the
real build is failing.

## Required checks -- canonical list

`main` should require:

- `Build frontend`
- Any other checks already present at the time an admin applies this runbook
  (retrieve the live list first; see Step 1 below).

## Admin steps

These commands require repo-admin permission on LeanerCloud/cloud-commitments-platform.

### Step 1 -- retrieve the current required checks for main

```bash
gh api repos/LeanerCloud/cloud-commitments-platform/branches/main/protection \
  --jq '.required_status_checks.contexts'
```

If this returns `"Branch not protected"` (HTTP 404), the protection rule
must be created from scratch; use the `gh api -X PUT` form in Step 3.

### Step 2 -- add the check to an already-protected branch

Replace `EXISTING_CHECK_1` etc. with the contexts returned by Step 1:

```bash
gh api -X PATCH \
  repos/LeanerCloud/cloud-commitments-platform/branches/main/protection/required_status_checks \
  -F 'contexts[]=EXISTING_CHECK_1' \
  -F 'contexts[]=Build frontend'
```

### Step 3 -- create protection from scratch (if Step 1 returned 404)

Adjust `required_pull_request_reviews` and `enforce_admins` to taste:

```bash
gh api -X PUT \
  repos/LeanerCloud/cloud-commitments-platform/branches/main/protection \
  --input - <<'EOF'
{
  "required_status_checks": {
    "strict": true,
    "contexts": ["Build frontend"]
  },
  "enforce_admins": false,
  "required_pull_request_reviews": null,
  "restrictions": null
}
EOF
```

### Step 4 -- verify

```bash
gh api repos/LeanerCloud/cloud-commitments-platform/branches/main/protection \
  --jq '.required_status_checks.contexts'
```

The output should include `"Build frontend"`.

### Step 5 -- smoke test

Open a draft PR with a deliberate TypeScript error in `frontend/` and confirm
that the `Build frontend` check turns red and the merge button is blocked.

Then confirm the other half: on any PR that touches nothing under
`frontend/` (a docs-only change will do), `Build frontend` must still
report, pass, and log "No frontend changes in this PR; skipping the
frontend build." A PR stuck at "Expected - Waiting for status to be
reported" means the trigger has been path-filtered again.

## Notes

- The `strict` flag in Step 3 requires the branch to be up to date before
  merging; adjust if that is too strict for the current workflow.
- The GitHub UI path for the same change is: Settings > Branches >
  Branch protection rules > Edit rule > "Require status checks to pass" >
  search for `Build frontend` > save.
