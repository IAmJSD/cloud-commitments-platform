package main

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// ciWorkflowPath is relative to this package's directory, which is where
// `go test` runs it.
var ciWorkflowPath = filepath.Join("..", "..", ".github", "workflows", "ci.yml")

// shippedImageArches are the architectures the image built from this package
// ships for: the AWS deploy workflows build linux/arm64 (Graviton) on
// ubuntu-24.04-arm, and the GCP and Azure deploys build linux/amd64 on
// ubuntu-latest. CI must build and run each of them before a deploy does.
var shippedImageArches = []string{"amd64", "arm64"}

type ciStep struct {
	Name string            `yaml:"name"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	Env  map[string]string `yaml:"env"`
	With map[string]string `yaml:"with"`
}

type ciJob struct {
	RunsOn   string   `yaml:"runs-on"`
	Needs    []string `yaml:"needs"`
	Strategy struct {
		FailFast *bool `yaml:"fail-fast"`
		Matrix   struct {
			Include []map[string]string `yaml:"include"`
		} `yaml:"matrix"`
	} `yaml:"strategy"`
	Steps []ciStep `yaml:"steps"`
}

func loadCIJobs(t *testing.T) map[string]ciJob {
	t.Helper()
	content, err := os.ReadFile(ciWorkflowPath)
	require.NoError(t, err)
	var wf struct {
		Jobs map[string]ciJob `yaml:"jobs"`
	}
	require.NoError(t, yaml.Unmarshal(content, &wf))
	return wf.Jobs
}

// isARMRunner reports whether a GitHub-hosted runner label is an ARM64 one
// (ubuntu-24.04-arm, ubuntu-22.04-arm, ...).
func isARMRunner(label string) bool {
	return strings.HasSuffix(label, "-arm")
}

// TestDockerBuildJob_BuildsEveryShippedArchNatively guards issue #56: the
// docker-build job built only the runner's native linux/amd64, so the arm64
// image the AWS deploys ship was never built or run by CI.
func TestDockerBuildJob_BuildsEveryShippedArchNatively(t *testing.T) {
	jobs := loadCIJobs(t)
	job, ok := jobs["docker-build"]
	require.True(t, ok, "ci.yml has no docker-build job")

	assert.Equal(t, "${{ matrix.runner }}", job.RunsOn, "each leg must run on its matrix runner")
	if assert.NotNil(t, job.Strategy.FailFast, "fail-fast must be stated") {
		assert.False(t, *job.Strategy.FailFast, "one architecture failing must not cancel the other")
	}

	runners := map[string]string{}
	for _, leg := range job.Strategy.Matrix.Include {
		arch := leg["arch"]
		_, dup := runners[arch]
		assert.False(t, dup, "architecture %q appears in more than one matrix leg", arch)
		runners[arch] = leg["runner"]
	}
	assert.ElementsMatch(t, shippedImageArches, slices.Collect(maps.Keys(runners)), "matrix legs must be exactly the shipped architectures")
	for arch, runner := range runners {
		// Native builds only: an amd64 runner building arm64 would need QEMU
		// and would not run the $BUILDPLATFORM stages the way a deploy does.
		assert.Equal(t, arch == "arm64", isARMRunner(runner), "%s leg runs on %q, which is not a native %s runner", arch, runner, arch)
	}

	build := findStep(job.Steps, func(s ciStep) bool { return strings.HasPrefix(s.Uses, "docker/build-push-action@") })
	require.NotNil(t, build, "docker-build has no docker/build-push-action step")
	assert.Equal(t, "linux/${{ matrix.arch }}", build.With["platforms"], "the build must target the leg's architecture explicitly")
	assert.Equal(t, "true", build.With["load"], "the image must be loaded so later steps check this exact artifact")

	verify := findStep(job.Steps, func(s ciStep) bool {
		return s.Env["ARCH"] == "${{ matrix.arch }}" && strings.Contains(s.Run, "docker image inspect")
	})
	assert.NotNil(t, verify, "docker-build must check the built image's architecture against the leg's")

	assert.Contains(t, jobs["ci-success"].Needs, "docker-build", "ci-success must wait for every docker-build leg")
}

func findStep(steps []ciStep, match func(ciStep) bool) *ciStep {
	for i := range steps {
		if match(steps[i]) {
			return &steps[i]
		}
	}
	return nil
}
