package config

import (
	"fmt"
	"os"
	"path/filepath"
)

const ConfigDirName = ".chronos-code"

func Discover() (projectDir, userDir string, err error) {
	userDir, err = userConfigDir()
	if err != nil {
		return "", "", err
	}
	projectDir = findProjectConfigDir()
	return projectDir, userDir, nil
}

func findProjectConfigDir() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		candidate := filepath.Join(dir, ConfigDirName)
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// WorkspaceRoot returns the canonical project/worktree root of the current
// directory, or the canonical current directory if no .git marker is found.
// Call ResolveProjectPaths when root resolution errors must be reported.
func WorkspaceRoot() string {
	root, err := canonicalProjectRoot("")
	if err != nil {
		return "."
	}
	return root
}

func canonicalProjectRoot(root string) (string, error) {
	if root == "" {
		root = "."
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("absolute project root: %w", err)
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("canonical project root: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("stat project root: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("project root %q is not a directory", abs)
	}
	dir := abs
	for {
		info, err := os.Stat(filepath.Join(dir, ".git"))
		if err == nil && (info.IsDir() || info.Mode().IsRegular()) {
			return dir, nil
		}
		if err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("stat project .git marker: %w", err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return abs, nil
		}
		dir = parent
	}
}

func userConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ConfigDirName), nil
}
