# CLAUDE.md

## Behavioral Rules (Always Enforced)

- Do what has been asked; nothing more, nothing less
- NEVER create files unless they're absolutely necessary for achieving your goal
- ALWAYS prefer editing an existing file to creating a new one
- NEVER proactively create documentation files (*.md) or README files unless explicitly requested
- NEVER save working files, text/mds, or tests to the root folder
- ALWAYS read a file before editing it
- NEVER commit secrets, credentials, or .env files

## Planning (ALWAYS follow this process)

- **EVERY TIME** you create or update a plan, you MUST enter a review loop: thoroughly review the plan and fix any issues found. Repeat until 3 consecutive review passes find no issues. Do NOT skip this step — it is mandatory for all plans, no exceptions.
- In each loop iteration, print a summary of found issues before and after fixing them.

## File Organization

- NEVER save working files to the root folder; use the directories below
- `cmd/`: CLI and server entry points (main packages)
- `internal/`: backend application code (API, auth, purchase, scheduler, ...)
- `frontend/`: TypeScript web frontend (webpack + jest)
- `terraform/`, `cloudformation/`, `arm/`, `iac/`: infrastructure as code
- `docs/`: documentation and markdown files
- `scripts/`: utility scripts
- `tests/`: end-to-end tests (Go unit tests live next to the code they test)

## Project Architecture

- Follow Domain-Driven Design with bounded contexts
- Keep files under 500 lines
- Use typed interfaces for all public APIs
- Prefer TDD London School (mock-first) for new code
- Use event sourcing for state changes
- Ensure input validation at system boundaries

## Go Module Notes

- This project does NOT use a vendor directory. Do not use `go mod vendor`.
- The shared libraries and cloud providers come from `github.com/LeanerCloud/cloud-commitments-go`, pinned to fixed versions in `go.mod`. There are no `replace` directives and no sibling checkout is needed.
- `tests/e2e` is a separate standard-library-only module, excluded from the root module on purpose.
- Build and test with `make build` and `make test-unit` from the repository root.

## Build & Test

The root of the repo is a Go project; the npm scripts live in `frontend/`.

```bash
# Build (backend, from the repo root)
go build ./...        # or: make build

# Test (backend)
go test ./...         # or: make test-unit

# Lint (backend)
make lint             # golangci-lint; also: make vet, make fmt

# Frontend (run from frontend/)
cd frontend && npm ci
npm run build         # webpack production build
npm test              # jest --coverage
npm run lint          # eslint src/**/*.ts
```

- ALWAYS run tests after making code changes
- ALWAYS verify build succeeds before committing

## Known Issues

The `known_issues/` directory tracks deferred tech debt and surfaced bugs.
When a referenced GitHub issue is closed, move the corresponding doc to
`known_issues/resolved/` (do not delete it) so the rationale is preserved.
Do this in the same PR that closes the issue. A full sweep of the directory
should happen at the start of each sprint. Full convention and entry format
are in `CONTRIBUTING.md` under "Known Issues Sweep".

## Issue assignment — claim before you work (MANDATORY)

Several people (and their agents) work through this repo's issue queue in
parallel. GitHub issue assignment is the lock that keeps two of them from
fixing the same thing. The `~/.claude/agent-comms/` bus below only
coordinates agents on one machine; assignment is what coordinates people.

1. **Pick work in priority order.** Take the highest-priority open issue
   that nobody owns, oldest first within a priority. Skip anything
   labelled `status/blocked`, `status/needs-info` or `status/wontdo`:

   ```bash
   for p in p0 p1 p2 p3; do
     echo "== priority/$p"
     gh issue list --repo LeanerCloud/cloud-commitments-platform --label "priority/$p" --limit 500 \
       --search "no:assignee -label:status/blocked -label:status/needs-info -label:status/wontdo sort:created-asc" \
       --json number,title --jq '.[] | "#\(.number) \(.title)"'
   done
   echo "== no priority label"
   gh issue list --repo LeanerCloud/cloud-commitments-platform --limit 500 \
     --search "no:assignee -label:status/blocked -label:status/needs-info -label:status/wontdo sort:created-asc -label:priority/p0 -label:priority/p1 -label:priority/p2 -label:priority/p3" \
     --json number,title --jq '.[] | "#\(.number) \(.title)"'
   ```

   Issues with no `priority/*` label come after `p3`. The query only sees
   assignees, so open the issue and check its comments for a claim (step 2)
   too. Before claiming, also check that no open PR already addresses it:
   `gh pr list --repo LeanerCloud/cloud-commitments-platform --search "<issue number> in:body"`.

