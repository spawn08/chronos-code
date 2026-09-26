package retrieve

import "sync"

// Seen records the source ranges one session has already been given, keyed
// by file and content hash, so later retrievals do not repeat them and can
// report files that changed since. Safe for concurrent use.
type Seen struct {
	mu    sync.Mutex
	files map[string]*seenFile
}

type seenFile struct {
	hash   uint64
	ranges [][2]int
}

// maxSeenRanges bounds the ranges kept per file; the oldest are dropped.
const maxSeenRanges = 64

// NewSeen returns empty session state.
func NewSeen() *Seen { return &Seen{files: map[string]*seenFile{}} }

// Add records that lines [start, end] of file at content hash were delivered.
// A new hash replaces what was recorded for the file.
func (s *Seen) Add(file string, hash uint64, start, end int) {
	if hash == 0 || end < start {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.files[file]
	if f == nil || f.hash != hash {
		f = &seenFile{hash: hash}
		s.files[file] = f
	}
	f.ranges = append(f.ranges, [2]int{start, end})
	if len(f.ranges) > maxSeenRanges {
		f.ranges = f.ranges[len(f.ranges)-maxSeenRanges:]
	}
}

// Covered reports whether [start, end] of file at hash was fully delivered.
func (s *Seen) Covered(file string, hash uint64, start, end int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.files[file]
	if f == nil || f.hash != hash {
		return false
	}
	for line := start; line <= end; {
		next := line
		for _, r := range f.ranges {
			if r[0] <= line && r[1] >= line {
				next = max(next, r[1]+1)
			}
		}
		if next == line {
			return false
		}
		line = next
	}
	return true
}

// Changed reports whether file was delivered at a different content hash,
// and forgets the stale ranges so the change is reported once.
func (s *Seen) Changed(file string, hash uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.files[file]
	if f == nil || f.hash == hash {
		return false
	}
	delete(s.files, file)
	return true
}
