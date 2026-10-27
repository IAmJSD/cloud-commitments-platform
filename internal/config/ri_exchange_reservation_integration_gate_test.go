package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"
)

// riExchangeReservationIntegrationFiles hold the only real-PostgreSQL coverage
// of the RI-exchange daily-cap reservation. They are behind //go:build
// integration, so this parse-only check is what a default `go test` sees.
var riExchangeReservationIntegrationFiles = []string{
	"store_postgres_ri_exchange_reservation_integration_test.go",
	"store_postgres_ri_exchange_approved_reservation_integration_test.go",
	filepath.Join("..", "server", "handler_ri_exchange_reservation_integration_test.go"),
	"store_postgres_auto_purchase_stamps_integration_test.go",
	"store_postgres_ladder_arming_integration_test.go",
}

// TestRIExchangeReservationIntegrationTestsAreNotEnvGated guards #724 and #202. These
// tests used to read RI_EXCHANGE_TEST_DATABASE_URL and t.Skip when it was
// unset (the auto-purchase stamp and ladder arming tests, #202, did the same
// with their own variables), which CI never set, so the spend-limiting path
// ran nowhere. They now
// get their database from testhelpers.RequirePostgresContainer, which decides
// on its own whether Docker is missing. Any os.Getenv or direct skip in these
// files would bring back a way to run them without running them.
func TestRIExchangeReservationIntegrationTestsAreNotEnvGated(t *testing.T) {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file")
	}
	dir := filepath.Dir(self)
	fset := token.NewFileSet()
	for _, name := range riExchangeReservationIntegrationFiles {
		path := filepath.Join(dir, name)
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, call := range envGateCalls(f) {
			t.Errorf("%s: %s gates real-database coverage on the environment; "+
				"use testhelpers.RequirePostgresContainer instead", fset.Position(call.Pos()), callName(call))
		}
	}
}

// envGateCalls returns every os.Getenv / os.LookupEnv call and every
// Skip / Skipf / SkipNow call in f.
func envGateCalls(f *ast.File) []*ast.CallExpr {
	var found []*ast.CallExpr
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch callName(call) {
		case "os.Getenv", "os.LookupEnv", "Skip", "Skipf", "SkipNow":
			found = append(found, call)
		}
		return true
	})
	return found
}

// callName renders os.X calls as "os.X" and any other method call by its bare
// selector, so t.Skip and a renamed *testing.T both match "Skip".
func callName(call *ast.CallExpr) string {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	if pkg, isIdent := sel.X.(*ast.Ident); isIdent && pkg.Name == "os" {
		return "os." + sel.Sel.Name
	}
	return sel.Sel.Name
}