2. **Claim it before writing any code.** Assign yourself, then re-read the
   assignees and comments. If someone else is also assigned or has posted
   a claim comment, the earliest claim on the issue timeline wins; if that
   is not you, remove yourself and pick another issue.

   ```bash
   gh issue edit <N> --repo LeanerCloud/cloud-commitments-platform --add-assignee @me
   gh issue view <N> --repo LeanerCloud/cloud-commitments-platform --json assignees,comments \
     --jq '{assignees: [.assignees[].login],
            claims: [.comments[] | select(.body | test("(?i)claim"))
                     | "\(.createdAt) \(.author.login): \(.body[0:80])"]}'
   ```

   If you cannot assign yourself (no write access yet), comment
   `Claiming: working on this` on the issue instead. A claim comment
   counts the same as an assignment, including the 7-day rule in step 3.
   After posting, re-read the issue the same way: if an earlier claim
   exists, delete your comment and pick another issue.

3. **Respect other people's claims.** An issue that is assigned, or has a
   claim comment, belongs to that person. Do not work on it, even if it
   looks stale. If it has had no activity and no linked PR for 7 days, ask
   on the issue, mentioning the owner. Take it over once they agree or
   unassign. If they have not replied after 3 more days, comment that you
   are taking it over. That comment is your claim and supersedes only the
   stale owner's claim: when you check for an earlier claim as in step 2,
   ignore the stale owner, but any other claim posted before your takeover
   comment still wins. Then move the assignment to yourself:
   `gh issue edit <N> --repo LeanerCloud/cloud-commitments-platform --add-assignee @me --remove-assignee <old-owner>`
   (or ask a maintainer to, if you lack write access).

4. **Link the PR.** Put `Closes #<N>` in the PR body so the issue and PR
   stay connected (and the PR labeling rule below can find the issue).

5. **Release what you drop.** If you stop working on an issue without a
   PR, unassign yourself (`gh issue edit <N> --repo LeanerCloud/cloud-commitments-platform --remove-assignee @me`)
   and leave a short comment on where you got to, so the next person
   can pick it up.

Work that is not tracked by an issue yet (for example a larger UI or
integration effort) gets an issue first, assigned to whoever is doing it,
so it shows up as taken.

## Post-push CI watcher (MANDATORY — even for one-line fix commits)

After **every** `git push` that publishes new commits to a PR branch on
this repo (including follow-up CodeRabbit-nitpick fixes), launch a
background watcher per workflow run **before** ending the turn. This is
the project-level reinforcement of the global rule in
`~/.claude/git-workflow.md` §Post-push CI watcher — CUDly's pre-commit
job alone takes 9–15 min and is silently broken by routine changes
(missing CI tools, pre-existing markdownlint debt, git-secrets
allowlist drift), so leaving a push unwatched routinely lets CI
failures sit overnight.

Mechanics:

1. After `git push`, list the runs the push triggered:

   ```bash
   gh run list --repo LeanerCloud/cloud-commitments-platform --commit "$(git rev-parse HEAD)" \
     --limit 10 --json databaseId,workflowName,status
   ```

2. For each run still in `queued`/`in_progress`, launch one background
   watcher script via `Bash` with `run_in_background: true` (do NOT use
   foreground polling — the main session must stay unblocked). The
   minimum viable watcher polls `gh run view <ID> --json status,conclusion`
   every 30s and on completion either reports success or dumps the failed
   step digest. A reusable template lives at
   `.claude/scripts/watch-ci-run.sh` if present, or write a one-shot to
   `/tmp/claude/watch-<sha>-<workflow>.sh`.

3. If a watcher reports failure, the same session investigates and
   pushes a fix on the same branch (which fires a fresh watcher round).
   Decisions that need a human go to PR comments; everything else is
   autonomous per the global rule.

Forgetting this rule has been a recurring failure mode in this
project. Before declaring a "pushed and done" turn complete, confirm
at least one `ci-watch-*` background task is armed.

## CodeRabbit loop: iterate to silence (when required by the review gate)

