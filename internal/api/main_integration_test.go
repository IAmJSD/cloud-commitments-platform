//go:build integration

package api

import (
	"log"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
)

// The integration tests in this package share one PostgreSQL container (issue
// #982): each still gets a fresh database of its own from
// testhelpers.RequireMigratedDatabase or RequireEmptyDatabase, but the server
// and its migrated template outlive any one test. TestMain, in
// main_nonet_test.go because it exists in every build, stops the server after
// the last test.
func init() {
	afterTests = append(afterTests, func() {
		if err := testhelpers.TerminateSharedPostgres(); err != nil {
			// Ryuk reaps the container anyway, so a teardown hiccup does not
			// overrule the tests' own verdict.
			log.Printf("terminate shared postgres container: %v", err)
		}
	})
}
