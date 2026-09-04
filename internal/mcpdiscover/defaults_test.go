package mcpdiscover

import (
	"errors"
	"testing"
	"time"

	"github.com/spawn08/chronos/engine/mcp"
)

func TestDefaultDocumentationProvider_ExposesEmbeddedFetchForAllowedNetwork(t *testing.T) {
	provider, err := DefaultDocumentationProvider(DocumentationPolicy{
		AllowNetwork:    true,
		OfficialDomains: []string{"pkg.go.dev"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if provider.Server.Name != "fetch" || provider.Server.Command != "npx" || provider.Server.Transport != mcp.TransportStdio {
		t.Fatalf("unexpected documentation provider: %+v", provider.Server)
	}
	if provider.Capability() != DocumentationCapability {
		t.Fatalf("capability=%q, want %q", provider.Capability(), DocumentationCapability)
	}
	if !provider.AllowsURL("https://pkg.go.dev/net/http") {
		t.Fatal("allowlisted official documentation URL was unavailable")
	}
}

func TestDefaultDocumentationProvider_FailsClosedWithoutNetworkOrDomains(t *testing.T) {
	_, err := DefaultDocumentationProvider(DocumentationPolicy{OfficialDomains: []string{"pkg.go.dev"}})
	if !errors.Is(err, ErrDocumentationUnavailable) {
		t.Fatalf("error=%v, want unavailable documentation capability", err)
	}

	_, err = DefaultDocumentationProvider(DocumentationPolicy{AllowNetwork: true})
	if !errors.Is(err, ErrDocumentationUnavailable) {
		t.Fatalf("error=%v, want unavailable documentation capability", err)
	}
}

func TestDocumentationProvider_RejectsUntrustedDomainsBeforeEvidence(t *testing.T) {
	provider, err := DefaultDocumentationProvider(DocumentationPolicy{
		AllowNetwork:    true,
		OfficialDomains: []string{"pkg.go.dev"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if provider.AllowsURL("https://pkg.go.dev.attacker.example/net/http") {
		t.Fatal("untrusted lookalike domain was allowed")
	}
	if _, err := provider.Evidence("net/http", "v1.26.0", "https://attacker.example/docs", time.Now()); err == nil {
		t.Fatal("untrusted source produced documentation evidence")
	}
}

func TestDocumentationProvider_EvidenceIncludesVersionAndSource(t *testing.T) {
	provider, err := DefaultDocumentationProvider(DocumentationPolicy{
		AllowNetwork:    true,
		OfficialDomains: []string{"pkg.go.dev"},
	})
	if err != nil {
		t.Fatal(err)
	}
	retrievedAt := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.FixedZone("test", 3600))
	evidence, err := provider.Evidence("net/http", "v1.26.0", "https://pkg.go.dev/net/http", retrievedAt)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Package != "net/http" || evidence.Version != "v1.26.0" || evidence.URL != "https://pkg.go.dev/net/http" {
		t.Fatalf("unexpected evidence: %+v", evidence)
	}
	if evidence.RetrievedAt != retrievedAt.UTC() || evidence.SourceClass != "official-documentation" {
		t.Fatalf("missing provenance metadata: %+v", evidence)
	}
}
