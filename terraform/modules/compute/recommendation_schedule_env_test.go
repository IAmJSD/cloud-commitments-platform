// Issue #756: every compute module that schedules collect_recommendations
// hands the same schedule to the API as RECOMMENDATION_SCHEDULE, so GET
// /api/recommendations/freshness can report next_collection_at. The value
// must be empty when the scheduler resource is not created, otherwise the
// API would announce runs that never happen.
package compute_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/api"
)

const scheduleWhenEnabled = `var.enable_scheduled_tasks ? var.recommendation_schedule : ""`

// recommendationScheduleEnv lists, per module file, the expression the
// RECOMMENDATION_SCHEDULE entry must carry.
var recommendationScheduleEnv = map[string]string{
	"aws/lambda/main.tf":    scheduleWhenEnabled,
	"aws/fargate/main.tf":   scheduleWhenEnabled,
	"gcp/cloud-run/main.tf": scheduleWhenEnabled,
	"gcp/gke/main.tf":       scheduleWhenEnabled,
	"azure/aks/main.tf":     scheduleWhenEnabled,
	// The Logic App runs daily at the hour field only (scheduled-tasks.tf),
	// so the env var describes that recurrence rather than the raw cron.
	"azure/container-apps/main.tf": `var.enable_scheduled_tasks ? "0 ${local.schedule_hour} * * *" : ""`,
}

var (
	// Map style: `RECOMMENDATION_SCHEDULE = <expr>`.
	scheduleEnvMapRe = regexp.MustCompile(`(?m)^\s*RECOMMENDATION_SCHEDULE\s*=\s*(.+?)\s*$`)
	// List style: `name = "RECOMMENDATION_SCHEDULE"` followed by
	// `value = <expr>` in the same object (ECS, Kubernetes env blocks and
	// the AKS env list).
	scheduleEnvListRe = regexp.MustCompile(`(?s)name\s*=\s*"RECOMMENDATION_SCHEDULE"\s*,?\s*value\s*=\s*([^\n]+?)\s*(?:,|\n)`)
)

// scheduleEnvValues returns every RECOMMENDATION_SCHEDULE expression in src.
func scheduleEnvValues(src string) []string {
	var out []string
	for _, re := range []*regexp.Regexp{scheduleEnvMapRe, scheduleEnvListRe} {
		for _, m := range re.FindAllStringSubmatch(src, -1) {
			out = append(out, strings.TrimSpace(m[1]))
		}
	}
	return out
}

func TestRecommendationScheduleEnvWiring(t *testing.T) {
	for file, want := range recommendationScheduleEnv {
		t.Run(file, func(t *testing.T) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			got := scheduleEnvValues(stripComments(string(raw)))
			if len(got) != 1 {
				t.Fatalf("want exactly one RECOMMENDATION_SCHEDULE env entry, got %d: %q", len(got), got)
			}
			if got[0] != want {
				t.Errorf("RECOMMENDATION_SCHEDULE = %s; want %s", got[0], want)
			}
		})
	}
}

func TestScheduleEnvValuesShapes(t *testing.T) {
	for name, src := range map[string]string{
		"map":       "  RECOMMENDATION_SCHEDULE = var.x\n",
		"ecs":       "{\n  name  = \"RECOMMENDATION_SCHEDULE\"\n  value = var.x\n}\n",
		"aks list":  "{ name = \"RECOMMENDATION_SCHEDULE\", value = var.x, secret_ref = null },\n",
		"aks multi": "{\n name       = \"RECOMMENDATION_SCHEDULE\"\n value      = var.x\n secret_ref = null\n},\n",
	} {
		if got := scheduleEnvValues(src); len(got) != 1 || got[0] != "var.x" {
			t.Errorf("%s: got %q", name, got)
		}
	}
	if got := scheduleEnvValues("  OTHER = var.x\n"); len(got) != 0 {
		t.Errorf("unrelated entry matched: %q", got)
	}
}

var tfvarsScheduleRe = regexp.MustCompile(`(?m)^\s*recommendation_schedule\s*=\s*"([^"]*)"`)

// recommendationSchedules returns the recommendation_schedule values the
// deployment can end up with: each listed variables.tf default and each value
// a tfvars file matched by tfvarsGlobs sets, keyed by where it came from.
func recommendationSchedules(t *testing.T, varsFiles, tfvarsGlobs []string) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, f := range varsFiles {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		body, ok := blockOnce(stripComments(string(raw)), `variable\s+"recommendation_schedule"\s*\{`)
		m := tfStringDefaultRe.FindStringSubmatch(body)
		if !ok || m == nil {
			t.Fatalf("%s: no single recommendation_schedule variable with a string default", f)
		}
		got[f] = m[1]
	}
	for _, g := range tfvarsGlobs {
		paths, err := filepath.Glob(g)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range paths {
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range tfvarsScheduleRe.FindAllStringSubmatch(stripComments(string(raw)), -1) {
				got[p] = m[1]
			}
		}
	}
	return got
}

// Owner decision D32 (#756): AWS collects at 02:00 UTC like GCP and Azure,
// on a cron() schedule, so GET /api/recommendations/freshness can report
// next_collection_at. Under the old rate(1 day) default the API could not
// know the phase and always reported null on AWS.
func TestRecommendationScheduleDefaultsReportNextCollection(t *testing.T) {
	now := time.Date(2026, 10, 27, 15, 4, 5, 0, time.UTC)
	want := time.Date(2026, 10, 28, 2, 0, 0, 0, time.UTC)
	schedules := recommendationSchedules(t,
		[]string{
			awsEnvDir + "/variables.tf", "aws/lambda/variables.tf", "aws/fargate/variables.tf",
			gcpEnvDir + "/variables.tf", azureEnvDir + "/variables.tf",
		},
		// Committed files only; a developer's untracked dev.tfvars may differ.
		[]string{awsEnvDir + "/github-*.tfvars", awsEnvDir + "/*.tfvars.example", "../../profiles/aws/*.tfvars.example"},
	)
	for _, env := range []string{"dev", "staging", "prod"} {
		if _, ok := schedules[awsEnvDir+"/github-"+env+".tfvars"]; !ok {
			t.Errorf("github-%s.tfvars no longer sets recommendation_schedule; update this test", env)
		}
	}
	for src, expr := range schedules {
		next := api.NewHandler(api.HandlerConfig{RecommendationSchedule: expr}).NextCollectionAt(now)
		if next == nil || !next.Equal(want) {
			t.Errorf("%s: recommendation_schedule %q gives next_collection_at %v; want %s (daily at 02:00 UTC)", src, expr, next, want)
		}
	}
}
