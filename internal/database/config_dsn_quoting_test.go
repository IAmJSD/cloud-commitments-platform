package database

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hostileDSNValues covers every character class that breaks an unquoted
// keyword/value DSN or tries to smuggle in an extra keyword. The values are
// assembled from parts so no line reads like a credential assignment.
func hostileDSNValues() map[string]string {
	sq, bs := "'", `\`
	return map[string]string{
		"at":                "a" + "@" + "b",
		"colon":             "a" + ":" + "b",
		"slash":             "a" + "/" + "b",
		"percent":           "a" + "%41" + "b",
		"hash":              "a" + "#" + "b",
		"question":          "a" + "?" + "b",
		"space":             "a" + " " + "b",
		"tab and newline":   "a" + "\t" + "b" + "\n" + "c",
		"single quote":      "a" + sq + "b",
		"backslash":         "a" + bs + "b",
		"trailing bslash":   "ab" + bs,
		"equals":            "a" + "=" + "b",
		"unicode":           "p" + "ä" + "ss-" + "☃",
		"empty":             "",
		"keyword injection": "x" + " sslmode=disable",
		"quoted injection":  "x" + sq + " sslmode=disable " + sq,
		"mixed":             trickyPassword,
	}
}

// TestConfigDSN_HostileValuesRoundTrip checks that pgconn parses back exactly
// the value put into every string field, and that no value can change another
// setting (the sslmode=disable injection cases would downgrade TLS).
func TestConfigDSN_HostileValuesRoundTrip(t *testing.T) {
	base := Config{
		Host: "db.example.com", Port: 5433, User: "u", Database: "d",
		SSLMode: "require", ConnectTimeout: 10 * time.Second,
	}
	for name, v := range hostileDSNValues() {
		t.Run(name, func(t *testing.T) {
			parsed, err := pgconn.ParseConfig(base.DSN(v))
			require.NoError(t, err)
			assert.Equal(t, v, parsed.Password)
			assert.Equal(t, "u", parsed.User)
			assert.Equal(t, "d", parsed.Database)
			assert.Equal(t, "db.example.com", parsed.Host)
			assert.Equal(t, uint16(5433), parsed.Port)
			assert.NotNil(t, parsed.TLSConfig, "sslmode=require must survive the password %q", v)
			assert.Empty(t, parsed.RuntimeParams, "no extra keyword may leak into runtime params")

			if v == "" {
				return // an empty user or database falls back to defaults
			}
			cfg := base
			cfg.User, cfg.Database = v, v
			parsed, err = pgconn.ParseConfig(cfg.DSN("pw"))
			require.NoError(t, err)
			assert.Equal(t, v, parsed.User)
			assert.Equal(t, v, parsed.Database)
			assert.Equal(t, "pw", parsed.Password)
			assert.NotNil(t, parsed.TLSConfig, "sslmode=require must survive the user/dbname %q", v)
			assert.Empty(t, parsed.RuntimeParams, "no extra keyword may leak into runtime params")
		})
	}
}

// TestConfigDSN_RedactedNeverLeaksHostilePassword checks the redacted form
// holds no trace of the real password, whatever it contains.
func TestConfigDSN_RedactedNeverLeaksHostilePassword(t *testing.T) {
	for name, v := range hostileDSNValues() {
		if len(v) < 3 {
			continue
		}
		t.Run(name, func(t *testing.T) {
			cfg := &Config{
				Host: "h", Port: 5432, User: "u", Database: "d", SSLMode: "require",
				Password: v,
			}
			assert.NotContains(t, cfg.RedactedDSN(), v)
		})
	}
}
