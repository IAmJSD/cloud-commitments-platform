package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Postgres keeps microseconds, so a last-change time read back from the
// database is the caller's time truncated. A check stamped with that same
// instant (nanoseconds included) must not count as after the change.
func TestCheckPostdatesDBTime(t *testing.T) {
	change := time.Date(2026, 10, 23, 1, 2, 3, 456789123, time.UTC)
	stored := change.Truncate(time.Microsecond)

	assert.False(t, checkPostdatesDBTime(change, stored), "same instant, extra nanoseconds")
	assert.False(t, checkPostdatesDBTime(stored, stored), "equal")
	assert.False(t, checkPostdatesDBTime(stored.Add(-time.Microsecond), stored), "before")
	assert.True(t, checkPostdatesDBTime(stored.Add(time.Microsecond), stored), "one microsecond after")
	assert.True(t, checkPostdatesDBTime(change.Add(time.Second), stored), "well after")
}

func TestCheckRIExchangeCloudCheck_CheckedAtTheChangeIsRefused(t *testing.T) {
	change := time.Date(2026, 10, 23, 1, 2, 3, 456789123, time.UTC)
	row := riExchangeResolveRow{
		accountID: "123456789012",
		region:    "us-east-1",
		updatedAt: change.Truncate(time.Microsecond),
		dbNow:     change.Add(time.Hour).Truncate(time.Microsecond),
	}
	check := RIExchangeCloudCheck{AccountID: row.accountID, Region: row.region, CheckedAt: change}

	require.ErrorIs(t, checkRIExchangeCloudCheck(check, row), ErrRIExchangeResolveInvalid)

	check.CheckedAt = change.Add(time.Minute)
	require.NoError(t, checkRIExchangeCloudCheck(check, row))
}
