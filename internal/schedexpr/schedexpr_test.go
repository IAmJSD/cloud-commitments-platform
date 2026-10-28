package schedexpr

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func utc(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// Tuesday 2026-10-27 15:04:05 UTC.
var base = utc("2026-10-27T15:04:05Z")

func TestParseRate(t *testing.T) {
	cases := map[string]time.Duration{
		"rate(1 minute)":   time.Minute,
		"rate(5 minutes)":  5 * time.Minute,
		"rate(1 hour)":     time.Hour,
		"rate(12 hours)":   12 * time.Hour,
		"rate(1 day)":      24 * time.Hour,
		"rate(7 days)":     7 * 24 * time.Hour,
		" rate(2 hours) ":  2 * time.Hour,
		"rate(1   minute)": time.Minute,
	}
	for expr, want := range cases {
		t.Run(expr, func(t *testing.T) {
			s, err := Parse(expr)
			require.NoError(t, err)
			r, ok := s.(rateSchedule)
			require.True(t, ok, "got %T", s)
			assert.Equal(t, want, r.period)
			// The rule's creation time anchors a rate schedule and is not
			// visible to the runtime, so the next run is unknown.
			next, ok := s.Next(base)
			assert.False(t, ok)
			assert.True(t, next.IsZero())
		})
	}
}

func TestParseRateInvalid(t *testing.T) {
	for _, expr := range []string{
		"rate(0 days)", "rate(-1 days)", "rate(+2 days)", "rate(1 days)", "rate(2 day)",
		"rate(1 week)", "rate(3 weeks)", "rate(x minutes)", "rate(1)", "rate()",
		"rate(1 day", "rate(1.5 hours)", "Rate(1 day)", "rate(1 day 2)",
	} {
		t.Run(expr, func(t *testing.T) {
			_, err := Parse(expr)
			assert.Error(t, err)
		})
	}
}

func TestParseEmpty(t *testing.T) {
	for _, expr := range []string{"", "   "} {
		_, err := Parse(expr)
		assert.True(t, errors.Is(err, ErrEmpty), "%q: %v", expr, err)
	}
}

func TestNextEventBridgeCron(t *testing.T) {
	cases := []struct {
		expr, from, want string
	}{
		{"cron(0 2 * * ? *)", "2026-10-27T15:04:05Z", "2026-10-28T02:00:00Z"},
		{"cron(0 2 * * ? *)", "2026-10-27T01:59:59Z", "2026-10-27T02:00:00Z"},
		// Strictly after: a call at the fire instant gets the following run.
		{"cron(0 2 * * ? *)", "2026-10-27T02:00:00Z", "2026-10-28T02:00:00Z"},
		{"cron(0/15 * * * ? *)", "2026-10-27T15:04:05Z", "2026-10-27T15:15:00Z"},
		{"cron(5,35 */6 * * ? *)", "2026-10-27T15:04:05Z", "2026-10-27T18:05:00Z"},
		{"cron(0 0 1 * ? *)", "2026-10-27T15:04:05Z", "2026-11-01T00:00:00Z"},
		{"cron(0 0 ? * * *)", "2026-10-27T15:04:05Z", "2026-10-28T00:00:00Z"},
		// Day-of-week 1-7 starts on Sunday: 2 is Monday.
		{"cron(30 6 ? * 2 *)", "2026-10-27T15:04:05Z", "2026-11-02T06:30:00Z"},
		{"cron(0 9 ? * MON-FRI *)", "2026-10-30T10:00:00Z", "2026-11-02T09:00:00Z"},
		{"cron(0 9 ? * sun *)", "2026-10-27T15:04:05Z", "2026-11-01T09:00:00Z"},
		{"cron(0 0 1 JAN ? 2027)", "2026-10-27T15:04:05Z", "2027-01-01T00:00:00Z"},
		{"cron(0 0 29 2 ? *)", "2026-10-27T15:04:05Z", "2028-02-29T00:00:00Z"},
		// Non-UTC input is evaluated in UTC (EventBridge cron is UTC):
		// 03:30+02:00 is 01:30Z, so 02:00Z the same day is next.
		{"cron(0 2 * * ? *)", "2026-10-27T03:30:00+02:00", "2026-10-27T02:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.expr+"@"+tc.from, func(t *testing.T) {
			s, err := Parse(tc.expr)
			require.NoError(t, err)
			next, ok := s.Next(utc(tc.from))
			require.True(t, ok)
			assert.Equal(t, utc(tc.want), next)
			assert.Equal(t, time.UTC, next.Location())
		})
	}
}

