// Package archera wires the explicit Archera comparison (pkg/insurance) into
// the server. The surface is off unless all three settings are present, and
// the API key is resolved lazily on the first explicit request.
package archera

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/insurance"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/secrets"
)

// Setting names, shared with the CLI and MCP consumers.
const (
	EnvOrgID     = "ARCHERA_ORG_ID"
	EnvPlanID    = "ARCHERA_PLAN_ID"
	EnvKeySecret = "ARCHERA_API_KEY_SECRET" // #nosec G101 -- setting name, not a credential value
)

// ErrNotConfigured is returned (wrapped, naming the missing settings) when an
// explicit request arrives and one or more settings are absent.
var ErrNotConfigured = errors.New("archera comparison is not configured")

// ErrKeyUnavailable is returned when the API key secret cannot be resolved or
// yields an unusable client. The message is fixed so no vendor or secret text
// can reach a response.
var ErrKeyUnavailable = errors.New("archera API key could not be resolved from " + EnvKeySecret)

// Settings are the non-secret Archera settings. KeySecretRef is a reference to
// the secret (name, ARN or URI), not the key itself.
type Settings struct {
	OrgID        string
	PlanID       string
	KeySecretRef string
}

// SettingsFromEnv reads the settings from the process environment.
func SettingsFromEnv() Settings {
	return Settings{
		OrgID:        os.Getenv(EnvOrgID),
		PlanID:       os.Getenv(EnvPlanID),
		KeySecretRef: os.Getenv(EnvKeySecret),
	}
}

// Missing lists the names (never values) of absent settings, in a fixed order.
func (s Settings) Missing() []string {
	missing := []string{}
	if s.KeySecretRef == "" {
		missing = append(missing, EnvKeySecret)
	}
	if s.OrgID == "" {
		missing = append(missing, EnvOrgID)
	}
	if s.PlanID == "" {
		missing = append(missing, EnvPlanID)
	}
	return missing
}

// Status reports whether the three settings are present. It resolves nothing
// and never carries a value.
type Status struct {
	Missing    []string `json:"missing"`
	Configured bool     `json:"configured"`
}

// Status returns the presence-only status for the settings.
func (s Settings) Status() Status {
	m := s.Missing()
	return Status{Configured: len(m) == 0, Missing: m}
}

// Provider builds the insurance client on first use. It holds the secret
// resolver and a *insurance.Client only, never the raw key or a Config value.
type Provider struct {
	resolver secrets.Resolver
	hc       *http.Client // nil in production (SSRF-hardened default); tests inject
	// client is an atomic pointer so fmt prints an address, never the Client's
	// fields: fmt dereferences a plain *Client held in an unexported field for
	// verbs such as %s, which would print the key.
	client   atomic.Pointer[insurance.Client]
	settings Settings
	mu       sync.Mutex
}

// NewProvider returns a Provider. hc must be nil in production.
func NewProvider(settings Settings, resolver secrets.Resolver, hc *http.Client) *Provider {
	return &Provider{settings: settings, resolver: resolver, hc: hc}
}

// Status reports setting presence without any outbound call or secret read.
func (p *Provider) Status() Status {
	if p == nil {
		return Settings{}.Status()
	}
	return p.settings.Status()
}

// Format keeps the provider's contents out of every fmt verb.
func (p *Provider) Format(s fmt.State, _ rune) {
	_, _ = fmt.Fprint(s, "archera.Provider{}")
}

// PlanID is the configured Archera plan ID.
func (p *Provider) PlanID() string { return p.settings.PlanID }

// Client returns the lazily built client. A failure is not cached: the next
// call retries (a mutex, not sync.Once, guards the build).
func (p *Provider) Client(ctx context.Context) (insurance.QuoteClient, error) {
	if p == nil {
		return nil, fmt.Errorf("%w: missing %v", ErrNotConfigured, Settings{}.Missing())
	}
	if missing := p.settings.Missing(); len(missing) > 0 {
		return nil, fmt.Errorf("%w: missing %v", ErrNotConfigured, missing)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if c := p.client.Load(); c != nil {
		return c, nil
	}
	if p.resolver == nil {
		return nil, ErrKeyUnavailable
	}
	key, err := p.resolver.GetSecret(ctx, p.settings.KeySecretRef)
	if err != nil {
		logging.Warnf("archera: resolving %s failed: %v", EnvKeySecret, err)
		return nil, ErrKeyUnavailable
	}
	c, err := insurance.NewClient(insurance.Config{APIKey: insurance.NewSecret(strings.TrimSpace(key)), OrgID: p.settings.OrgID}, p.hc)
	if err != nil {
		logging.Warnf("archera: building client failed: %v", err)
		return nil, ErrKeyUnavailable
	}
	p.client.Store(c)
	return c, nil
}
