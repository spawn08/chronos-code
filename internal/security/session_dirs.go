package security

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DirectoryAccess is the level of access requested for a directory outside
// the workspace policy roots.
type DirectoryAccess string

const (
	// DirectoryRead allows reading, listing, and searching.
	DirectoryRead DirectoryAccess = "read"
	// DirectoryWrite allows reading, writing, and running shell commands.
	DirectoryWrite DirectoryAccess = "write"
)

// DirectoryApprovalFunc asks a human whether the current session may access
// dir. target is the path the tool call actually requested. It returns false
// when the user declines; an error aborts the tool call.
type DirectoryApprovalFunc func(ctx context.Context, dir, target string, access DirectoryAccess) (bool, error)

type directoryGrant struct {
	dir   string
	write bool
}

// outsideRootsError marks a file or shell call blocked only because its path
// lies outside the configured roots, which a human may approve for the
// session. Denied-path matches never produce this error.
type outsideRootsError struct {
	path     string
	resolved string
	access   DirectoryAccess
	msg      string
}

func (e *outsideRootsError) Error() string { return e.msg }

// SetDirectoryApproval installs the interactive handler consulted when a tool
// call touches a directory outside the configured roots. A nil handler (the
// default, e.g. headless runs) keeps such calls denied.
func (p *Policy) SetDirectoryApproval(fn DirectoryApprovalFunc) {
	p.dirMu.Lock()
	defer p.dirMu.Unlock()
	p.dirApproval = fn
}

func (p *Policy) directoryApproval() DirectoryApprovalFunc {
	p.dirMu.RLock()
	defer p.dirMu.RUnlock()
	return p.dirApproval
}

// AddSessionDirectory grants this process access to dir (and everything
// beneath it) until exit. Denied paths still apply. It returns the canonical
// directory that was granted.
func (p *Policy) AddSessionDirectory(dir string, access DirectoryAccess) (string, error) {
	if strings.HasPrefix(dir, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("security: resolve home directory: %w", err)
		}
		dir = filepath.Join(home, strings.TrimPrefix(dir, "~"))
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("security: resolve %q: %w", dir, err)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("security: resolve %q: %w", dir, err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("security: stat %q: %w", dir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("security: %q is not a directory", dir)
	}
	if tooBroadDirectory(canonical) {
		return "", fmt.Errorf("security: refusing to grant %q (filesystem root or home directory); choose a narrower directory", canonical)
	}
	if matchesAnyGlob(p.DeniedPaths, canonical) {
		return "", fmt.Errorf("security: %q is denied by policy", canonical)
	}
	write := access == DirectoryWrite
	p.dirMu.Lock()
	defer p.dirMu.Unlock()
	for i, grant := range p.sessionDirs {
		if grant.dir == canonical {
			p.sessionDirs[i].write = grant.write || write
			return canonical, nil
		}
	}
	p.sessionDirs = append(p.sessionDirs, directoryGrant{dir: canonical, write: write})
	return canonical, nil
}

// SessionDirectories lists directories granted for this session, formatted
// as "path (read)" or "path (read/write)".
func (p *Policy) SessionDirectories() []string {
	p.dirMu.RLock()
	defer p.dirMu.RUnlock()
	out := make([]string, 0, len(p.sessionDirs))
	for _, grant := range p.sessionDirs {
		level := "read"
		if grant.write {
			level = "read/write"
		}
		out = append(out, grant.dir+" ("+level+")")
	}
	return out
}

// sessionDirectoryAllows reports whether resolved lies in a directory granted
// for this session with at least the requested access.
func (p *Policy) sessionDirectoryAllows(resolved string, access DirectoryAccess) bool {
	p.dirMu.RLock()
	defer p.dirMu.RUnlock()
	for _, grant := range p.sessionDirs {
		if access == DirectoryWrite && !grant.write {
			continue
		}
		if resolved == grant.dir || pathWithin(grant.dir, resolved) {
			return true
		}
	}
	return false
}

// requestDirectory asks the installed handler to grant the directory around
// blocked.resolved. It reports whether access is now allowed.
func (p *Policy) requestDirectory(ctx context.Context, workspace string, blocked *outsideRootsError) (bool, error) {
	approve := p.directoryApproval()
	if approve == nil {
		return false, nil
	}
	dir := directoryCandidate(workspace, blocked.resolved)
	if dir == "" || tooBroadDirectory(dir) {
		return false, nil
	}
	ok, err := approve(ctx, dir, blocked.resolved, blocked.access)
	if err != nil || !ok {
		return false, err
	}
	if _, err := p.AddSessionDirectory(dir, blocked.access); err != nil {
		return false, err
	}
	return p.sessionDirectoryAllows(blocked.resolved, blocked.access), nil
}

// directoryCandidate picks the directory to offer for target: its enclosing
// git repository when that repository does not contain the workspace,
// otherwise the nearest existing directory.
func directoryCandidate(workspace, target string) string {
	dir := target
	for {
		info, err := os.Stat(dir)
		if err == nil && info.IsDir() {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	ws, _ := canonicalWorkspace(workspace)
	for candidate := dir; ; {
		if _, err := os.Stat(filepath.Join(candidate, ".git")); err == nil {
			if ws == "" || (candidate != ws && !pathWithin(candidate, ws)) {
				return candidate
			}
			break
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			break
		}
		candidate = parent
	}
	return dir
}

func tooBroadDirectory(dir string) bool {
	clean := filepath.Clean(dir)
	if clean == filepath.Dir(clean) {
		return true
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if canonicalHome, err := filepath.EvalSymlinks(home); err == nil {
			home = canonicalHome
		}
		if clean == filepath.Clean(home) {
			return true
		}
	}
	return false
}

func isOutsideRoots(err error) (*outsideRootsError, bool) {
	var outside *outsideRootsError
	if errors.As(err, &outside) {
		return outside, true
	}
	return nil, false
}
