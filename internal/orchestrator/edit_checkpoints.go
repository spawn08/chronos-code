package orchestrator

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	maxEditSnapshotBytes = 64 << 20
	maxEditMemoryBytes   = 1 << 20 // Aggregate fallback budget, not per checkpoint.
	maxEditCheckpoints   = 100
	maxEditJournalBytes  = 16 << 10
)

// Compatibility wrappers for callers without an error channel. Tool hooks use
// the error-returning helpers so an uncheckpointable write is blocked.
func (o *Orchestrator) snapshotWrite(input any) {
	if err := o.prepareWrite(input, o.CurrentSessionID()); err != nil {
		slog.Warn("edit checkpoint skipped", "error", err)
	}
}

func (o *Orchestrator) commitWrite(input any) {
	if err := o.finishWrite(input, o.CurrentSessionID()); err != nil {
		slog.Warn("write succeeded but checkpoint could not be committed", "error", err)
	}
}

func (o *Orchestrator) editCheckpointDir(sessionID string) (string, error) {
	if o.cfg == nil {
		return "", nil
	}
	if sessionID == "" {
		return "", fmt.Errorf("edit checkpoint requires a session; start or resume a session before writing")
	}
	paths, err := o.cfg.ResolveProjectPaths("")
	if err != nil {
		return "", fmt.Errorf("resolve edit checkpoint storage: %w", err)
	}
	return filepath.Join(paths.Dir, "edit-checkpoints", fmt.Sprintf("%x", sha256.Sum256([]byte(sessionID)))), nil
}

func (o *Orchestrator) prepareWrite(input any, sessionID string) error {
	path, abs, ok := o.writePath(input)
	if !ok {
		return nil // Let the tool report invalid arguments.
	}
	abs, err := filepath.Abs(abs)
	if err != nil {
		return fmt.Errorf("checkpoint %s: %w", path, err)
	}
	o.editsMu.Lock()
	defer o.editsMu.Unlock()
	for _, cp := range o.edits {
		if cp.Abs == abs && cp.State == "pending" {
			return fmt.Errorf("checkpoint %s: another write is pending; finish it before retrying", path)
		}
	}
	dir, err := o.editCheckpointDir(sessionID)
	if err != nil {
		return err
	}
	// Never evict an in-flight snapshot. Disk-backed committed metadata may be
	// evicted, but its journal and snapshot remain available for lazy recovery.
	if len(o.edits) >= maxEditCheckpoints {
		if dir == "" {
			return fmt.Errorf("in-memory checkpoint limit (%d) reached; undo existing edits before retrying", maxEditCheckpoints)
		}
		evict := -1
		for i := range o.edits {
			if o.edits[i].State == "committed" {
				evict = i
				break
			}
		}
		if evict < 0 {
			return fmt.Errorf("checkpoint limit (%d) reached by pending writes; finish them before retrying", maxEditCheckpoints)
		}
		o.removeEdit(evict)
	}
	cp := fileCheckpoint{Path: path, Abs: abs, SessionID: sessionID, State: "pending",
		ID: fmt.Sprintf("%020d-%s", time.Now().UnixNano(), rand.Text())}
	limit := int64(maxEditSnapshotBytes)
	if dir == "" {
		limit = maxEditMemoryBytes
		for _, old := range o.edits {
			limit -= int64(len(old.Prev))
		}
	} else {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create private checkpoint directory: %w", err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("secure checkpoint directory: %w", err)
		}
		cp.Journal = filepath.Join(dir, cp.ID+".json")
		cp.Snapshot = cp.ID + ".snapshot"
	}
	_, statErr := os.Lstat(abs)
	if statErr != nil && !os.IsNotExist(statErr) {
		return fmt.Errorf("checkpoint %s: cannot inspect original file: %w", path, statErr)
	}
	var src *os.File
	var info os.FileInfo
	if statErr == nil {
		src, info, err = openEditFile(abs)
		if err != nil {
			return fmt.Errorf("checkpoint %s: cannot read original file: %w", path, err)
		}
	}
	if src != nil {
		defer src.Close()
		cp.Existed, cp.PrevMode = true, editMode(info.Mode())
		if info.Size() > limit {
			return fmt.Errorf("checkpoint %s: file exceeds %d-byte available snapshot limit; reduce file size or free in-memory undo history", path, limit)
		}
		var memory bytes.Buffer
		var dst io.Writer = &memory
		var snapshot *os.File
		if dir != "" {
			snapshot, err = os.OpenFile(filepath.Join(dir, cp.Snapshot), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return fmt.Errorf("create snapshot: %w", err)
			}
			dst = snapshot
		}
		cp.PrevHash, err = copyEditBytes(dst, src, limit)
		if snapshot != nil {
			if err == nil {
				err = snapshot.Sync()
			}
			closeErr := snapshot.Close()
			if err == nil {
				err = closeErr
			}
		}
		if err != nil {
			if dir != "" {
				_ = os.Remove(filepath.Join(dir, cp.Snapshot))
			}
			return fmt.Errorf("snapshot %s: %w", path, err)
		}
		if dir == "" {
			// Do not retain bytes.Buffer's spare capacity outside the aggregate
			// fallback budget.
			cp.Prev = make([]byte, memory.Len())
			copy(cp.Prev, memory.Bytes())
		}
		hash, mode, err := editFingerprint(abs)
		if err != nil || hash != cp.PrevHash || mode != cp.PrevMode {
			if dir != "" {
				_ = os.Remove(filepath.Join(dir, cp.Snapshot))
			}
			return fmt.Errorf("checkpoint %s: original changed during snapshot or cannot be verified; retry the write", path)
		}
	}
	if err := persistEdit(cp); err != nil {
		if dir != "" && cp.Existed {
			_ = os.Remove(filepath.Join(dir, cp.Snapshot))
		}
		return fmt.Errorf("checkpoint %s: write blocked; check project data permissions and free space: %w", path, err)
	}
	o.edits = append(o.edits, cp)
	return nil
}

