// Regression test for issue #718: the DB connection pool (25 max, 2 idle per
// instance, internal/database/config.go) was never sized in IaC, and Lambda
// concurrency was unreserved (lambda_reserved_concurrency = -1) with no RDS
// Proxy, so a burst of execution environments, each holding its own pool,
// could exhaust the RDS max_connections.
package aws_test

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

// rdsMaxConnectionsFallback is what this test assumes for the default
// db.t4g.micro while variables.tf does not declare database_max_connections.
const rdsMaxConnectionsFallback = 80

func TestDBPoolFitsRDSMaxConnections(t *testing.T) {
	variables := readFile(t, "variables.tf")
	reserved := reservedConnections(t)
	appMax := appEnvDefault(t, "DB_MAX_CONNECTIONS")
	appMin := appEnvDefault(t, "DB_MIN_CONNECTIONS")

	for _, env := range []string{"dev", "staging", "prod"} {
		file := "github-" + env + ".tfvars"
		t.Run(file, func(t *testing.T) {
			tfvars := readFile(t, file)
			platform, ok := tfvarsString(tfvars, "compute_platform")
			if !ok {
				platform, _ = variableDefault(variables, "compute_platform")
			}
			poolMax := intSetting(t, tfvars, variables, "db_max_connections", appMax)
			poolMin := intSetting(t, tfvars, variables, "db_min_connections", appMin)
			dbMax := intSetting(t, tfvars, variables, "database_max_connections", rdsMaxConnectionsFallback)
			budget := dbMax - reserved

			if poolMax < 2 {
				t.Errorf("db_max_connections = %d; a scheduled task holds one pooled connection for its advisory lock while it queries through the pool, so it needs at least 2", poolMax)
			}
			if poolMin < 0 || poolMin > poolMax {
				t.Errorf("db_min_connections = %d must be between 0 and db_max_connections (%d)", poolMin, poolMax)
			}

			// Fargate's cap is always set, so check it even where Lambda is
			// the active platform: switching compute_platform must not
			// silently overrun the database.
			fargate := intSetting(t, tfvars, variables, "fargate_max_capacity", 0)
			if fargate*poolMax > budget {
				t.Errorf("fargate_max_capacity x db_max_connections = %d x %d = %d exceeds RDS max_connections %d - %d reserved = %d (#718)",
					fargate, poolMax, fargate*poolMax, dbMax, reserved, budget)
			}

			if platform != "lambda" {
				return
			}
			lambda := intSetting(t, tfvars, variables, "lambda_reserved_concurrency", -1)
			if lambda < 0 {
				if env == "prod" {
					t.Errorf("prod runs on Lambda with lambda_reserved_concurrency = %d: concurrency is unbounded, so a burst of execution environments, each with its own pool of %d, can exhaust RDS max_connections %d (#718)", lambda, poolMax, dbMax)
				}
				return
			}
			if lambda*poolMax > budget {
				t.Errorf("lambda_reserved_concurrency x db_max_connections = %d x %d = %d exceeds RDS max_connections %d - %d reserved = %d (#718)",
					lambda, poolMax, lambda*poolMax, dbMax, reserved, budget)
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
		t.Fatalf("compute.tf: expected the lambda and fargate additional_env_vars merges, found %d", merges)
	}
	// local.db_pool_env must be the last argument of every merge so it is
	// what the function or task sees.
	wired := len(regexp.MustCompile(`var\.additional_env_vars,\s*local\.db_pool_env\s*\)`).FindAllString(compute, -1))
	if wired != merges {
		t.Errorf("compute.tf: %d of %d additional_env_vars merges end with local.db_pool_env; every compute platform must get the sized pool (#718)", wired, merges)
	}
}

func TestDBPoolBudgetGuardsPresent(t *testing.T) {
	variables := readFile(t, "variables.tf")

	guards := []struct {
		variable, pattern string
	}{
		{"db_max_connections", `condition\s*=\s*\(var\.compute_platform\s*==\s*"lambda"\s*\?\s*max\(var\.lambda_reserved_concurrency,\s*0\)\s*:\s*var\.fargate_max_capacity\)\s*\*\s*var\.db_max_connections\s*<=\s*var\.database_max_connections\s*-\s*local\.db_reserved_connections`},
		{"db_max_connections", `condition\s*=\s*var\.db_max_connections\s*>=\s*2\b`},
		{"db_min_connections", `condition\s*=\s*var\.db_min_connections\s*>=\s*0\s*&&\s*var\.db_min_connections\s*<=\s*var\.db_max_connections`},
		{"lambda_reserved_concurrency", `condition\s*=\s*!\(var\.environment\s*==\s*"prod"\s*&&\s*var\.compute_platform\s*==\s*"lambda"\s*&&\s*var\.lambda_reserved_concurrency\s*<\s*0\)`},
		{"additional_env_vars", `!contains\(keys\(var\.additional_env_vars\),\s*"DB_MAX_CONNECTIONS"\)\s*&&\s*!contains\(keys\(var\.additional_env_vars\),\s*"DB_MIN_CONNECTIONS"\)`},
	}
	for _, g := range guards {
		block, ok := variableBlock(variables, g.variable)
		if !ok {
			t.Errorf("variables.tf: missing variable %q (#718)", g.variable)
			continue
		}
		if !regexp.MustCompile(g.pattern).MatchString(block) {
			t.Errorf("variables.tf: variable %q is missing the validation %s (#718)", g.variable, g.pattern)
		}
	}

	checks := readFile(t, "checks.tf")
	if !strings.Contains(checks, `check "lambda_concurrency_bounds_db_connections"`) ||
		!regexp.MustCompile(`condition\s*=\s*!\(var\.compute_platform\s*==\s*"lambda"\s*&&\s*var\.lambda_reserved_concurrency\s*<\s*0\)`).MatchString(checks) {
		t.Error(`checks.tf: missing check "lambda_concurrency_bounds_db_connections" warning on unbounded Lambda concurrency (#718)`)
	}
}

// Setting reserved_concurrent_executions makes the AWS provider call
// PutFunctionConcurrency (and DeleteFunctionConcurrency to go back to -1), so
// without these grants the first prod apply with a cap fails with
// AccessDenied.
func TestDeployRoleCanSetLambdaReservedConcurrency(t *testing.T) {
	src := readFile(t, "ci-cd-permissions/policy_compute_b.tf")
	stmt := regexp.MustCompile(`(?s)Sid\s*=\s*"LambdaReservedConcurrency".*?Resource\s*=\s*"([^"]*)"`).FindStringSubmatch(src)
	if stmt == nil {
		t.Fatal("ci-cd-permissions/policy_compute_b.tf: missing the LambdaReservedConcurrency statement (#718)")
	}
	for _, action := range []string{"lambda:PutFunctionConcurrency", "lambda:DeleteFunctionConcurrency"} {
		if !strings.Contains(stmt[0], `"`+action+`"`) {
			t.Errorf("LambdaReservedConcurrency statement does not grant %s (#718)", action)
		}
	}
	if stmt[1] != "arn:aws:lambda:*:*:function:cudly-*" {
		t.Errorf("LambdaReservedConcurrency Resource = %q, want the cudly-* function scope", stmt[1])
	}
}
