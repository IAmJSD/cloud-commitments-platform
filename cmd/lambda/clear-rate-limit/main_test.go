package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestForgotPasswordPatternEscapesLikeMetacharacters pins the pattern the
// DELETE runs with ESCAPE '\': only the email's local part is a wildcard
// (issue #357). The real-Postgres test in main_integration_test.go checks
// which rows the pattern actually deletes.
func TestForgotPasswordPatternEscapesLikeMetacharacters(t *testing.T) {
	cases := map[string]string{
		"example.com":      `EMAIL#%@example.com#ENDPOINT#forgot\_password`,
		"%":                `EMAIL#%@\%#ENDPOINT#forgot\_password`,
		"example_test.com": `EMAIL#%@example\_test.com#ENDPOINT#forgot\_password`,
		`ex\ample.com`:     `EMAIL#%@ex\\ample.com#ENDPOINT#forgot\_password`,
	}
	for domain, want := range cases {
		assert.Equal(t, want, forgotPasswordPattern(domain), "domain %q", domain)
	}
}
