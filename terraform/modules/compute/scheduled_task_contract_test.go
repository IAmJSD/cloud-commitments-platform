// Contract test for issue #710: every scheduler resource in the compute
// modules must name a task the server actually recognizes. GCP and Azure
// pointed at /api/scheduled/recommendations and /api/scheduled/ri-exchange,
// and the AWS Lambda rule sent {event: "scheduled_recommendations"}; none of
// those are in the server's task roster, so every scheduled run failed.
package compute_test

import (
	"os"
	"regexp"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/server"
)

// schedulerRefs lists, per file, the regexp whose first capture group is a
// task name the scheduler will send to the server, and the minimum number of
// references expected (guards against the regexp silently matching nothing).
var schedulerRefs = []struct {
	file string
	re   *regexp.Regexp
	min  int
}{
	{"gcp/cloud-run/main.tf", regexp.MustCompile(`/api/scheduled/([A-Za-z0-9_-]+)`), 2},
	{"gcp/gke/main.tf", regexp.MustCompile(`/api/scheduled/([A-Za-z0-9_-]+)`), 1},
	{"azure/container-apps/scheduled-tasks.tf", regexp.MustCompile(`/api/scheduled/([A-Za-z0-9_-]+)`), 3},
	// Lambda EventBridge targets must send the "action" key; any other key
	// (the old "event") is not recognized by the Lambda event router.
	{"aws/lambda/main.tf", regexp.MustCompile(`jsonencode\(\{\s*action\s*=\s*"([^"]+)"`), 6},
	{"aws/fargate/main.tf", regexp.MustCompile(`"--task",\s*"([^"]+)"`), 4},
}

func TestSchedulerTargetsNameKnownServerTasks(t *testing.T) {
	for _, ref := range schedulerRefs {
		data, err := os.ReadFile(ref.file)
		if err != nil {
			t.Fatalf("reading %s: %v", ref.file, err)
		}
		matches := ref.re.FindAllStringSubmatch(string(data), -1)
		if len(matches) < ref.min {
			t.Errorf("%s: found %d scheduler task references, want at least %d", ref.file, len(matches), ref.min)
		}
		for _, m := range matches {
			if _, err := server.ParseScheduledTaskType(m[1]); err != nil {
				t.Errorf("%s: scheduler sends task %q which the server does not recognize", ref.file, m[1])
			}
		}
	}
}

func TestLambdaScheduledTargetsUseActionKey(t *testing.T) {
	data, err := os.ReadFile("aws/lambda/main.tf")
	if err != nil {
		t.Fatal(err)
	}
	if bad := regexp.MustCompile(`jsonencode\(\{\s*event\s*=`).FindString(string(data)); bad != "" {
		t.Errorf("aws/lambda/main.tf has an EventBridge input keyed %q; the Lambda router only reads \"action\"", bad)
	}
}
