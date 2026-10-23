//go:build integration

package api

import (
	"log"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
)

// Integration tests that take a database from testhelpers.RequireMigratedDatabase
// share one PostgreSQL container per test binary (issue #982); stop it once
// every test has finished.
func init() {
	afterAllTests = append(afterAllTests, func() {
		if err := testhelpers.TerminateSharedPostgres(); err != nil {
			// Ryuk reaps the container anyway, so a teardown hiccup does not
			// overrule the tests' own verdict.
			log.Printf("terminate shared postgres container: %v", err)
		}
	})
}
