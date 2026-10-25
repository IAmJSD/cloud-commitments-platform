package database

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds database configuration.
type Config struct {
	Host              string
	LogLevel          string
	Database          string
	User              string
	Password          string //nolint:gosec // G101: field holds a user-supplied runtime password, not a hardcoded credential
	PasswordSecret    string
	SSLMode           string
	MigrationsPath    string
	MinConnections    int
	MaxConnLifetime   time.Duration
	MaxConnIdleTime   time.Duration
	HealthCheckPeriod time.Duration
	ConnectTimeout    time.Duration
	MaxConnections    int
	Port              int
	AutoMigrate       bool
}

// LoadFromEnv loads database configuration from environment variables. A set
// but malformed numeric, boolean or duration variable is an error, like an
// out-of-range one: falling back to the default would let a typo such as
// DB_MAX_CONNECTIONS="100 " run the pool at a quarter of its intended size
// with nothing in the logs (#120).
func LoadFromEnv() (*Config, error) {
	env := &envParser{}
	config := &Config{
		// Required fields
		Host:     getEnv("DB_HOST", "localhost"),
		Port:     env.getEnvInt("DB_PORT", 5432),
		Database: getEnv("DB_NAME", "cudly"),
		User:     getEnv("DB_USER", "cudly"),

		// Password (one of these must be set)
		Password:       getEnv("DB_PASSWORD", ""),
		PasswordSecret: getEnv("DB_PASSWORD_SECRET", ""),

		// SSL (default to require for security)
		SSLMode: getEnv("DB_SSL_MODE", "require"),

		// Connection pool defaults
		MaxConnections:    env.getEnvInt("DB_MAX_CONNECTIONS", 25),
		MinConnections:    env.getEnvInt("DB_MIN_CONNECTIONS", 2),
		MaxConnLifetime:   env.getEnvDuration("DB_MAX_CONN_LIFETIME", time.Hour),
		MaxConnIdleTime:   env.getEnvDuration("DB_MAX_CONN_IDLE_TIME", 30*time.Minute),
		HealthCheckPeriod: env.getEnvDuration("DB_HEALTH_CHECK_PERIOD", time.Minute),
		ConnectTimeout:    env.getEnvDuration("DB_CONNECT_TIMEOUT", 10*time.Second),

		// Migrations
		AutoMigrate:    env.getEnvBool("DB_AUTO_MIGRATE", false),
		MigrationsPath: getEnv("DB_MIGRATIONS_PATH", "internal/database/postgres/migrations"),

		// Logging
		LogLevel: getEnv("DB_LOG_LEVEL", "info"),
	}
	if err := errors.Join(env.errs...); err != nil {
		return nil, err
	}

	// Validate configuration
	if err := config.Validate(); err != nil {
		return nil, err
	}

	return config, nil
}

// Validate checks if the configuration is valid.
func (c *Config) Validate() error {
	if err := c.validateRequiredFields(); err != nil {
		return err
	}
	if err := c.validateSSLMode(); err != nil {
		return err
	}
	return c.validatePoolSettings()
}

// validateRequiredFields checks that all required configuration fields are set.
func (c *Config) validateRequiredFields() error {
	if c.Host == "" {
		return fmt.Errorf("DB_HOST is required")
	}
	if c.Database == "" {
		return fmt.Errorf("DB_NAME is required")
	}
	if c.User == "" {
		return fmt.Errorf("DB_USER is required")
	}
	if c.Password == "" && c.PasswordSecret == "" {
		return fmt.Errorf("either DB_PASSWORD or DB_PASSWORD_SECRET must be set")
	}
	// Warn when both are set: plaintext DB_PASSWORD is visible in Lambda/ECS config
	// (readable by lambda:GetFunction) without needing secretsmanager:GetSecretValue.
	// Prefer DB_PASSWORD_SECRET for production deployments.
	if c.Password != "" && c.PasswordSecret != "" {
		fmt.Fprintf(os.Stderr, "WARNING: both DB_PASSWORD and DB_PASSWORD_SECRET are set; "+
			"DB_PASSWORD_SECRET will be used. Remove DB_PASSWORD from environment to avoid "+
			"storing plaintext credentials in Lambda/ECS task configuration.\n")
	}
	return nil
}

