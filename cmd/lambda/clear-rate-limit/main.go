package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database"
	"github.com/aws/aws-lambda-go/lambda"
)

const defaultDomain = "leanercloud.com"

// forgotPasswordEndpoint is the rate-limit endpoint whose per-email rows this
// function clears (see internal/api/rate_limiter.go).
const forgotPasswordEndpoint = "forgot_password"

// likeEscaper escapes the LIKE metacharacters (and the escape character
// itself) so a value matches only itself inside a pattern that is used with
// ESCAPE '\'.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// forgotPasswordPattern is the LIKE pattern for every forgot_password row of
// one email domain. Only the email's local part is a wildcard: the domain and
// endpoint are escaped, so RATE_LIMIT_DOMAIN=% matches a literal "%" domain
// instead of every domain, and an "_" matches only "_" (issue #357).
func forgotPasswordPattern(domain string) string {
	return "EMAIL#%@" + likeEscaper.Replace(domain) + "#ENDPOINT#" + likeEscaper.Replace(forgotPasswordEndpoint)
}

func getDomain() string {
	if domain := os.Getenv("RATE_LIMIT_DOMAIN"); domain != "" {
		return domain
	}
	return defaultDomain
}

// Response is the Lambda function's return value, reporting how many
// forgot_password rate-limit rows were deleted and how many rate-limit
// rows (across all endpoints) remain in the table.
type Response struct {
	Message        string `json:"message"`
	DeletedCount   int    `json:"deleted_count"`
	RemainingCount int    `json:"remaining_count"`
}

func clearRateLimit(ctx context.Context) (Response, error) {
	db, err := database.OpenFromEnv(ctx)
	if err != nil {
		return Response{}, err
	}
	defer db.Close()

	return clearForgotPasswordLimits(ctx, db, getDomain())
}

// clearForgotPasswordLimits deletes the forgot_password rate-limit rows of
// every email at domain and reports how many rate-limit rows remain.
func clearForgotPasswordLimits(ctx context.Context, db *database.Connection, domain string) (Response, error) {
	tag, err := db.Exec(ctx,
		`DELETE FROM rate_limits WHERE id LIKE $1 ESCAPE '\'`,
		forgotPasswordPattern(domain))
	if err != nil {
		return Response{}, fmt.Errorf("failed to delete rate limits: %w", err)
	}

	deletedCount := tag.RowsAffected()

	// Get remaining count
	var remainingCount int64
	err = db.QueryRow(ctx, "SELECT COUNT(*) FROM rate_limits").Scan(&remainingCount)
	if err != nil {
		return Response{}, fmt.Errorf("failed to count remaining rate limits: %w", err)
	}

	log.Printf("Successfully cleared %d rate limit(s), %d remaining", deletedCount, remainingCount)

	return Response{
		Message:        fmt.Sprintf("Successfully cleared %d rate limit(s)", deletedCount),
		DeletedCount:   int(deletedCount),
		RemainingCount: int(remainingCount),
	}, nil
}

func main() {
	lambda.Start(clearRateLimit)
}
