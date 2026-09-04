package mcpdiscover

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/spawn08/chronos/engine/mcp"
)

const (
	ScopeProject = "project"
	ScopeUser    = "user"
)

// CanonicalPath returns the MCP file managed by the CLI for a scope.
func CanonicalPath(root, home, scope string) (string, error) {
	switch scope {
	case "", ScopeProject:
		return filepath.Join(root, ".mcp.json"), nil
	case ScopeUser:
		return filepath.Join(home, ".chronos-code", "mcp.json"), nil
	default:
		return "", fmt.Errorf("invalid MCP scope %q (supported: project, user)", scope)
	}
}

// ManagedServer is a strictly validated entry in a canonical MCP config.
type ManagedServer struct {
	Name       string
	Transport  mcp.Transport
	Command    string
	Args       []string
	URL        string
	Permission string
}

// ValidateManagedServer rejects unsupported transports and ambiguous shapes.
func ValidateManagedServer(server ManagedServer) error {
	if err := validateManagedServerShape(server); err != nil {
		return err
	}
	return validateSecretReferences(server)
}

func validateManagedServerShape(server ManagedServer) error {
	if server.Name == "" || strings.TrimSpace(server.Name) != server.Name {
		return fmt.Errorf("MCP server name must be non-empty and have no surrounding whitespace")
	}
	for _, r := range server.Name {
		if unicode.IsControl(r) {
			return fmt.Errorf("MCP server name must not contain control characters")
		}
	}

	switch server.Transport {
	case mcp.TransportStdio:
		if strings.TrimSpace(server.Command) == "" {
			return fmt.Errorf("MCP server %q: command is required for stdio transport", server.Name)
		}
		if server.URL != "" {
			return fmt.Errorf("MCP server %q: stdio transport cannot include url", server.Name)
		}
	case mcp.TransportSSE:
		if server.Command != "" || len(server.Args) != 0 {
			return fmt.Errorf("MCP server %q: sse transport cannot include command or args", server.Name)
		}
		parsed, err := url.Parse(server.URL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
			return fmt.Errorf("MCP server %q: sse url must be an absolute HTTPS URL without userinfo or fragment", server.Name)
		}
	default:
		return fmt.Errorf("MCP server %q: unsupported transport %q (supported: stdio, sse; HTTP is not supported)", server.Name, server.Transport)
	}

	if server.Permission == "" {
		server.Permission = "require_approval"
	}
	if server.Permission != "require_approval" && server.Permission != "allow" && server.Permission != "deny" {
		return fmt.Errorf("MCP server %q: invalid permission %q", server.Name, server.Permission)
	}
	return nil
}

// ListManaged reads and strictly validates one canonical MCP config.
func ListManaged(path string) ([]ManagedServer, error) {
	_, servers, err := readDocument(path)
	if err != nil {
		return nil, err
	}
	return decodeManagedServers(path, servers)
}

func decodeManagedServers(path string, servers map[string]json.RawMessage) ([]ManagedServer, error) {
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]ManagedServer, 0, len(names))
	for _, name := range names {
		server, err := decodeManagedServer(name, servers[name])
		if err != nil {
			return nil, fmt.Errorf("parse MCP config %s: %w", path, err)
		}
		out = append(out, server)
	}
	return out, nil
}

// AddManaged adds a server without replacing an existing entry.
func AddManaged(path string, server ManagedServer, userScope bool) error {
	if server.Permission == "" {
		server.Permission = "require_approval"
	}
	if err := ValidateManagedServer(server); err != nil {
		return err
	}
	doc, servers, err := readDocument(path)
	if err != nil {
		return err
	}
	if _, err := decodeManagedServers(path, servers); err != nil {
		return err
	}
	if _, exists := servers[server.Name]; exists {
		return fmt.Errorf("MCP server %q already exists", server.Name)
	}
	entry := map[string]any{
		"transport":  server.Transport,
		"permission": server.Permission,
	}
	if server.Transport == mcp.TransportStdio {
		entry["command"] = server.Command
		if len(server.Args) > 0 {
			entry["args"] = server.Args
		}
	} else {
		entry["url"] = server.URL
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode MCP server %q: %w", server.Name, err)
	}
	servers[server.Name] = raw
	return writeDocument(path, doc, servers, userScope)
}

// RemoveManaged removes a server and fails if it does not exist.
func RemoveManaged(path, name string, userScope bool) error {
	doc, servers, err := readDocument(path)
	if err != nil {
		return err
	}
	if _, err := decodeManagedServers(path, servers); err != nil {
		return err
	}
	if _, exists := servers[name]; !exists {
		return fmt.Errorf("MCP server %q does not exist", name)
	}
	delete(servers, name)
	return writeDocument(path, doc, servers, userScope)
}

// RedactedEndpoint returns a safe command or URL summary for terminal output.
func RedactedEndpoint(server ManagedServer) string {
	if server.Transport == mcp.TransportSSE {
		parsed, err := url.Parse(server.URL)
		if err != nil {
			return "<invalid URL>"
		}
		query := parsed.Query()
		for key := range query {
			if credentialLike(key) {
				query.Set(key, "<redacted>")
			}
		}
		parsed.RawQuery = query.Encode()
		return parsed.String()
	}

	parts := append([]string{server.Command}, server.Args...)
	for i := 1; i < len(parts); i++ {
		key := strings.TrimLeft(parts[i], "-")
		if before, _, ok := strings.Cut(key, "="); ok && credentialLike(before) {
			parts[i] = parts[i][:strings.Index(parts[i], "=")+1] + "<redacted>"
			continue
		}
		if credentialLike(key) && i+1 < len(parts) {
			parts[i+1] = "<redacted>"
			i++
		}
	}
	return strings.Join(parts, " ")
}

