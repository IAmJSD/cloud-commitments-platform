// Package migrations: transactional-safety invariant test.
//
// How a file reaches PostgreSQL: golang-migrate's pgx/v5 driver (v4.19.1,
// database/pgx/v5/pgx.go Run and runStatement) adds no BEGIN/COMMIT. With
// x-multi-statement unset (no migration DSN in this repo sets it), it reads the
// whole file and passes it to a single ExecContext call with no arguments, and
// pgx sends a call with no arguments over the simple query protocol (pgx v5
// conn.go exec). The whole file therefore arrives as one simple Query message.
//
// Why a file is atomic: PostgreSQL runs a simple Query that holds several
// statements as one implicit transaction, unless the text contains its own
// transaction control (000040 and the 000101 down migration wrap the whole file
// in BEGIN ... COMMIT, which is atomic as well). When a statement fails or the
// session is canceled, the statements before it roll back too. A
// one-statement file is atomic because it is a single statement.
//
// What this test enforces: no migration contains a statement from
// forbiddenPatterns. Most of them (CREATE INDEX CONCURRENTLY, VACUUM, ALTER
// SYSTEM, CREATE/DROP DATABASE, CREATE TABLESPACE, ...) cannot run inside a
// transaction block. In a multi-statement file such a statement fails with
// "cannot run inside a transaction block" and the whole file rolls back. Alone
// in a file it does run, outside any transaction. The test rejects both cases
// anyway: keeping these statements out of the request-time auto-migrate path
// (RunMigrations, called from ensureDB) is a policy, not something PostgreSQL
// guarantees. A statement such as CREATE INDEX CONCURRENTLY that fails or is
// canceled partway can leave work behind (an INVALID index), and that path has
// no safe cancellation or recovery story for it.
//
// Relation to dirty-state recovery: golang-migrate commits
// schema_migrations (version=N, dirty=true) in its own transaction before it
// runs file N, and clears dirty only after the file succeeds (migrate.go
// runMigrations, pgx.go SetVersion). A failed or interrupted file rolls back
// as a whole but leaves dirty=true at N. RunMigrations never clears that flag
// by itself: maybeAutoHealDirty (migrate.go) fails loud, and an operator
// inspects the schema and recovers with CUDLY_FORCE_MIGRATION_VERSION (see
// maybeForceMigrationVersion). Because every file is atomic, N's effects are
// either all present or all absent, so that inspection has two possible
// answers and never a half-applied file.
//
// Opt-out: a migration that genuinely must run outside a transaction may
// declare itself with the exact marker comment
//
//	-- migrate:no-transaction
//
// anywhere in the file (conventionally on one of the first lines). Such files
// are SKIPPED by the forbidden-statement scan, but the test logs a loud warning
// naming them. Only this test reads the marker: golang-migrate ignores it, and
// RunMigrations applies a marked file like any other, on the request-time path
// too. No migration uses it today. Applying one safely needs the one-shot,
// deploy-time migration runner tracked in #31, which does not exist yet, so
// using the marker needs its own reviewed design first.
//
// The scanner strips SQL comments and dollar-quoted bodies ($$...$$, $tag$...$tag$)
// before matching, so CONCURRENTLY mentioned in a comment or inside a
// CREATE FUNCTION / DO body does not trip the check. A function body only runs
// when the function is called later. A DO body runs inside the migration's
// transaction, where PostgreSQL rejects the non-transactional statements
// ("cannot be executed from a function"), so nothing in it can commit
// separately from the rest of the file. Only top-level statements that the
// migration itself executes are scanned.
package migrations

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// noTransactionMarker, when present in a migration file, declares it as an
// intentional, deploy-time-only non-transactional migration (see #31). Only
// this test reads it; the migration runner does not.
const noTransactionMarker = "-- migrate:no-transaction"