// openEditFile refuses symlinks and non-regular files rather than interpreting
// read errors as a new file. Check identity again after opening to catch swaps.
func openEditFile(path string) (*os.File, os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil || !os.SameFile(before, info) {
		_ = f.Close()
		if err == nil {
			err = fmt.Errorf("file changed while opening %s", path)
		}
		return nil, nil, err
	}
	return f, info, nil
}

func editMode(mode os.FileMode) os.FileMode {
	return mode & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
}

func copyEditBytes(dst io.Writer, src io.Reader, limit int64) (string, error) {
	hash := sha256.New()
	n, err := io.CopyBuffer(io.MultiWriter(dst, hash), io.LimitReader(src, limit+1), make([]byte, 32<<10))
	if err != nil {
		return "", err
	}
	if n > limit {
		return "", fmt.Errorf("file exceeds %d-byte snapshot limit", limit)
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func editFingerprint(path string) (string, os.FileMode, error) {
	f, info, err := openEditFile(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	hash, err := copyEditBytes(io.Discard, f, maxEditSnapshotBytes)
	if err != nil {
		return "", 0, err
	}
	after, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if info.Size() != after.Size() || info.ModTime() != after.ModTime() || info.Mode() != after.Mode() {
		return "", 0, fmt.Errorf("file changed while hashing %s", path)
	}
	current, err := os.Lstat(path)
	if err != nil {
		return "", 0, err
	}
	if !current.Mode().IsRegular() || !os.SameFile(after, current) {
		return "", 0, fmt.Errorf("file replaced while hashing %s", path)
	}
	return hash, editMode(info.Mode()), nil
}

func (o *Orchestrator) pendingEdit(input any, sessionID string) int {
	_, abs, ok := o.writePath(input)
	if !ok {
		return -1
	}
	abs, err := filepath.Abs(abs)
	if err != nil {
		return -1
	}
	for i := len(o.edits) - 1; i >= 0; i-- {
		cp := o.edits[i]
		if cp.Abs == abs && cp.SessionID == sessionID && cp.State == "pending" {
			return i
		}
	}
	return -1
}

func (o *Orchestrator) finishWrite(input any, sessionID string) error {
	o.editsMu.Lock()
	defer o.editsMu.Unlock()
	i := o.pendingEdit(input, sessionID)
	if i < 0 {
		return nil
	}
	cp := o.edits[i]
	var err error
	cp.PostHash, cp.PostMode, err = editFingerprint(cp.Abs)
	if err != nil {
		return fmt.Errorf("write succeeded, but checkpoint %s cannot record its result (snapshot retained): %w", cp.Path, err)
	}
	cp.State = "committed"
	if err := persistEdit(cp); err != nil {
		return fmt.Errorf("write succeeded, but checkpoint journal failed (snapshot retained): %w", err)
	}
	o.edits[i] = cp
	return nil
}

func (o *Orchestrator) discardWrite(input any, sessionID string) {
	o.editsMu.Lock()
	defer o.editsMu.Unlock()
	i := o.pendingEdit(input, sessionID)
	if i < 0 {
		return
	}
	cp := o.edits[i]
	cp.State = "discarded"
	if err := persistEdit(cp); err != nil {
		slog.Warn("discard failed-write checkpoint journal", "error", err)
	} else if cp.Journal != "" && cp.Existed {
		if err := os.Remove(filepath.Join(filepath.Dir(cp.Journal), cp.Snapshot)); err != nil && !os.IsNotExist(err) {
			slog.Warn("remove failed-write snapshot", "error", err)
		}
	}
	o.removeEdit(i)
}

func (o *Orchestrator) removeEdit(i int) {
	copy(o.edits[i:], o.edits[i+1:])
	o.edits[len(o.edits)-1] = fileCheckpoint{}
	o.edits = o.edits[:len(o.edits)-1]
}

// persistEdit atomically replaces a small private journal. Snapshot bytes are
// never encoded into JSON. Pending records are deliberately not auto-recovered:
// after a crash there is no trustworthy post-write fingerprint to compare.
func persistEdit(cp fileCheckpoint) error {
	if cp.Journal == "" {
		return nil
	}
	data, err := json.Marshal(cp)
	if err != nil {
		return fmt.Errorf("encode edit journal: %w", err)
	}
	if len(data) > maxEditJournalBytes {
		return fmt.Errorf("edit journal exceeds %d bytes", maxEditJournalBytes)
	}
	f, err := os.CreateTemp(filepath.Dir(cp.Journal), ".journal-*")
	if err != nil {
		return fmt.Errorf("create edit journal: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), cp.Journal)
	}
	if err != nil {
		return fmt.Errorf("persist edit journal: %w", err)
	}
	return nil
}

// recoverEdits scans in bounded batches and retains only the newest metadata.
// It never prunes disk artifacts, including records outside the cache window.
func (o *Orchestrator) recoverEdits(sessionID string) error {
	dir, err := o.editCheckpointDir(sessionID)
	if err != nil || dir == "" {
		return err
	}
	for i := len(o.edits) - 1; i >= 0; i-- {
		if cp := o.edits[i]; cp.SessionID != sessionID && cp.State != "pending" && cp.Journal != "" {
			o.removeEdit(i)
		}
	}
	d, err := os.Open(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read edit checkpoints: %w", err)
	}
	defer d.Close()
	for {
		entries, readErr := d.ReadDir(32)
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			f, _, err := openEditFile(path)
			if err != nil {
				return fmt.Errorf("read edit journal: %w", err)
			}
			data, err := io.ReadAll(io.LimitReader(f, maxEditJournalBytes+1))
			_ = f.Close()
			if err != nil {
				return fmt.Errorf("read edit journal: %w", err)
			}
			var cp fileCheckpoint
			if len(data) > maxEditJournalBytes || json.Unmarshal(data, &cp) != nil {
				return fmt.Errorf("invalid edit journal %s; repair or move it before retrying undo", path)
			}
			if cp.SessionID != sessionID || cp.State != "committed" {
				continue
			}
			if cp.ID+".json" != entry.Name() || cp.Snapshot != cp.ID+".snapshot" || !filepath.IsAbs(cp.Abs) || cp.PostHash == "" {
				return fmt.Errorf("invalid checkpoint metadata in %s", path)
			}
			cp.Journal = path
			found := false
			for _, old := range o.edits {
				if old.ID == cp.ID && old.SessionID == sessionID {
					found = true
					break
				}
			}
			if found {
				continue
			}
			if len(o.edits) >= maxEditCheckpoints {
				oldest := -1
				for i, old := range o.edits {
					if old.State != "pending" && (oldest < 0 || old.ID < o.edits[oldest].ID) {
						oldest = i
					}
				}
				if oldest < 0 || o.edits[oldest].ID >= cp.ID {
					continue
				}
				o.removeEdit(oldest)
			}
			o.edits = append(o.edits, cp)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("scan edit checkpoints: %w", readErr)
		}
	}
	sort.Slice(o.edits, func(i, j int) bool { return o.edits[i].ID < o.edits[j].ID })
	return nil
}

// UndoLastEdit refuses changed content, changed permissions, missing files and
// symlinks. Conflicts and restore errors leave the checkpoint available.
func (o *Orchestrator) UndoLastEdit() (string, error) {
	sessionID := o.CurrentSessionID()
	o.editsMu.Lock()
	defer o.editsMu.Unlock()
	if err := o.recoverEdits(sessionID); err != nil {
		return "", err
	}
	i := len(o.edits) - 1
	for i >= 0 && o.edits[i].SessionID != sessionID {
		i--
	}
	if i < 0 {
		return "", fmt.Errorf("nothing to undo")
	}
	cp := o.edits[i]
	if cp.State != "committed" {
		return "", fmt.Errorf("checkpoint %s has no confirmed successful write; snapshot retained", cp.Path)
	}
	check := func() error {
		hash, mode, err := editFingerprint(cp.Abs)
		if err != nil {
			return fmt.Errorf("undo conflict for %s; checkpoint retained: %w", cp.Path, err)
		}
		if hash != cp.PostHash || mode != cp.PostMode {
			return fmt.Errorf("undo conflict for %s: file content or permissions changed; checkpoint retained", cp.Path)
		}
		return nil
	}
	if err := check(); err != nil {
		return "", err
	}
	if cp.Existed {
		var src io.Reader = bytes.NewReader(cp.Prev)
		if cp.Journal != "" {
			f, _, err := openEditFile(filepath.Join(filepath.Dir(cp.Journal), cp.Snapshot))
			if err != nil {
				return "", fmt.Errorf("open undo snapshot: %w", err)
			}
			defer f.Close()
			src = f
		}
		tmp, err := os.CreateTemp(filepath.Dir(cp.Abs), ".chronos-undo-*")
		if err != nil {
			return "", fmt.Errorf("prepare undo: %w", err)
		}
		defer os.Remove(tmp.Name())
		hash, err := copyEditBytes(tmp, src, maxEditSnapshotBytes)
		if err == nil && hash != cp.PrevHash {
			err = fmt.Errorf("snapshot integrity mismatch; checkpoint retained")
		}
		if err == nil {
			err = tmp.Chmod(cp.PrevMode)
		}
		if err == nil {
			err = tmp.Sync()
		}
		closeErr := tmp.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return "", fmt.Errorf("restore %s: %w", cp.Path, err)
		}
		// Copying a large snapshot can take time: revalidate immediately before
		// replacement so intervening edits during preparation are preserved.
		if err := check(); err != nil {
			return "", err
		}
		if err := os.Rename(tmp.Name(), cp.Abs); err != nil {
			return "", fmt.Errorf("restore %s: %w", cp.Path, err)
		}
	} else if err := os.Remove(cp.Abs); err != nil {
		return "", fmt.Errorf("undo create %s: %w", cp.Path, err)
	}
	cp.State = "undone"
	if err := persistEdit(cp); err != nil {
		return "", fmt.Errorf("file restored but undo journal update failed: %w", err)
	}
	o.removeEdit(i)
	return cp.Path, nil
}
