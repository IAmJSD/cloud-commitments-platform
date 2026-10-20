// Contract test for issue #714: the per-cloud monitoring modules alert on
// marker tokens the Go code logs when a purchase execution or a scheduled task
// fails. A rename on either side would silently stop the alerts, so every
// module must match the exact tokens the code emits.
package monitoring_test

import (
	"os"
	"regexp"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/purchase"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/server"
)

func TestMonitoringLogMarkersMatchGo(t *testing.T) {
	markers := map[string]string{
		"purchase.PurchaseFailedLogMarker":    purchase.PurchaseFailedLogMarker,
		"server.ScheduledTaskFailedLogMarker": server.ScheduledTaskFailedLogMarker,
	}
	// Each module's failure_markers map must carry the token as a `token = "..."`
	// attribute, so a token that merely appears in a comment does not count.
	for _, file := range []string{"aws/failure_alerts.tf", "gcp/failure_alerts.tf", "azure/failure_alerts.tf"} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		src := string(data)
		for name, token := range markers {
			if token == "" {
				t.Fatalf("%s is empty", name)
			}
			re := regexp.MustCompile(`(?m)^\s*token\s*=\s*"` + regexp.QuoteMeta(token) + `"\s*$`)
			if !re.MatchString(src) {
				t.Errorf("%s: no failure_markers entry with token = %q (%s)", file, token, name)
			}
		}
	}
}