func readDocument(path string) (map[string]json.RawMessage, map[string]json.RawMessage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]json.RawMessage), make(map[string]json.RawMessage), nil
		}
		return nil, nil, fmt.Errorf("read MCP config %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, nil, fmt.Errorf("parse MCP config %s: empty document", path)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, nil, fmt.Errorf("parse MCP config %s: expected JSON object: %w", path, err)
	}
	if doc == nil {
		return nil, nil, fmt.Errorf("parse MCP config %s: expected JSON object", path)
	}
	servers := make(map[string]json.RawMessage)
	if raw, ok := doc["mcpServers"]; ok {
		if err := json.Unmarshal(raw, &servers); err != nil || servers == nil {
			return nil, nil, fmt.Errorf("parse MCP config %s: mcpServers must be an object", path)
		}
	}
	return doc, servers, nil
}

func decodeManagedServer(name string, raw json.RawMessage) (ManagedServer, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return ManagedServer{}, fmt.Errorf("MCP server %q must be an object", name)
	}
	var entry serverEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return ManagedServer{}, fmt.Errorf("MCP server %q has invalid field shape: %w", name, err)
	}
	for _, field := range []string{"command", "args", "type", "transport", "url", "permission"} {
		if value, ok := fields[field]; ok && string(value) == "null" {
			return ManagedServer{}, fmt.Errorf("MCP server %q field %q must not be null", name, field)
		}
	}
	permission := "require_approval"
	if value, ok := fields["permission"]; ok {
		if err := json.Unmarshal(value, &permission); err != nil {
			return ManagedServer{}, fmt.Errorf("MCP server %q permission must be a string", name)
		}
	}
	if entry.Transport != "" && entry.Type != "" && entry.Transport != entry.Type {
		return ManagedServer{}, fmt.Errorf("MCP server %q has conflicting type and transport", name)
	}
	server := ManagedServer{
		Name:       name,
		Transport:  mcp.Transport(entry.Transport),
		Command:    entry.Command,
		Args:       entry.Args,
		URL:        entry.URL,
		Permission: permission,
	}
	if server.Transport == "" {
		server.Transport = mcp.Transport(entry.Type)
	}
	if server.Transport == "" {
		if server.URL != "" {
			server.Transport = mcp.TransportSSE
		} else if server.Command != "" {
			server.Transport = mcp.TransportStdio
		}
	}
	if err := validateManagedServerShape(server); err != nil {
		return ManagedServer{}, err
	}
	return server, nil
}

func writeDocument(path string, doc, servers map[string]json.RawMessage, userScope bool) error {
	serverData, err := json.Marshal(servers)
	if err != nil {
		return fmt.Errorf("encode MCP servers: %w", err)
	}
	doc["mcpServers"] = serverData
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode MCP config: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	dirMode := os.FileMode(0o755)
	if userScope {
		dirMode = 0o700
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("create MCP config directory: %w", err)
	}
	if userScope {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("secure MCP config directory: %w", err)
		}
	}
	mode := os.FileMode(0o600)
	if !userScope {
		if info, statErr := os.Stat(path); statErr == nil {
			mode = info.Mode().Perm()
		} else if !os.IsNotExist(statErr) {
			return fmt.Errorf("stat MCP config %s: %w", path, statErr)
		}
	}
	tmp, err := os.CreateTemp(dir, ".mcp.json.tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary MCP config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil {
		return fmt.Errorf("write temporary MCP config: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("close temporary MCP config: %w", closeErr)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace MCP config: %w", err)
	}
	return nil
}

func validateSecretReferences(server ManagedServer) error {
	for i, arg := range server.Args {
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		key := strings.TrimLeft(arg, "-")
		if before, value, ok := strings.Cut(key, "="); ok && credentialLike(before) {
			if !secretReference(value) {
				return fmt.Errorf("MCP server %q: credential-like argument %q must use an environment reference such as ${TOKEN}", server.Name, before)
			}
			continue
		}
		if credentialLike(key) {
			if i+1 >= len(server.Args) || !secretReference(server.Args[i+1]) {
				return fmt.Errorf("MCP server %q: credential-like argument %q must use an environment reference such as ${TOKEN}", server.Name, key)
			}
		}
	}
	if server.Transport == mcp.TransportSSE {
		parsed, _ := url.Parse(server.URL)
		for key, values := range parsed.Query() {
			if credentialLike(key) {
				for _, value := range values {
					if !secretReference(value) {
						return fmt.Errorf("MCP server %q: credential-like URL query %q must use an environment reference such as ${TOKEN}", server.Name, key)
					}
				}
			}
		}
	}
	return nil
}

func credentialLike(value string) bool {
	value = strings.ToLower(strings.ReplaceAll(value, "-", "_"))
	for _, part := range []string{"token", "secret", "password", "passwd", "api_key", "apikey", "authorization", "credential"} {
		if strings.Contains(value, part) {
			return true
		}
	}
	return false
}

func secretReference(value string) bool {
	return strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}") && len(value) > 3
}