// validateSSLMode checks that SSL mode is valid and warns about insecure production settings.
func (c *Config) validateSSLMode() error {
	validSSLModes := map[string]bool{
		"disable":     true,
		"require":     true,
		"verify-ca":   true,
		"verify-full": true,
	}
	if !validSSLModes[c.SSLMode] {
		return fmt.Errorf("invalid DB_SSL_MODE: %s (must be one of: disable, require, verify-ca, verify-full)", c.SSLMode)
	}

	if c.SSLMode == "disable" && os.Getenv("ENVIRONMENT") == "production" {
		fmt.Fprintf(os.Stderr, "WARNING: DB_SSL_MODE=disable should not be used in production\n")
	}

	return nil
}

// validatePoolSettings validates connection pool configuration.
func (c *Config) validatePoolSettings() error {
	if c.MaxConnections < 1 {
		return fmt.Errorf("DB_MAX_CONNECTIONS must be at least 1")
	}
	if c.MinConnections < 0 {
		return fmt.Errorf("DB_MIN_CONNECTIONS cannot be negative")
	}
	if c.MinConnections > c.MaxConnections {
		return fmt.Errorf("DB_MIN_CONNECTIONS cannot be greater than DB_MAX_CONNECTIONS")
	}
	return nil
}

// dsn formats a PostgreSQL connection string with the supplied password. It is
// the single source of the DSN field layout so DSN and RedactedDSN can never
// drift (a field added here applies to both the real and the redacted form).
// Every string value goes through quoteDSNValue, so a space or quote in any
// of them (not only the password) can neither break the DSN nor inject an
// extra keyword such as sslmode=disable.
func (c *Config) dsn(password string) string {
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s connect_timeout=%d",
		quoteDSNValue(c.Host),
		c.Port,
		quoteDSNValue(c.User),
		quoteDSNValue(password),
		quoteDSNValue(c.Database),
		quoteDSNValue(c.SSLMode),
		int(c.ConnectTimeout.Seconds()),
	)
}

// quoteDSNValue quotes a keyword/value DSN value per libpq rules when it is
// empty or contains whitespace, a single quote or a backslash; otherwise it
// returns the value unchanged. It is the only keyword/value encoder; URL
// connection strings are built with net/url (url.UserPassword) instead.
func quoteDSNValue(v string) string {
	if v != "" && !strings.ContainsAny(v, " \t\n\r\f\v'\\") {
		return v
	}
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`)
	return "'" + r.Replace(v) + "'"
}

// DSN generates a PostgreSQL connection string
// If passwordOverride is provided, it's used instead of config.Password.
func (c *Config) DSN(passwordOverride string) string {
	password := c.Password
	if passwordOverride != "" {
		password = passwordOverride
	}
	return c.dsn(password)
}

// RedactedDSN returns a DSN string with the password masked, safe for logging.
func (c *Config) RedactedDSN() string {
	return c.dsn("*****")
}

// Helper functions for environment variable parsing

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// envParser reads typed environment variables. An unset variable yields the
// default; a set one that does not parse also yields the default but records
// an error, so LoadFromEnv can refuse to start and name every bad variable.
type envParser struct {
	errs []error
}

func (p *envParser) invalid(key, value string, err error) {
	p.errs = append(p.errs, fmt.Errorf("invalid %s %q: %w", key, value, err))
}

func (p *envParser) getEnvInt(key string, defaultValue int) int {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	intVal, err := strconv.Atoi(value)
	if err != nil {
		p.invalid(key, value, err)
		return defaultValue
	}
	return intVal
}

func (p *envParser) getEnvBool(key string, defaultValue bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	boolVal, err := strconv.ParseBool(value)
	if err != nil {
		p.invalid(key, value, err)
		return defaultValue
	}
	return boolVal
}

func (p *envParser) getEnvDuration(key string, defaultValue time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		p.invalid(key, value, err)
		return defaultValue
	}
	return duration
}
