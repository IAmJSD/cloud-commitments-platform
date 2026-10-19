// Regression test for issue #648: after #488 added /ready (503 until the
// replica has completed initialization), the GKE readiness probe and the
// Cloud Run startup probe still gated traffic on /health, which answers 200
// even while the replica is cold, so uninitialized replicas were admitted to
// traffic. Liveness must stay on /health so a slow init never gets the
// container killed.
package gcp_test

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

func readTF(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

// probeBlock returns the body of the first `<name> { ... }` block, matching
// braces so nested http_get blocks are included.
func probeBlock(t *testing.T, content, name string) string {
	t.Helper()
	loc := regexp.MustCompile(`(?m)^\s*` + name + `\s*\{`).FindStringIndex(content)
	if loc == nil {
		t.Fatalf("no %s block found", name)
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
	t.Fatalf("unterminated %s block", name)
	return ""
}

func blockPath(block string) string {
	m := regexp.MustCompile(`path\s*=\s*(\S+)`).FindStringSubmatch(block)
	if m == nil {
		return ""
	}
	return m[1]
}

func blockInt(t *testing.T, block, attr string) int {
	t.Helper()
	m := regexp.MustCompile(attr + `\s*=\s*(\d+)`).FindStringSubmatch(block)
	if m == nil {
		t.Fatalf("probe block has no numeric %s", attr)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("parsing %s: %v", attr, err)
	}
	return n
}

func variableDefault(t *testing.T, vars, name string) string {
	t.Helper()
	block := regexp.MustCompile(`(?s)variable\s+"` + name + `"\s*\{.*?\n\}`).FindString(vars)
	if block == "" {
		t.Fatalf("variable %q is not declared", name)
	}
	m := regexp.MustCompile(`default\s*=\s*"([^"]*)"`).FindStringSubmatch(block)
	if m == nil {
		t.Fatalf("variable %q has no string default", name)
	}
	return m[1]
}

func TestGKEReadinessProbeGatesOnReady(t *testing.T) {
	main := readTF(t, "gke/main.tf")
	vars := readTF(t, "gke/variables.tf")

	if got := blockPath(probeBlock(t, main, "readiness_probe")); got != "var.readiness_probe_path" {
		t.Errorf("GKE readiness_probe path = %s, want var.readiness_probe_path (#648): readiness is the traffic-admission signal and must not share the liveness path", got)
	}
	if got := variableDefault(t, vars, "readiness_probe_path"); got != "/ready" {
		t.Errorf(`var.readiness_probe_path default = %q, want "/ready" (#648); /health answers 200 on a cold replica`, got)
	}

	if got := blockPath(probeBlock(t, main, "liveness_probe")); got != "var.health_check_path" {
		t.Errorf("GKE liveness_probe path = %s, want var.health_check_path: liveness must stay on /health so a slow init does not restart the pod", got)
	}
	if got := variableDefault(t, vars, "health_check_path"); got != "/health" {
		t.Errorf(`var.health_check_path default = %q, want "/health": pointing liveness at /ready would kill pods whose init is slow`, got)
	}
}

func TestCloudRunStartupProbeGatesOnReady(t *testing.T) {
	main := readTF(t, "cloud-run/main.tf")

	startup := probeBlock(t, main, "startup_probe")
	if got := blockPath(startup); got != `"/ready"` {
		t.Errorf(`Cloud Run startup_probe path = %s, want "/ready" (#648): Cloud Run admits an instance once its startup probe passes`, got)
	}

	// A startup probe that never passes shuts the container down, so the
	// window must outlast the 120 s default migration timeout plus connect
	// retries, and stay inside Cloud Run's 600 s cap on
	// failure_threshold x period_seconds.
	window := blockInt(t, startup, "failure_threshold") * blockInt(t, startup, "period_seconds")
	if window < 180 {
		t.Errorf("Cloud Run startup probe window = %d s, want >= 180 s so a slow-but-recovering init does not restart-loop the container", window)
	}
	if window > 600 {
		t.Errorf("Cloud Run startup probe window = %d s, exceeds Cloud Run's 600 s limit on failure_threshold x period_seconds", window)
	}
	if timeout, period := blockInt(t, startup, "timeout_seconds"), blockInt(t, startup, "period_seconds"); timeout > period {
		t.Errorf("Cloud Run startup probe timeout_seconds (%d) must not exceed period_seconds (%d)", timeout, period)
	}

	if got := blockPath(probeBlock(t, main, "liveness_probe")); got != `"/health"` {
		t.Errorf(`Cloud Run liveness_probe path = %s, want "/health": liveness must not depend on initialization`, got)
	}
}