func TestNextUnixCron(t *testing.T) {
	cases := []struct {
		expr, from, want string
	}{
		{"0 2 * * *", "2026-10-27T15:04:05Z", "2026-10-28T02:00:00Z"},
		{"*/30 * * * *", "2026-10-27T15:04:05Z", "2026-10-27T15:30:00Z"},
		{"0 9 * * 1-5", "2026-10-30T10:00:00Z", "2026-11-02T09:00:00Z"},
		// Day-of-week 0 and 7 are both Sunday.
		{"0 9 * * 0", "2026-10-27T15:04:05Z", "2026-11-01T09:00:00Z"},
		{"0 9 * * 7", "2026-10-27T15:04:05Z", "2026-11-01T09:00:00Z"},
		{"0 9 ? * MON", "2026-10-27T15:04:05Z", "2026-11-02T09:00:00Z"},
		// Both day fields restricted: either matches (Sunday 1 Nov is
		// the 1st, before Monday 2 Nov).
		{"0 0 1 * 1", "2026-10-27T15:04:05Z", "2026-11-01T00:00:00Z"},
		{"0 0 15 * 1", "2026-10-27T15:04:05Z", "2026-11-02T00:00:00Z"},
		{"0 0 1 * *", "2026-10-27T15:04:05Z", "2026-11-01T00:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.expr+"@"+tc.from, func(t *testing.T) {
			s, err := Parse(tc.expr)
			require.NoError(t, err)
			next, ok := s.Next(utc(tc.from))
			require.True(t, ok)
			assert.Equal(t, utc(tc.want), next)
		})
	}
}

func TestNextNoFireWithinHorizon(t *testing.T) {
	for _, expr := range []string{"cron(0 0 1 1 ? 2020)", "cron(0 0 1 1 ? 2199)", "0 0 31 2 *"} {
		s, err := Parse(expr)
		require.NoError(t, err, expr)
		_, ok := s.Next(base)
		assert.False(t, ok, expr)
	}
}

func TestParseCronInvalid(t *testing.T) {
	for _, expr := range []string{
		// EventBridge
		"cron(0 2 * * * *)",      // neither day field is ?
		"cron(0 2 ? * ? *)",      // both day fields are ?
		"cron(0 2 * * ?)",        // five fields
		"cron(0 2 * * ? * *)",    // seven fields
		"cron(60 2 * * ? *)",     // minute out of range
		"cron(0 24 * * ? *)",     // hour out of range
		"cron(0 0 32 * ? *)",     // day-of-month out of range
		"cron(0 0 ? * 0 *)",      // EventBridge day-of-week starts at 1
		"cron(0 0 ? * 8 *)",      // day-of-week out of range
		"cron(0 0 ? 13 * *)",     // month out of range
		"cron(0 0 * * ? 1969)",   // year out of range
		"cron(0 0 L * ? *)",      // L is not supported
		"cron(0 0 15W * ? *)",    // W is not supported
		"cron(0 12 ? * MON#1 *)", // # is not supported
		"cron(0/0 * * * ? *)",    // zero step
		"cron(5-1 * * * ? *)",    // backwards range
		"cron(0 2 * * ?  )",      // trailing blanks are not a field
		"cron(0 +2 * * ? *)",     // signed value
		"cron(0 1,,2 * * ? *)",   // empty list element
		"cron(? 2 * * ? *)",      // ? outside the day fields
		"cron()",
		// Classic cron
		"0 2 * *",
		"0 2 * * * *",
		"@daily",
		"0 25 * * *",
		"a b c d e",
		"0 0 */2 * 1", // */n with a restricted day-of-week is ambiguous
		"0 0 1 * */2",
		"0 0 * * FRI-MON",
	} {
		t.Run(expr, func(t *testing.T) {
			_, err := Parse(expr)
			assert.Error(t, err)
		})
	}
}
