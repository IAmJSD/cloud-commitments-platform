package api

import (
	"fmt"
	"regexp"
	"strings"

	cudlyiac "github.com/LeanerCloud/cloud-commitments-platform/iac"
)

// bundleProvider is one entry of a federation bundle's Terraform
// required_providers block.
type bundleProvider struct {
	Name    string // local name, e.g. "aws"
	Source  string // registry source, e.g. "hashicorp/aws"
	Version string // version constraint exactly as written in main.tf
}

var (
	tfRequiredProvidersOpen = regexp.MustCompile(`^required_providers\s*\{$`)
	tfProviderEntryOpen     = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_-]*)\s*=\s*\{$`)
	tfProviderAttr          = regexp.MustCompile(`^(source|version)\s*=\s*"([^"]*)"$`)
)

type providerScanState int

const (
	scanOutside providerScanState = iota
	scanBlock
	scanEntry
	scanDone
)

// providerScanner reads the required_providers block of a `terraform fmt`
// formatted main.tf line by line. It is deliberately strict: any line it does
// not recognize inside the block is an error, so a layout change in a bundle
// fails loudly instead of silently dropping a provider.
type providerScanner struct {
	state providerScanState
	cur   bundleProvider
	out   []bundleProvider
}

// parseRequiredProviders returns the required_providers entries of a bundle's
// main.tf, in file order.
func parseRequiredProviders(src []byte) ([]bundleProvider, error) {
	s := &providerScanner{}
	for i, line := range strings.Split(string(src), "\n") {
		if err := s.feed(strings.TrimSpace(line)); err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
	}
	if s.state != scanDone {
		return nil, fmt.Errorf("no complete required_providers block found")
	}
	return s.out, nil
}

func (s *providerScanner) feed(line string) error {
	if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
		return nil
	}
	switch s.state {
	case scanOutside:
		if tfRequiredProvidersOpen.MatchString(line) {
			s.state = scanBlock
		}
	case scanBlock:
		return s.feedBlock(line)
	case scanEntry:
		return s.feedEntry(line)
	}
	return nil
}

func (s *providerScanner) feedBlock(line string) error {
	if line == "}" {
		s.state = scanDone
		return nil
	}
	m := tfProviderEntryOpen.FindStringSubmatch(line)
	if m == nil {
		return fmt.Errorf("unexpected line in required_providers: %q", line)
	}
	s.cur = bundleProvider{Name: m[1]}
	s.state = scanEntry
	return nil
}

func (s *providerScanner) feedEntry(line string) error {
	if line == "}" {
		if s.cur.Source == "" || s.cur.Version == "" {
			return fmt.Errorf("provider %q needs both source and version", s.cur.Name)
		}
		s.out = append(s.out, s.cur)
		s.state = scanBlock
		return nil
	}
	m := tfProviderAttr.FindStringSubmatch(line)
	if m == nil {
		return fmt.Errorf("unexpected line in provider %q: %q", s.cur.Name, line)
	}
	if m[1] == "source" {
		s.cur.Source = m[2]
	} else {
		s.cur.Version = m[2]
	}
	return nil
}

// bundleProviders reads and parses the required_providers block that ships
// in the Terraform module of the given federation bundle.
func bundleProviders(target, source string) ([]bundleProvider, error) {
	path := bundleModuleDir(target, source) + "/terraform/main.tf"
	src, err := cudlyiac.Modules.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	providers, err := parseRequiredProviders(src)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return providers, nil
}

// bundleProviderReadmeSection documents the provider version ranges a
// bundle's Terraform module declares, and how a customer moves to a newer
// range. The ranges are read from the embedded main.tf, so the README cannot
// drift from the constraints the customer actually runs.
func bundleProviderReadmeSection(target, source string) (string, error) {
	providers, err := bundleProviders(target, source)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	sb.WriteString("\nProvider versions (from required_providers in terraform/main.tf):\n")
	for _, p := range providers {
		fmt.Fprintf(&sb, "  %-18s %s\n", p.Source, p.Version)
	}
	sb.WriteString("\nThe bundle ships without a .terraform.lock.hcl. The first `terraform init`\n" +
		"picks the newest provider release inside each range and records it in\n" +
		"terraform/.terraform.lock.hcl. Keep that lock file next to your state so\n" +
		"later runs use the same provider versions.\n\n" +
		"When CUDly moves a bundle to a newer provider line, download a fresh bundle,\n" +
		"copy its .tf files over your existing ones (keep your state, backend\n" +
		"configuration and lock file), then run `terraform init -upgrade` and review\n" +
		"`terraform plan` before applying.\n")
	return sb.String(), nil
}
