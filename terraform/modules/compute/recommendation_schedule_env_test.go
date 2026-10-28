// Issue #756: every compute module that schedules collect_recommendations
// hands the same schedule to the API as RECOMMENDATION_SCHEDULE, so GET
// /api/recommendations/freshness can report next_collection_at. The value
// must be empty when the scheduler resource is not created, otherwise the
// API would announce runs that never happen.
package compute_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
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