Run this loop when CodeRabbit is the chosen review path, or when the
independent-review condition in "Review gate" below is not met. The full
rules live in `~/.claude/git-workflow.md` §"Post-PR review loop"
(§§3, 3a); read them. The minimum-viable loop for this project:

1. After every push, ping `@coderabbitai review` on the PR (CR doesn't
   always re-review automatically; the explicit ping makes it
   deterministic).
2. Wait for the review (60–120s polling, soft-handle 429s).
3. Triage every Actionable / Outside-diff / Nitpick finding into:
   actionable-fix-now, dismiss-with-justification-on-thread, or
   genuine-nitpick-batch-into-one-fix-commit.
4. Push the fix(es), comment on the PR summarising what was addressed
   vs. dismissed (and why), end the comment with a fresh
   `@coderabbitai review` ping.
5. **Repeat until CR's most recent review has zero Actionable items
   AND every Nitpick is either fixed or has a justification reply.**
   "I fixed pass 1" is not loop-exit. CUDly PRs commonly run 3–6 CR
   passes before settling.
6. If a fix push triggers conflicts (`mergeStateStatus: DIRTY`), resolve
   them per `~/.claude/git-workflow.md` §3a — `git rebase
   origin/<base>`, atomic conflict resolution, `git push
   --force-with-lease`, post a rebase note on the PR, then continue
   the CR loop.

Forgetting this rule leaves CR threads silently unresolved and pushes
the triage burden onto the human reviewer.

When the CodeRabbit loop applies (see Review gate) and you are
**delegating PR work to a subagent**, the prompt MUST include the
full CR loop, not stop at the first `@coderabbitai review` ping. A fork
that pushes the PR, pings CR, then exits leaves the CR threads
unresolved — same failure mode as forgetting the post-push CI watcher.
The subagent's exit criteria must be: "CR's most recent review has zero
Actionable items AND every Nitpick is either fixed or has a
justification reply on the thread", not "PR opened and CR pinged". When
in doubt, copy the iteration loop above (steps 2–6) into the fork
prompt verbatim.

## Review gate

Merge only at the reviewed SHA, and only when all of these cover it:

- A capable independent reviewer adversarially reviews the full PR diff
  and names the SHA.
- All actionable findings from any reviewer (independent review,
  CodeRabbit, CI) are resolved. CodeRabbit is optional when
  exact-revision local verification plus a thorough independent review
  cover the SHA; otherwise run the loop above. CI is green on the SHA.
- Local verification exercises the real affected scenario on macOS
  (Linux via CI; Windows out of scope). Realistic fixtures, mocks,
  recorded responses or local integration count when they exercise the
  actual affected path and data shape, with fail-before/pass-after
  regression proof where applicable. Label that evidence and any
  remaining real-account verification gaps honestly.
- The verdict, the reviewed SHA and the local verification evidence are
  recorded on the PR itself.

Merge normally; never bypass failing or required checks (no
`gh pr merge --admin`, no `--no-verify`). Verification never authorizes
real purchases, deploys or the CLI's `--yes`. Any new commit or rebase
restarts the gate; a missing reviewer or verification blocks the PR.

## PR labeling — mirror closing-issue labels (MANDATORY)

Every PR opened in this repo must carry the **same** triage labels as
the issue it closes — `priority/*`, `severity/*`, `urgency/*`,
`impact/*`, `effort/*`, `type/*`, plus `triaged` if the issue carries
it. Skipping this leaves PRs invisible to the same priority queries
that surface the issues, so an unlabeled PR is effectively
unreviewable in priority order.

Mechanics: apply the labels right after `gh pr create`, in the same
round:

```bash
# Right after `gh pr create ...` returns the PR URL:
# Derive PR_NUM from the current branch context (avoids brittle hand-copying).
PR_NUM=$(gh pr view "$(git rev-parse --abbrev-ref HEAD)" --repo LeanerCloud/cloud-commitments-platform --json number --jq '.number')
ISSUE_NUM=<the issue this PR closes>

LABELS=$(gh issue view "$ISSUE_NUM" --repo LeanerCloud/cloud-commitments-platform --json labels \
  --jq '[.labels[].name | select(test("^(priority|severity|urgency|impact|effort|type)/")) ]
        + (if [.labels[].name] | any(. == "triaged") then ["triaged"] else [] end)
        | join(",")')

# Guard against empty $LABELS — gh pr edit --add-label "" fails, which would
# silently break this MANDATORY flow. If the closing issue has no triage
# labels in the selected classes, surface the gap deterministically instead.
if [ -n "$LABELS" ]; then
  gh pr edit "$PR_NUM" --repo LeanerCloud/cloud-commitments-platform --add-label "$LABELS"
else
  echo "WARN: issue #$ISSUE_NUM has no priority/severity/urgency/impact/effort/type labels"
  echo "      Triage the issue first, then re-run the label-mirror step."
  echo "      Surface this gap in the PR body or as a comment on issue #$ISSUE_NUM."
fi

# Verify
gh pr view "$PR_NUM" --repo LeanerCloud/cloud-commitments-platform --json labels \
  --jq '[.labels[].name] | sort | join(",")'
```

