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
//   - the ALB tolerates a short database blip before marking targets
//     unhealthy, since ECS replaces them once it does;
//   - the deploy smoke gate polls /ready via scripts/wait-for-healthy.sh with
//     no warm-up request, like deploy-azure.yml, on the URL that serves the
//     app (an http:// ALB URL only gets a 301 when HTTPS is enabled).
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

// minTimeToUnhealthy is the floor, in seconds, for unhealthy_threshold x
// interval: how long /ready must keep failing before the ALB marks a target
// unhealthy and ECS replaces it. An RDS Multi-AZ failover typically takes
// 60-120s, so anything below this cycles every task on a routine failover.
const minTimeToUnhealthy = 150

// varDefault returns the integer default of a variable in variables.tf.
func varDefault(t *testing.T, vars, name string) int {
	t.Helper()
	v := hclBlock(vars, `variable\s+"`+name+`"`)
	m := regexp.MustCompile(`\bdefault\s*=\s*(\d+)`).FindStringSubmatch(v)
	if m == nil {
		t.Fatalf(`variable %q must exist with a numeric default`, name)
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

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

	if n := varDefault(t, fargateFile(t, "variables.tf"), "health_check_grace_period_seconds"); n < minHealthCheckGracePeriod {
		t.Errorf("health_check_grace_period_seconds defaults to %d; it must be at least %d to cover database initialization", n, minHealthCheckGracePeriod)
	}
}

func TestFargateALBToleratesShortDatabaseBlip(t *testing.T) {
	tg := hclBlock(fargateFile(t, "main.tf"), `resource\s+"aws_lb_target_group"\s+"main"`)
	hc := hclBlock(tg, `\bhealth_check`)
	for _, wire := range []string{`unhealthy_threshold\s*=\s*var\.unhealthy_threshold\b`, `interval\s*=\s*var\.health_check_interval\b`} {
		if !regexp.MustCompile(`\b` + wire).MatchString(hc) {
			t.Errorf("aws_lb_target_group.main health_check must set %s", wire)
		}
	}

	vars := fargateFile(t, "variables.tf")
	threshold, interval := varDefault(t, vars, "unhealthy_threshold"), varDefault(t, vars, "health_check_interval")

	env := hclBlock(stripCommentLines(readFile(t, filepath.Join(repoRoot(t), "terraform", "environments", "aws", "compute.tf"))), `module\s+"compute_fargate"`)
	for name, val := range map[string]*int{"unhealthy_threshold": &threshold, "health_check_interval": &interval} {
		if m := regexp.MustCompile(`\b` + name + `\s*=\s*(\d+)`).FindStringSubmatch(env); m != nil {
			*val, _ = strconv.Atoi(m[1])
		}
	}
	if threshold*interval < minTimeToUnhealthy {
		t.Errorf("unhealthy_threshold (%d) x health_check_interval (%ds) = %ds of failing /ready before ECS replaces a task; it must be at least %ds so a short database blip or Multi-AZ failover does not cycle every task", threshold, interval, threshold*interval, minTimeToUnhealthy)
	}
	if threshold > 10 {
		t.Errorf("unhealthy_threshold %d exceeds the ALB maximum of 10", threshold)
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

	// With HTTPS enabled the port 80 listener answers 301 on every path, so a
	// gate on the http:// ALB URL never reaches a task.
	if !strings.Contains(job, "needs.deploy.outputs.app_url") || strings.Contains(job, "needs.deploy.outputs.alb_url") {
		t.Error("test-deployment must gate on needs.deploy.outputs.app_url (fargate_app_url), not the bare ALB DNS name: with HTTPS enabled http://<ALB> only answers 301")
	}
	if strings.Contains(job, `"http://$`) {
		t.Error("test-deployment must not prepend http:// to the gate URL; fargate_app_url carries the scheme that serves the app")
	}
	if !regexp.MustCompile(`app_url=\$\(terraform output -raw fargate_app_url\b`).MatchString(wf) ||
		!regexp.MustCompile(`\bapp_url:\s*\$\{\{\s*steps\.outputs\.outputs\.app_url\s*\}\}`).MatchString(wf) {
		t.Error("the deploy job must export app_url from `terraform output -raw fargate_app_url`")
	}

	// The fast gate's budget must cover the ECS grace period, or a slow first
	// boot that ECS still tolerates fails the deploy.
	fastIdx := strings.Index(job, "name: Test readiness endpoint")
	if fastIdx < 0 {
		t.Fatal("test-deployment has no Test readiness endpoint step")
	}
	fast := job[fastIdx:]
	attempts := regexp.MustCompile(`HEALTH_MAX_ATTEMPTS:\s*"(\d+)"`).FindStringSubmatch(fast)
	interval := regexp.MustCompile(`HEALTH_INTERVAL_SECONDS:\s*"(\d+)"`).FindStringSubmatch(fast)
	if attempts == nil || interval == nil {
		t.Fatal("Test readiness endpoint must set HEALTH_MAX_ATTEMPTS and HEALTH_INTERVAL_SECONDS")
	}
	a, _ := strconv.Atoi(attempts[1])
	i, _ := strconv.Atoi(interval[1])
	grace := varDefault(t, fargateFile(t, "variables.tf"), "health_check_grace_period_seconds")
	if a*i < grace {
		t.Errorf("Test readiness endpoint polls for %d x %ds = %ds, shorter than the %ds ECS grace period", a, i, a*i, grace)
	}
}

func TestFargateAppURLOutputFollowsListener(t *testing.T) {
	out := stripCommentLines(readFile(t, filepath.Join(repoRoot(t), "terraform", "environments", "aws", "outputs.tf")))
	o := hclBlock(out, `output\s+"fargate_app_url"`)
	if o == "" {
		t.Fatal(`terraform/environments/aws/outputs.tf has no output "fargate_app_url"`)
	}
	// https only when the module enabled its HTTPS listener, and on the alias the
	// wildcard certificate covers (https://<ALB DNS> fails TLS verification).
	if !regexp.MustCompile(`module\.compute_fargate\[0\]\.api_https_url\s*!=\s*null`).MatchString(o) {
		t.Error("fargate_app_url must switch to https exactly when the module's HTTPS listener exists (api_https_url != null)")
	}
	if !strings.Contains(o, `"https://${aws_route53_record.fargate_alb_alias[0].fqdn}"`) {
		t.Error("fargate_app_url must use https on the api-<env> alias (fargate_alb_alias), the name the listener certificate covers")
	}
	if !strings.Contains(o, "module.compute_fargate[0].api_url") {
		t.Error("fargate_app_url must fall back to the module's http api_url when HTTPS is disabled")
	}
}
