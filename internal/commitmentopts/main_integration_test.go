//go:build integration
// +build integration

package commitmentopts_test

import (
	"log"
	"os"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
)

// TestMain stops the PostgreSQL container that every integration test in this
// package shares (issue #982). Each test still gets a fresh database of its own
// from testhelpers.RequireMigratedDatabase or RequireEmptyDatabase; only the
// container and the migrated template it is cloned from outlive a single test.
func TestMain(m *testing.M) {
	code := m.Run()
	if err := testhelpers.TerminateSharedPostgres(); err != nil {
		// Ryuk reaps the container anyway, so a teardown hiccup does not
		// overrule the tests' own verdict.
		log.Printf("terminate shared postgres container: %v", err)
	}
	os.Exit(code)
}
