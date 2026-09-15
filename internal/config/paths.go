package config

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ProjectPaths identifies one canonical checkout/worktree and its persistent
// data locations. Resolving paths never creates directories or migrates data.
type ProjectPaths struct {
	Root        string
	ID          string
	Dir         string
	SessionsDB  string
	GraphDB     string
	TelemetryDB string
	MemoryDB    string
	LegacyDir   string
}

// ResolveProjectPaths finds the nearest .git directory or file above root,
// normalizing symlinks before computing a basename plus 128-bit SHA-256 ID.
// Without a .git marker, root itself is the identity; empty root uses CWD.
// Data lives under CHRONOS_CODE_DATA_HOME/projects/<id>, defaulting to
// ~/.chronos-code/projects/<id>. A relative data home is project-root-relative.
func ResolveProjectPaths(root string) (ProjectPaths, error) {
	root, err := canonicalProjectRoot(root)
	if err != nil {
		return ProjectPaths{}, err
	}
	home := os.Getenv("CHRONOS_CODE_DATA_HOME")
	if home == "" {
		home, err = userConfigDir()
	} else {
		home, err = resolveRootPath(root, home)
	}
	if err != nil {
		return ProjectPaths{}, fmt.Errorf("resolve project data home: %w", err)
	}
	hash := sha256.Sum256([]byte(root))
	name := filepath.Base(root)
	if name == string(filepath.Separator) || name == "." {
		name = "root"
	}
	id := fmt.Sprintf("%s-%x", name, hash[:16])
	dir := filepath.Join(home, "projects", id)
	return ProjectPaths{
		Root: root, ID: id, Dir: dir,
		SessionsDB:  filepath.Join(dir, "sessions.db"),
		GraphDB:     filepath.Join(dir, "graph.db"),
		TelemetryDB: filepath.Join(dir, "telemetry.db"),
		MemoryDB:    filepath.Join(dir, "memory.db"),
		LegacyDir:   filepath.Join(root, ConfigDirName),
	}, nil
}

// ResolveProjectPaths applies workspace.root, defaults.storage.dsn and
// workspace.graph_db without changing c. Relative overrides use the canonical
// project root; a relative workspace.root uses the supplied root (or CWD).
// Embedded legacy DB paths select the new defaults, while explicit YAML values
// (even the same legacy spelling) win. Non-SQLite DSNs, SQLite URIs and :memory:
// are preserved verbatim in SessionsDB for the storage opener.
func (c *Config) ResolveProjectPaths(root string) (ProjectPaths, error) {
	if c.Workspace.Root != "" {
		if root == "" {
			root = "."
		}
		var err error
		root, err = resolveRootPath(root, c.Workspace.Root)
		if err != nil {
			return ProjectPaths{}, fmt.Errorf("resolve workspace root: %w", err)
		}
	}
	paths, err := ResolveProjectPaths(root)
	if err != nil {
		return ProjectPaths{}, err
	}
	if c.Defaults != nil {
		storage := c.Defaults.Storage
		if c.explicitDBPath("defaults.storage.dsn", storage.DSN, "sessions.db") {
			if (storage.Backend != "" && storage.Backend != "sqlite") || storage.DSN == ":memory:" || strings.HasPrefix(storage.DSN, "file:") {
				paths.SessionsDB = storage.DSN
			} else {
				paths.SessionsDB, err = resolveRootPath(paths.Root, storage.DSN)
				if err != nil {
					return ProjectPaths{}, fmt.Errorf("resolve sessions DB: %w", err)
				}
			}
		}
	}
	if c.explicitDBPath("workspace.graph_db", c.Workspace.GraphDB, "graph.db") {
		paths.GraphDB, err = resolveRootPath(paths.Root, c.Workspace.GraphDB)
		if err != nil {
			return ProjectPaths{}, fmt.Errorf("resolve graph DB: %w", err)
		}
	}
	return paths, nil
}

func (c *Config) explicitDBPath(field, value, legacyName string) bool {
	return value != "" && !(c.sources[field] == "embedded" && value == filepath.Join(ConfigDirName, legacyName))
}

func resolveRootPath(root, path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		if path == "~" {
			path = home
		} else {
			path = filepath.Join(home, path[2:])
		}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	return filepath.Clean(path), nil
}
