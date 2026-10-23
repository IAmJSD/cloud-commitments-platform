//go:build integration
// +build integration

package auth

import (
	"log"
	"os"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
)

// TestMain stops the PostgreSQL container shared by the integration tests in
// this package that take their database from testhelpers.RequireMigratedDatabase
// (issue #982). Each such test still gets a freshly migrated database of its
// own; only the container and the migrated template it is cloned from outlive
// a single test.
func TestMain(m *testing.M) {
	code := m.Run()
	if err := testhelpers.TerminateSharedPostgres(); err != nil {
		// Ryuk reaps the container anyway, so a teardown hiccup does not
		// overrule the tests' own verdict.
		log.Printf("terminate shared postgres container: %v", err)
	}
	os.Exit(code)
}
