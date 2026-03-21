package rule

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sydlexius/stillwater/internal/filesystem"
)

// UndoWindowDuration is the time window within which a fix can be reverted.
const UndoWindowDuration = 30 * time.Second

// RevertFunc is a function that reverses a previously applied fix.
// It accepts a context and returns an error if the revert fails.
type RevertFunc func(ctx context.Context) error

// UndoEntry holds the data needed to undo a recently applied fix.
type UndoEntry struct {
	ID          string     // unique undo token (UUID)
	ViolationID string     // the violation that was fixed
	ExpiresAt   time.Time  // when the undo window closes
	Revert      RevertFunc // how to reverse the fix
}

// Expired reports whether the undo window has passed.
func (e *UndoEntry) Expired() bool {
	return time.Now().After(e.ExpiresAt)
}

// UndoStore holds short-lived undo entries for recently applied fixes.
// It is safe for concurrent use.
type UndoStore struct {
	mu      sync.Mutex
	entries map[string]*UndoEntry // keyed by undo ID
}

// NewUndoStore creates an empty UndoStore.
func NewUndoStore() *UndoStore {
	return &UndoStore{
		entries: make(map[string]*UndoEntry),
	}
}

// Register adds an undo entry and returns its ID.
// The entry expires after UndoWindowDuration.
func (s *UndoStore) Register(violationID string, revert RevertFunc) string {
	id := uuid.New().String()
	entry := &UndoEntry{
		ID:          id,
		ViolationID: violationID,
		ExpiresAt:   time.Now().Add(UndoWindowDuration),
		Revert:      revert,
	}
	s.mu.Lock()
	s.entries[id] = entry
	s.mu.Unlock()
	return id
}

// Pop retrieves and removes an undo entry by ID.
// Returns (entry, true) if found and not expired, (nil, false) otherwise.
func (s *UndoStore) Pop(id string) (*UndoEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.entries[id]
	if !ok {
		return nil, false
	}
	delete(s.entries, id)
	if entry.Expired() {
		return nil, false
	}
	return entry, true
}

// Expire removes all entries whose undo window has passed.
// Call periodically to prevent unbounded memory growth.
func (s *UndoStore) Expire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, entry := range s.entries {
		if entry.Expired() {
			delete(s.entries, id)
		}
	}
}

// StartCleanup launches a background goroutine that calls Expire every 60 seconds
// to prevent unbounded memory growth. It returns immediately; the goroutine stops
// when ctx is canceled.
func (s *UndoStore) StartCleanup(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.Expire()
			}
		}
	}()
}

// ForceExpire immediately expires the entry with the given ID.
// This is intended for use in tests only.
func (s *UndoStore) ForceExpire(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.entries[id]; ok {
		entry.ExpiresAt = time.Now().Add(-time.Second)
	}
}

// FileSnapshot holds the pre-fix state of a single file on disk.
// If Exists is false, the file did not exist before the fix was applied.
type FileSnapshot struct {
	Path    string // absolute path to the file
	Exists  bool   // whether the file existed before the fix
	Content []byte // file content captured before the fix (nil if Exists is false)
}

// CaptureFile reads the current content of a file for undo purposes.
// If the file does not exist, returns a snapshot with Exists=false.
// Read errors (permissions, I/O) are returned as errors.
func CaptureFile(path string) (FileSnapshot, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is from trusted artist directory
	if os.IsNotExist(err) {
		return FileSnapshot{Path: path, Exists: false}, nil
	}
	if err != nil {
		return FileSnapshot{}, fmt.Errorf("capturing file for undo: %w", err)
	}
	return FileSnapshot{Path: path, Exists: true, Content: data}, nil
}

// FileRevert returns a RevertFunc that restores a file to its pre-fix state.
// If snap.Exists is false, the file is removed (it was created by the fix).
// If snap.Exists is true, the captured content is written back.
func FileRevert(snap FileSnapshot) RevertFunc {
	return func(_ context.Context) error {
		if !snap.Exists {
			// The fix created this file; remove it to restore the prior state.
			if err := os.Remove(snap.Path); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("removing file created by fix %q: %w", snap.Path, err)
			}
			return nil
		}
		// The fix modified or replaced this file; write the original content back
		// using atomic tmp/bak/rename to prevent corruption on interruption.
		if err := filesystem.WriteFileAtomic(snap.Path, snap.Content, 0o644); err != nil { //nolint:gosec // G306: 0644 is standard for media files
			return fmt.Errorf("restoring file %q: %w", snap.Path, err)
		}
		return nil
	}
}

// MultiFileRevert returns a RevertFunc that reverts multiple file snapshots.
// All reversions are attempted; all errors are aggregated and returned together.
func MultiFileRevert(snaps []FileSnapshot) RevertFunc {
	return func(ctx context.Context) error {
		var errs []error
		for _, snap := range snaps {
			if err := FileRevert(snap)(ctx); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
}

// DirectoryRenameRevert returns a RevertFunc that undoes a directory rename
// by renaming newPath back to oldPath.
func DirectoryRenameRevert(oldPath, newPath string) RevertFunc {
	return func(_ context.Context) error {
		if err := os.Rename(newPath, oldPath); err != nil {
			return fmt.Errorf("reverting directory rename %q -> %q: %w", newPath, oldPath, err)
		}
		return nil
	}
}
