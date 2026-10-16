package incctx

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
)

// ripgrep runs recursive searches with rg when it is installed: on large
// repositories it is several times faster than the Go walker and applies
// the same .gitignore semantics as git. The Go walker remains the fallback
// and the implementation for single files.
type ripgrep struct {
	once sync.Once
	path string
}

// available returns the rg executable, or "" when it is not on PATH.
func (r *ripgrep) available() string {
	r.once.Do(func() { r.path, _ = exec.LookPath("rg") })
	return r.path
}

// rgEcosystemGlobs excludes the ecosystem directories fsignore skips, so rg
// and the Go walker return the same files when a directory is not ignored.
// The search path itself is never excluded: a search below vendor/ or
// node_modules/ that the caller asked for still runs, as in the Go walker,
// because rg applies --glob only to paths below the search root.
func rgEcosystemGlobs() []string {
	names := fsignoreDirNames()
	args := make([]string, 0, 2*len(names))
	for _, name := range names {
		args = append(args, "--glob", "!**/"+name+"/**")
	}
	return args
}

// errRipgrepUnusable means rg could not run; the caller falls back to Go.
var errRipgrepUnusable = errors.New("ripgrep unusable")

// search runs rg over dir and fills s with the Go walker's result shape and
// bounds. Unlike the walker it searches over-long lines completely, so such a
// file does not make the result partial.
func (r *ripgrep) search(ctx context.Context, s *grepSearch, dir, pattern string, regex, caseInsensitive bool) error {
	bin := r.available()
	if bin == "" {
		return errRipgrepUnusable
	}
	// --hidden and --no-require-git match the Go walker: dotfiles such as
	// .github/ are searched, and .gitignore applies outside a repository.
	args := []string{"--json", "--no-config", "--no-messages", "--hidden", "--no-require-git",
		"--max-filesize", "8M"}
	if !regex {
		args = append(args, "--fixed-strings")
	}
	if caseInsensitive {
		args = append(args, "--ignore-case")
	}
	if s.glob != "" {
		args = append(args, "--glob", s.glob)
	}
	args = append(args, rgEcosystemGlobs()...)
	// rg runs in dir and searches ".", so --glob patterns match paths
	// relative to the search root, as the Go walker's do. With an absolute
	// search path they matched absolute paths: "src/**" found nothing, and a
	// workspace under any directory named like an ecosystem dir (~/.cache,
	// .../target/...) matched every exclusion.
	args = append(args, "--regexp", pattern, "--", ".")

	rgCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(rgCtx, bin, args...)
	cmd.Dir = dir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return errRipgrepUnusable
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return errRipgrepUnusable
	}
	sc := bufio.NewScanner(stdout)
	// JSON-escaped lines can be several times their source length; a longer
	// event ends the scan and the search is reported truncated below.
	sc.Buffer(make([]byte, 0, 64<<10), 8*maxLineBytes)
	binary := map[string]bool{}
	for sc.Scan() && !s.stopped {
		var ev rgEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Type != "match" {
			continue
		}
		path := ev.Data.Path.Text
		if path == "" {
			continue // non-UTF-8 path, reported as bytes
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		if ev.Data.Lines.Text == "" && ev.Data.Lines.Bytes != "" {
			// Invalid UTF-8: the Go walker treats the file as binary.
			binary[path] = true
			continue
		}
		if binary[path] || ignoredByMatcher(s, path) {
			continue
		}
		line := bytes.TrimRight([]byte(ev.Data.Lines.Text), "\r\n")
		if len(line) > maxLineBytes {
			s.truncated = true // as the Go walker: an over-long line makes the search partial
			continue
		}
		// errStopScan only ends this file's lines, which rg's per-line
		// events already bound; s.stopped (checked below) ends the search.
		_ = s.addMatch(path, ev.Data.LineNumber, line)
		if s.stopped {
			break
		}
	}
	if s.stopped {
		cancel()
	}
	scanErr := sc.Err()
	if scanErr != nil {
		s.truncated = true
		cancel()
		_, _ = io.Copy(io.Discard, stdout)
	}
	waitErr := cmd.Wait()
	s.dropBinary(binary)
	// rg searches in parallel; sort so identical searches return identical
	// bytes (stable tool results keep the prompt cache valid).
	s.sortResults()
	// Only the caller's cancellation is an error; our own cancel after an
	// early stop or an oversized event is not.
	if err := ctx.Err(); err != nil {
		return err
	}
	var exit *exec.ExitError
	switch {
	case s.stopped, waitErr == nil, scanErr != nil:
		return nil
	case errors.As(waitErr, &exit) && exit.ExitCode() == 1:
		return nil // no matches
	case errors.As(waitErr, &exit) && exit.ExitCode() == 2 && len(s.files)+len(s.matches) > 0:
		s.truncated = true // some files unreadable; results are partial
		return nil
	default:
		return fmt.Errorf("%w: %v: %s", errRipgrepUnusable, waitErr, bytes.TrimSpace(stderr.Bytes()))
	}
}

type rgEvent struct {
	Type string `json:"type"`
	Data struct {
		Path  struct{ Text string } `json:"path"`
		Lines struct {
			Text  string `json:"text"`
			Bytes string `json:"bytes"`
		} `json:"lines"`
		LineNumber int `json:"line_number"`
	} `json:"data"`
}

// dropBinary removes results from files later identified as binary.
func (s *grepSearch) dropBinary(binary map[string]bool) {
	if len(binary) == 0 {
		return
	}
	files := s.files[:0]
	for _, f := range s.files {
		if !binary[f] {
			files = append(files, f)
		} else {
			delete(s.counts, f)
		}
	}
	s.files = files
	matches := s.matches[:0]
	for _, m := range s.matches {
		if f, _ := m["file"].(string); !binary[f] {
			matches = append(matches, m)
		}
	}
	s.matches = matches
}

// sortResults orders files and matches by path, then line.
func (s *grepSearch) sortResults() {
	sort.Strings(s.files)
	sort.SliceStable(s.matches, func(i, j int) bool {
		fi, _ := s.matches[i]["file"].(string)
		fj, _ := s.matches[j]["file"].(string)
		if fi != fj {
			return fi < fj
		}
		li, _ := s.matches[i]["line_number"].(int)
		lj, _ := s.matches[j]["line_number"].(int)
		return li < lj
	})
}
