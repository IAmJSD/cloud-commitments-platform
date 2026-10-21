// Regression test for issue #718: the DB connection pool (25 max, 2 idle per
// instance, internal/database/config.go) was never sized in IaC. At
// cloud_run_max_instances = 50 in github-prod.tfvars, the idle connections
// alone (50 x 2) filled Cloud SQL's max_connections flag of 100, and a full
// pool per instance asked for 1250.
package gcp_test

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// readFile returns a file's contents or fails the test.
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

// tfvarsInt returns a top-level `key = <int>` assignment from a .tfvars
// file's contents.
func tfvarsInt(src, key string) (int, bool) {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + `\s*=\s*(-?\d+)\s*(#.*)?$`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

// tfvarsString returns a top-level `key = "<value>"` assignment.
func tfvarsString(src, key string) (string, bool) {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + `\s*=\s*"([^"]*)"`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// variableBlock returns the body of `variable "<name>" { ... }`.
func variableBlock(src, name string) (string, bool) {
	start := strings.Index(src, `variable "`+name+`" {`)
	if start < 0 {
		return "", false
	}
	depth := 0
	for i := start; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : i+1], true
			}
		}
	}
	return "", false
}

// variableDefault returns a variable's `default = <value>` (int or quoted
// string, unquoted on return).
func variableDefault(src, name string) (string, bool) {
	block, ok := variableBlock(src, name)
	if !ok {
		return "", false
	}
	m := regexp.MustCompile(`(?m)^\s*default\s*=\s*"?([^"\s]+)"?`).FindStringSubmatch(block)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// appEnvDefault returns the default the app uses when env var key is unset,
// from internal/database/config.go's getEnvInt("<key>", <default>).
func appEnvDefault(t *testing.T, key string) int {
	t.Helper()
	src := readFile(t, "../../../internal/database/config.go")
	m := regexp.MustCompile(`getEnvInt\("` + regexp.QuoteMeta(key) + `",\s*(\d+)\)`).FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("internal/database/config.go no longer reads %s via getEnvInt; update this test", key)
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// intSetting resolves a setting the way Terraform would for one tfvars
// file: the tfvars value, else the variable's default, else fallback (what
// applies when the variable does not exist at all).
func intSetting(t *testing.T, tfvars, variables, key string, fallback int) int {
	t.Helper()
	if n, ok := tfvarsInt(tfvars, key); ok {
		return n
	}
	if d, ok := variableDefault(variables, key); ok {
		n, err := strconv.Atoi(d)
		if err != nil {
			t.Fatalf("variable %q default %q is not an integer", key, d)
		}
		return n
	}
	return fallback
}

// reservedConnections returns local.db_reserved_connections from database.tf.
func reservedConnections(t *testing.T) int {
	t.Helper()
	m := regexp.MustCompile(`(?m)^\s*db_reserved_connections\s*=\s*(\d+)`).FindStringSubmatch(readFile(t, "database.tf"))
	if m == nil {
		t.Error("database.tf: missing local db_reserved_connections (connections kept out of the app's budget, #718)")
		return 10
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func TestDBPoolFitsCloudSQLMaxConnections(t *testing.T) {
	variables := readFile(t, "variables.tf")
	reserved := reservedConnections(t)
	appMax := appEnvDefault(t, "DB_MAX_CONNECTIONS")
	appMin := appEnvDefault(t, "DB_MIN_CONNECTIONS")

	for _, file := range []string{"github-dev.tfvars", "github-staging.tfvars", "github-prod.tfvars"} {
		t.Run(file, func(t *testing.T) {
			tfvars := readFile(t, file)
			platform, ok := tfvarsString(tfvars, "compute_platform")
			if !ok {
				platform, _ = variableDefault(variables, "compute_platform")
			}
			if platform != "cloud-run" {
				t.Skipf("compute_platform = %q; the budget below covers Cloud Run", platform)
			}

			instances := intSetting(t, tfvars, variables, "cloud_run_max_instances", 0)
			poolMax := intSetting(t, tfvars, variables, "db_max_connections", appMax)
			poolMin := intSetting(t, tfvars, variables, "db_min_connections", appMin)
			// 100 is modules/database/gcp's max_connections flag default,
			// which applies while the environment does not pass its own.
			dbMax := intSetting(t, tfvars, variables, "database_max_connections", 100)

			if instances <= 0 {
				t.Fatalf("cloud_run_max_instances resolved to %d", instances)
			}
			budget := dbMax - reserved
			if instances*poolMax > budget {
				t.Errorf("cloud_run_max_instances x db_max_connections = %d x %d = %d exceeds Cloud SQL max_connections %d - %d reserved = %d: new instances cannot connect under full scale-out (#718)",
					instances, poolMax, instances*poolMax, dbMax, reserved, budget)
			}
			if poolMax < 2 {
				t.Errorf("db_max_connections = %d; a scheduled task holds one pooled connection for its advisory lock while it queries through the pool, so it needs at least 2", poolMax)
			}
			if poolMin < 0 || poolMin > poolMax {
				t.Errorf("db_min_connections = %d must be between 0 and db_max_connections (%d)", poolMin, poolMax)
			}
		})
	}
}

func TestDBPoolEnvWiredIntoEveryComputeModule(t *testing.T) {
	database := readFile(t, "database.tf")
	for _, key := range []string{"DB_MAX_CONNECTIONS", "DB_MIN_CONNECTIONS"} {
		appEnvDefault(t, key) // the app must actually read this name
		varName := "db_" + strings.ToLower(strings.TrimPrefix(key, "DB_"))
		want := regexp.MustCompile(`(?m)^\s*` + key + `\s*=\s*tostring\(var\.` + varName + `\)`)
		if !want.MatchString(database) {
			t.Errorf("database.tf: local.db_pool_env must set %s = tostring(var.%s)", key, varName)
		}
	}

	compute := readFile(t, "compute.tf")
	merges := strings.Count(compute, "additional_env_vars = merge(")
	if merges < 2 {
		t.Fatalf("compute.tf: expected the cloud-run and gke additional_env_vars merges, found %d", merges)
	}
	// local.db_pool_env must be the last argument of every merge so it is
	// what the container sees.
	wired := len(regexp.MustCompile(`var\.additional_env_vars,\s*local\.db_pool_env\s*\)`).FindAllString(compute, -1))
	if wired != merges {
		t.Errorf("compute.tf: %d of %d additional_env_vars merges end with local.db_pool_env; every compute platform must get the sized pool (#718)", wired, merges)
	}
}

func TestDBPoolBudgetGuardsPresent(t *testing.T) {
	variables := readFile(t, "variables.tf")
	database := readFile(t, "database.tf")

	guards := []struct {
		file, src, variable, pattern string
	}{
		{"variables.tf", variables, "db_max_connections", `condition\s*=\s*var\.compute_platform\s*!=\s*"cloud-run"\s*\|\|\s*var\.cloud_run_max_instances\s*\*\s*var\.db_max_connections\s*<=\s*var\.database_max_connections\s*-\s*local\.db_reserved_connections`},
		{"variables.tf", variables, "db_max_connections", `condition\s*=\s*var\.db_max_connections\s*>=\s*2\b`},
		{"variables.tf", variables, "db_min_connections", `condition\s*=\s*var\.db_min_connections\s*>=\s*0\s*&&\s*var\.db_min_connections\s*<=\s*var\.db_max_connections`},
		{"variables.tf", variables, "additional_env_vars", `!contains\(keys\(var\.additional_env_vars\),\s*"DB_MAX_CONNECTIONS"\)\s*&&\s*!contains\(keys\(var\.additional_env_vars\),\s*"DB_MIN_CONNECTIONS"\)`},
	}
	for _, g := range guards {
		block, ok := variableBlock(g.src, g.variable)
		if !ok {
			t.Errorf("%s: missing variable %q (#718)", g.file, g.variable)
			continue
		}
		if !regexp.MustCompile(g.pattern).MatchString(block) {
			t.Errorf("%s: variable %q is missing the validation %s (#718)", g.file, g.variable, g.pattern)
		}
	}

	// The budget is checked against database_max_connections, so that must be
	// the value Cloud SQL actually gets.
	flag := regexp.MustCompile(`name\s*=\s*"max_connections"\s*\n\s*value\s*=\s*tostring\(var\.database_max_connections\)`)
	if !flag.MatchString(database) {
		t.Error("database.tf: module.database must set the max_connections flag from var.database_max_connections (#718)")
	}
}