If the closing issue lacks the `triaged` label, do NOT apply
`triaged` to the PR — that would lie. Surface the gap in the PR body
(or as a comment on the issue) so the human can triage.

If a label doesn't yet exist in the repo (rare — the label set is
populated from the existing issue queue), `gh label create` it with
the same color/description as a sibling label BEFORE applying.

For PRs that close more than one issue (e.g., a PR that closes
`#A` and `#B`): take the **highest** `priority/*` and `severity/*`
across the issues; `union` the rest (`type/*`, `effort/*`, `impact/*`,
etc.). The PR represents the work for both, so it should be
discoverable under either filter.

Forgetting this rule has the same shape as forgetting the post-push
CI watcher: it silently breaks priority-ordered review and triage.
Before declaring a "PR opened and done" turn complete, confirm the
label set on the PR matches the closing issue's set (or the merged
set for multi-close PRs).

## Security Rules

- NEVER hardcode API keys, secrets, or credentials in source files
- NEVER commit .env files or any file containing secrets
- Always validate user input at system boundaries
- Always sanitize file paths to prevent directory traversal

## CI/CD IAM — bootstrap vs runtime split

The per-cloud `terraform/environments/*/ci-cd-permissions/` modules provision
the CI/CD deploy identities and are **applied once, manually, by a privileged
human** — not by the CI workflow itself. The main deploy workflow assumes a
deploy SA already exists and only has permission to manage workloads. Keep
this split when adding new IAM:

- **Bootstrap-only permissions** (AWS `iam:*`, Azure RBAC role assignments,
  GCP `roles/iam.roleAdmin`, `roles/resourcemanager.projectIamAdmin`,
  `roles/cloudkms.admin`) live in `ci-cd-permissions/`. They let the deploy
  SA manage its own downstream grants but are not granted to anything
  ephemeral.
- **Runtime permissions** for the Lambda / Cloud Run / Container App service
  accounts are defined in the per-cloud compute module (`modules/compute/
  {aws,gcp,azure}/...`) with the **narrowest possible scope**. Prefer custom
  roles (GCP `google_project_iam_custom_role`) or prefixed resource ARNs
  (AWS `arn:aws:iam::*:role/{prefix}*`) over broad predefined roles like
  `roles/compute.admin` or `Resource = "*"`.
- **No silent fallbacks to over-privileged roles.** If a runtime grant
  requires a bootstrap permission the deploy SA doesn't have, the apply
  SHOULD 403 — that's the signal to re-run the bootstrap, not to paper over
  with a wider grant. Fallback flags are allowed only as short-term
  workarounds and must be removed once the bootstrap has been re-applied.
- **GCP WIF attribute_condition** in `ci-cd-permissions/github_oidc.tf`
  restricts which branch can impersonate the deploy SA. Re-applying the
  module with a different `deploy_ref` (or the default) resets the
  condition. Pin `deploy_ref` in `terraform.tfvars` (gitignored, per-env)
  to avoid silently locking out the current feature branch.

## Multi-Agent Communication

When multiple Claude instances or agents work on this project concurrently, they coordinate through a shared filesystem bus at `~/.claude/agent-comms/`. **Read `~/.claude/multi-agent-comms.md`** for the full protocol.

Key rules:

- Post a `sync` message at session start, after completing work, and before ending
- Post an `intent` message **before committing** and wait ~5s for conflicts
- `claim` the test runner lock before running the full test suite
- Post a `result` after commits and test runs so other agents stay informed
- Check for recent messages when resuming work to avoid conflicts
- Lock `git-commit` and `git-push` resources for the duration of those operations

Directory structure: `~/.claude/agent-comms/{messages,locks,status}`
