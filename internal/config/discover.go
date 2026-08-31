package config

import (
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

// WorkspaceRoot returns the project root: the nearest ancestor of the current
// directory containing a .git directory, or the current directory if none is
// found.
func WorkspaceRoot() string {
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	dir := cwd
	for {
		if info, err := os.Stat(filepath.Join(dir, ".git")); err == nil && info.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return cwd
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
