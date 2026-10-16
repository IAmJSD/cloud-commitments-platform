// Regression tests for issue #647: the Fargate ALB target group health check
// gated traffic on /health, which answers HTTP 200 even while the task is still
// initializing (degraded), so the ALB routed production traffic to cold tasks.
// PR #646 (#488) added /ready (503 until initialization completes) and moved
// every Azure traffic-admission check to it; these tests pin the AWS side of
// that contract:
//
//   - the ALB target group check (traffic admission) uses /ready and only
//     accepts HTTP 200;
//   - the ECS container health check (liveness: failing it replaces the task)
//     stays on /health;
//   - the ECS service has a health check grace period, because ECS also
//     replaces tasks the target group reports unhealthy, and a task still
//     connecting to the database would otherwise be killed before it is ready;
//   - the deploy smoke gate polls /ready via scripts/wait-for-healthy.sh with
//     no warm-up request, like deploy-azure.yml.
package aws_test

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// minHealthCheckGracePeriod is the floor for the ECS grace period default. The
// database connect retry budget alone is ~135s (internal/database
// connection.go), before migrations run.
const minHealthCheckGracePeriod = 180

// hclBlock returns the brace-balanced body of the first block whose header
// matches header (a regexp ending just before the opening brace), or "" when
// there is none. Braces inside "${...}" interpolations are balanced, so plain
// counting is enough for the files under test.
func hclBlock(content, header string) string {
	loc := regexp.MustCompile(header + `\s*\{`).FindStringIndex(content)
	if loc == nil {
		return ""
	}
	depth := 0
	for i := loc[1] - 1; i < len(content); i++ {
		switch content[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return content[loc[1]:i]
			}
		}
	}
	return ""
}

func fargateFile(t *testing.T, name string) string {
	t.Helper()
	return stripCommentLines(readFile(t, filepath.Join("fargate", name)))
}

func TestFargateALBHealthCheckUsesReadiness(t *testing.T) {
	main := fargateFile(t, "main.tf")
	tg := hclBlock(main, `resource\s+"aws_lb_target_group"\s+"main"`)
	if tg == "" {
		t.Fatal(`fargate/main.tf has no resource "aws_lb_target_group" "main"`)
	}
	hc := hclBlock(tg, `\bhealth_check`)
	if hc == "" {
		t.Fatal("aws_lb_target_group.main has no health_check block")
	}
	if !regexp.MustCompile(`\bpath\s*=\s*var\.health_check_path\b`).MatchString(hc) {
		t.Error("aws_lb_target_group.main health_check.path must be var.health_check_path")
	}
	if !regexp.MustCompile(`\bmatcher\s*=\s*"200"`).MatchString(hc) {
		t.Error(`aws_lb_target_group.main health_check.matcher must be "200": /ready signals not-ready with 503, and a wider matcher would admit it`)
	}

	vars := fargateFile(t, "variables.tf")
	v := hclBlock(vars, `variable\s+"health_check_path"`)
	if !regexp.MustCompile(`\bdefault\s*=\s*"/ready"`).MatchString(v) {
		t.Error(`variable "health_check_path" must default to "/ready" (#647): the ALB check admits traffic, and /health answers 200 while the task is still initializing`)
	}

	env := stripCommentLines(readFile(t, filepath.Join(repoRoot(t), "terraform", "environments", "aws", "compute.tf")))
	mod := hclBlock(env, `module\s+"compute_fargate"`)
	if mod == "" {
		t.Fatal(`terraform/environments/aws/compute.tf has no module "compute_fargate"`)
	}
	if m := regexp.MustCompile(`\bhealth_check_path\s*=\s*"([^"]*)"`).FindStringSubmatch(mod); m != nil && m[1] != "/ready" {
		t.Errorf(`module "compute_fargate" overrides health_check_path with %q; the ALB traffic-admission check must use "/ready" (#647)`, m[1])
	}
}

func TestFargateContainerHealthCheckStaysOnLiveness(t *testing.T) {
	main := fargateFile(t, "main.tf")
	td := hclBlock(main, `resource\s+"aws_ecs_task_definition"\s+"main"`)
	hc := hclBlock(td, `\bhealthCheck\s*=`)
	if hc == "" {
		t.Fatal("aws_ecs_task_definition.main has no container healthCheck")
	}
	if !strings.Contains(hc, "localhost:8080/health ") {
		t.Error("the ECS container healthCheck must probe /health: it is liveness (failing it replaces the task)")
	}
	if strings.Contains(hc, "/ready") {
		t.Error("the ECS container healthCheck must not probe /ready: a database outage would make ECS replace every task")
	}
}

func TestFargateServiceHasHealthCheckGracePeriod(t *testing.T) {
	main := fargateFile(t, "main.tf")
	svc := hclBlock(main, `resource\s+"aws_ecs_service"\s+"main"`)
	if svc == "" {
		t.Fatal(`fargate/main.tf has no resource "aws_ecs_service" "main"`)
	}
	if !regexp.MustCompile(`\bhealth_check_grace_period_seconds\s*=\s*var\.health_check_grace_period_seconds\b`).MatchString(svc) {
		t.Error("aws_ecs_service.main must set health_check_grace_period_seconds = var.health_check_grace_period_seconds, or ECS replaces tasks that fail /ready while still initializing")
	}

	vars := fargateFile(t, "variables.tf")
	v := hclBlock(vars, `variable\s+"health_check_grace_period_seconds"`)
	m := regexp.MustCompile(`\bdefault\s*=\s*(\d+)`).FindStringSubmatch(v)
	if m == nil {
		t.Fatal(`variable "health_check_grace_period_seconds" must exist with a numeric default`)
	}
	if n, _ := strconv.Atoi(m[1]); n < minHealthCheckGracePeriod {
		t.Errorf("health_check_grace_period_seconds defaults to %d; it must be at least %d to cover database initialization", n, minHealthCheckGracePeriod)
	}
}

func TestFargateDeploySmokeGateUsesReadiness(t *testing.T) {
	wf := stripCommentLines(readFile(t, filepath.Join(repoRoot(t), ".github", "workflows", "deploy-aws-fargate.yml")))
	idx := strings.Index(wf, "\n  test-deployment:")
	if idx < 0 {
		t.Fatal("deploy-aws-fargate.yml has no test-deployment job")
	}
	job := wf[idx+1:]
	if end := regexp.MustCompile(`\n  [A-Za-z0-9_-]+:\s*\n`).FindStringIndex(job[1:]); end != nil {
		job = job[:end[0]+1]
	}

	if n := strings.Count(job, `bash scripts/wait-for-healthy.sh "$APP_URL"`); n < 2 {
		t.Errorf("test-deployment must gate on scripts/wait-for-healthy.sh for both the readiness and smoke steps (found %d), like deploy-azure.yml", n)
	}
	if !strings.Contains(job, "sparse-checkout: scripts/wait-for-healthy.sh") {
		t.Error("test-deployment must check out scripts/wait-for-healthy.sh before running it")
	}
	if strings.Contains(job, "/api/auth/check-admin") {
		t.Error("test-deployment must not send a warm-up request: the server initializes in the background (#488), and a warm-up only initializes whichever task the ALB picked")
	}
	if regexp.MustCompile(`/health\b`).MatchString(job) {
		t.Error("test-deployment must not gate on /health: it answers 200 even while degraded; use /ready via scripts/wait-for-healthy.sh")
	}
}