// forbiddenPatterns are statements this test keeps out of migrations. Most of
// them cannot run inside a transaction block; REINDEX and REFRESH MATERIALIZED
// VIEW CONCURRENTLY are noted below. Each is matched case-insensitively against
// the comment-and-body-stripped SQL with whitespace tolerance and word
// boundaries.
var forbiddenPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"CREATE INDEX CONCURRENTLY", regexp.MustCompile(`(?is)\bcreate\s+(?:unique\s+)?index\s+concurrently\b`)},
	{"DROP INDEX CONCURRENTLY", regexp.MustCompile(`(?is)\bdrop\s+index\s+concurrently\b`)},
	// Only REINDEX CONCURRENTLY and REINDEX SYSTEM/DATABASE are refused inside
	// a transaction block; a plain REINDEX TABLE/INDEX runs in one. The pattern
	// matches every REINDEX, so all of them stay out of migrations.
	{"REINDEX", regexp.MustCompile(`(?is)\breindex\b`)},
	{"VACUUM", regexp.MustCompile(`(?is)\bvacuum\b`)},
	{"ALTER SYSTEM", regexp.MustCompile(`(?is)\balter\s+system\b`)},
	{"CREATE DATABASE", regexp.MustCompile(`(?is)\bcreate\s+database\b`)},
	{"DROP DATABASE", regexp.MustCompile(`(?is)\bdrop\s+database\b`)},
	{"CREATE TABLESPACE", regexp.MustCompile(`(?is)\bcreate\s+tablespace\b`)},
	// PostgreSQL does allow REFRESH MATERIALIZED VIEW CONCURRENTLY inside a
	// transaction block; this entry keeps a top-level one out of migrations
	// anyway. One inside a function/DO body is stripped before matching, so
	// this only fires at top level.
	{"REFRESH MATERIALIZED VIEW CONCURRENTLY", regexp.MustCompile(`(?is)\brefresh\s+materialized\s+view\s+concurrently\b`)},
}

// lineCommentRe matches a -- comment to end of line.
var lineCommentRe = regexp.MustCompile(`--[^\n]*`)

// blockCommentRe matches /* ... */ comments (non-greedy, across newlines).
var blockCommentRe = regexp.MustCompile(`(?s)/\*.*?\*/`)

// dollarTagRe matches a dollar-quote opening tag, e.g. $$ or $func$.
var dollarTagRe = regexp.MustCompile(`\$[A-Za-z0-9_]*\$`)

// stripDollarBodies removes the contents of dollar-quoted strings (function
// and DO bodies). A function body runs only when called, and a DO body runs
// inside the migration's transaction, where PostgreSQL rejects the
// non-transactional statements, so CONCURRENTLY etc. inside them cannot commit
// separately from the file (see the package comment).
// Matching is tag-aware: a body opened with $tag$ closes only on the same
// $tag$, per PostgreSQL dollar-quoting rules.
func stripDollarBodies(sql string) string {
	var b strings.Builder
	for {
		loc := dollarTagRe.FindStringIndex(sql)
		if loc == nil {
			b.WriteString(sql)
			break
		}
		b.WriteString(sql[:loc[0]])
		tag := sql[loc[0]:loc[1]]
		rest := sql[loc[1]:]
		end := strings.Index(rest, tag)
		if end == -1 {
			// Unterminated dollar quote: keep the tag and the remainder as-is
			// rather than silently dropping it, so a malformed file is still
			// scanned conservatively.
			b.WriteString(tag)
			b.WriteString(rest)
			break
		}
		// Drop the body (rest[:end]) and the closing tag; continue after it.
		sql = rest[end+len(tag):]
	}
	return b.String()
}

// sanitizeSQL removes comments and dollar-quoted bodies so the forbidden-pattern
// scan only sees top-level statements the migration actually executes.
func sanitizeSQL(sql string) string {
	sql = blockCommentRe.ReplaceAllString(sql, " ")
	sql = lineCommentRe.ReplaceAllString(sql, " ")
	sql = stripDollarBodies(sql)
	return sql
}

func TestMigrationsAreTransactional(t *testing.T) {
	files, err := filepath.Glob("*.sql")
	if err != nil {
		t.Fatalf("globbing migration files: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no *.sql migration files found; test is in the wrong directory or migrations vanished")
	}

	var scanned, skipped int
	for _, f := range files {
		name := filepath.Base(f)
		if !strings.HasSuffix(name, ".up.sql") && !strings.HasSuffix(name, ".down.sql") {
			continue
		}

		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		content := string(raw)

		if strings.Contains(content, noTransactionMarker) {
			skipped++
			t.Logf("WARNING: %s is declared non-transactional via %q and is SKIPPED by the "+
				"transactional-safety scan. It MUST be applied via the one-shot deploy-time "+
				"migration runner (#31, not built yet), never the in-request auto-migrate path; "+
				"RunMigrations does not read the marker and would apply it like any other file.",
				name, noTransactionMarker)
			continue
		}

		scanned++
		sanitized := sanitizeSQL(content)
		for _, p := range forbiddenPatterns {
			if p.re.MatchString(sanitized) {
				t.Errorf("%s contains a statement kept out of migrations (%s). PostgreSQL runs a "+
					"multi-statement migration file as one implicit transaction, where most of these "+
					"statements fail; alone in a file they run outside any transaction, which the "+
					"request-time auto-migrate path cannot safely cancel or recover. Rewrite it "+
					"transactionally, or mark the file with %q and apply it with a deploy-time "+
					"runner (see #31).",
					name, p.name, noTransactionMarker)
			}
		}
	}

	t.Logf("transactional-safety scan: %d migration files scanned, %d skipped (opt-out)", scanned, skipped)
}
