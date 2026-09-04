package mcpdiscover

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/spawn08/chronos-code/internal/defaults"
	"github.com/spawn08/chronos/engine/mcp"
)

const DocumentationCapability = "documentation"

var ErrDocumentationUnavailable = errors.New("documentation capability unavailable")

// DocumentationPolicy grants access to official documentation sources. Network
// access is disabled unless explicitly enabled, and every source must be
// allowlisted before an MCP connection is attempted.
type DocumentationPolicy struct {
	AllowNetwork    bool
	OfficialDomains []string
}

// DocumentationProvider is the policy-approved default MCP provider for
// official documentation.
type DocumentationProvider struct {
	Server          mcp.ServerConfig
	OfficialDomains []string
}

// Capability identifies this provider for a capability selector.
func (DocumentationProvider) Capability() string {
	return DocumentationCapability
}

// DocumentationEvidence identifies an official documentation result and the
// dependency version it was retrieved for.
type DocumentationEvidence struct {
	Package     string
	Version     string
	URL         string
	RetrievedAt time.Time
	SourceClass string
}

type defaultMCPServers struct {
	Servers []defaultMCPServer `yaml:"servers"`
}

type defaultMCPServer struct {
	Name       string   `yaml:"name"`
	Transport  string   `yaml:"transport"`
	Command    string   `yaml:"command"`
	Args       []string `yaml:"args"`
	Permission string   `yaml:"permission"`
}

// DefaultDocumentationProvider loads the embedded fetch provider only after
// network access has been explicitly allowed. An empty official-domain list is
// intentionally unavailable rather than permitting arbitrary web access.
func DefaultDocumentationProvider(policy DocumentationPolicy) (DocumentationProvider, error) {
	if !policy.AllowNetwork {
		return DocumentationProvider{}, fmt.Errorf("%w: network access is disabled", ErrDocumentationUnavailable)
	}
	domains, err := normalizeDomains(policy.OfficialDomains)
	if err != nil {
		return DocumentationProvider{}, fmt.Errorf("%w: %v", ErrDocumentationUnavailable, err)
	}

	data, err := defaults.ReadFile("mcp-servers.yaml")
	if err != nil {
		return DocumentationProvider{}, fmt.Errorf("read embedded MCP defaults: %w", err)
	}
	var manifest defaultMCPServers
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return DocumentationProvider{}, fmt.Errorf("parse embedded MCP defaults: %w", err)
	}
	for _, server := range manifest.Servers {
		if server.Name != "fetch" {
			continue
		}
		return DocumentationProvider{
			Server: mcp.ServerConfig{
				Name:       server.Name,
				Transport:  mcp.Transport(server.Transport),
				Command:    server.Command,
				Args:       append([]string(nil), server.Args...),
				Permission: server.Permission,
			},
			OfficialDomains: domains,
		}, nil
	}
	return DocumentationProvider{}, fmt.Errorf("%w: embedded fetch provider is not configured", ErrDocumentationUnavailable)
}

// AllowsURL reports whether rawURL is an allowlisted official source. Callers
// must check it before connecting to the documentation provider.
func (p DocumentationProvider) AllowsURL(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	for _, domain := range p.OfficialDomains {
		if host == domain {
			return true
		}
	}
	return false
}

// Evidence returns explicit provenance for an allowlisted official result.
func (p DocumentationProvider) Evidence(pkg, version, sourceURL string, retrievedAt time.Time) (DocumentationEvidence, error) {
	if strings.TrimSpace(pkg) == "" {
		return DocumentationEvidence{}, errors.New("documentation package is required")
	}
	if strings.TrimSpace(version) == "" {
		return DocumentationEvidence{}, errors.New("documentation package version is required")
	}
	if retrievedAt.IsZero() {
		return DocumentationEvidence{}, errors.New("documentation retrieval time is required")
	}
	if !p.AllowsURL(sourceURL) {
		return DocumentationEvidence{}, fmt.Errorf("documentation source %q is not an allowlisted official domain", sourceURL)
	}
	return DocumentationEvidence{
		Package:     pkg,
		Version:     version,
		URL:         sourceURL,
		RetrievedAt: retrievedAt.UTC(),
		SourceClass: "official-documentation",
	}, nil
}

func normalizeDomains(domains []string) ([]string, error) {
	if len(domains) == 0 {
		return nil, errors.New("no official documentation domains are configured")
	}
	seen := make(map[string]struct{}, len(domains))
	normalized := make([]string, 0, len(domains))
	for _, domain := range domains {
		domain = strings.ToLower(strings.TrimSpace(domain))
		if domain == "" || strings.Contains(domain, "/") || strings.Contains(domain, ":") {
			return nil, fmt.Errorf("invalid official documentation domain %q", domain)
		}
		if _, err := url.ParseRequestURI("https://" + domain); err != nil {
			return nil, fmt.Errorf("invalid official documentation domain %q", domain)
		}
		if _, ok := seen[domain]; ok {
			continue
		}
		seen[domain] = struct{}{}
		normalized = append(normalized, domain)
	}
	return normalized, nil
}
