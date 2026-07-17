package image

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sydlexius/stillwater/internal/filesystem"
)

// RepairDirName is the durable quarantine root for the cross-artist backdrop
// back-out (#2564). Bytes land here BEFORE anything is removed, so a false
// positive is recoverable.
//
// It mirrors BackupDirName's peer-inert dotfile-subdirectory shape for the same
// reasons: Emby, Jellyfin, and Kodi scan the artist folder's top level for a
// fixed allowlist of artwork basenames and never recurse into a hidden subdir,
// so quarantined artwork is never re-ingested as artwork -- which for THIS
// feature is not merely tidy but load-bearing, since the whole point is that the
// picture stops being served as this artist's backdrop. A subdir also cannot
// collide with the atomic writer's transient "<target>.tmp"/".bak"/".removing"
// temporaries, which are always written next to the target file.
//
// It is deliberately SEPARATE from .sw-backup rather than another type-keyed
// subdir under it. .sw-backup is one-deep per image TYPE and is pruned on that
// basis (see handlers_image.go's per-type prune, which deletes every file in
// .sw-backup/fanart/ except the newest): a repair op quarantining three slots
// for one artist would be shredded down to one by the next prune, silently
// destroying the recoverability this feature's safety argument rests on. The
// quarantine is keyed by OPERATION, holds many slots per op, and is never
// pruned implicitly -- only consumed by an explicit restore or discard.
const RepairDirName = ".sw-repair"

// repairManifestName is the per-op manifest basename.
const repairManifestName = "manifest.json"

// repairOpIDPattern constrains an op id to a conservative, path-safe alphabet.
//
// PATH-SANITIZATION GUARD: the op id reaches os.MkdirAll/ReadDir/Remove/Rename
// and filesystem.WriteFileAtomic through repairOpDir. It is matched against this
// closed pattern -- lowercase hex, digits, and single hyphens, bounded length --
// BEFORE it is joined into any path, which dominates every os.* sink below it.
// The pattern admits no separator, no dot, and therefore no "..", so a tainted
// id cannot traverse out of the .sw-repair subtree. This fails closed at the
// filesystem layer and gives static analysis a recognizable sanitizer for the
// go/path-injection class.
var repairOpIDPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// maxRepairOpIDLen bounds the op id so a pathological caller cannot push the
// joined path past the filesystem's limit and turn a quarantine write -- the one
// write that must not fail -- into an error after the source is already gone.
const maxRepairOpIDLen = 64

// RepairEntry is one quarantined image: the bytes plus everything needed to
// judge, audit, and reverse the removal that produced it.
type RepairEntry struct {
	ArtistID   string `json:"artist_id"`
	ArtistName string `json:"artist_name"`
	ImageType  string `json:"image_type"`

	// SlotIndex is the DiscoverFanart ordinal the image occupied AT REMOVAL
	// TIME.
	//
	// It is PROVENANCE, NOT AN ADDRESS. Removal renumbers the surviving
	// slots to close the gap, so by the time anyone restores, this ordinal
	// denotes a DIFFERENT picture -- or nothing. Writing here on restore
	// would overwrite a bystander with the content that was deliberately
	// removed, which is the corruption this feature exists to undo. It is
	// recorded so the audit trail can say where the image came from, and it
	// is never used to decide where the image goes back. See
	// Pipeline.RestorePHashQuarantine.
	SlotIndex int `json:"slot_index"`

	// FileName is the original basename (and thus the original format).
	FileName string `json:"file_name"`

	// StoredName is the basename under the op dir. It is namespaced by slot
	// so two slots sharing an original basename cannot clobber each other.
	StoredName string `json:"stored_name"`

	// PHash is the removed image's perceptual hash, hex-encoded. It is the
	// restore path's true address: content, not position. Empty means the
	// hash was unknown at removal time, which makes the entry restorable
	// only by appending (it can be neither matched nor deduplicated).
	PHash string `json:"phash,omitempty"`

	// MatchedArtistID/Name and Similarity record WHY this image was removed:
	// the other side of the perceptual collision and how close it was. The
	// collision is symmetric and proves only that two artists share a
	// picture, never which of them owns it -- so this is the evidence a
	// human weighs when deciding to restore, not a verdict.
	MatchedArtistID   string  `json:"matched_artist_id,omitempty"`
	MatchedArtistName string  `json:"matched_artist_name,omitempty"`
	Similarity        float64 `json:"similarity"`

	QuarantinedAt time.Time `json:"quarantined_at"`
}

// RepairManifest is one repair operation's durable record.
type RepairManifest struct {
	OpID      string        `json:"op_id"`
	CreatedAt time.Time     `json:"created_at"`
	Entries   []RepairEntry `json:"entries"`
}

// repairOpMu guards each repair operation's manifest read-modify-write, keyed by
// (artist dir, op id).
//
// The manifest is a single shared file per operation and every mutator is a
// read-modify-write on it, so two goroutines quarantining different slots of the
// SAME op will both read the manifest, both append to their own copy, and both
// write -- and the last writer wins. The loser's entry vanishes while its BYTES
// remain on disk, referenced by nothing: unreachable through ListRepairOps,
// ReadRepairManifest and RepairEntryBytes alike. Both calls return nil, so the
// caller goes on to delete both originals, and the artwork whose entry was lost
// is gone through every supported path. A lock is what makes the manifest
// describe every set of bytes actually stored.
//
// Entries are never evicted: one mutex per operation ever run, for the process
// lifetime. That is the same unbounded-sync.Map shape as slotMu above (and
// Router.stillwaterManagedMu in internal/api), and it is bounded in practice by
// the number of repair operations a process performs -- a few bytes each. An
// eviction scheme would need its own lock to close the evict-while-acquiring
// race, which costs more than it saves.
var repairOpMu sync.Map // map[string]*sync.Mutex

// repairOpMutex returns the one mutex guarding an operation's manifest.
// LoadOrStore guarantees every caller for a given (dir, opID) gets the SAME
// mutex even when several arrive at once.
//
// Only ONE lock is ever held at a time by design -- there is no multi-lock
// acquisition here and so no lock-ordering hazard of the kind lockSlots has to
// sort around. Keep it that way: a mutator that needed two operations' manifests
// at once would have to establish an order first.
func repairOpMutex(dir, opID string) *sync.Mutex {
	key := filepath.Clean(dir) + "\x00" + opID
	mu, _ := repairOpMu.LoadOrStore(key, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// storedNameOf returns the entry's on-disk basename under the op dir, deriving
// it when the field is absent.
//
// Both sides of any comparison MUST go through this. Deriving only the caller's
// entry and matching it against the manifest's raw field means an entry whose
// StoredName is empty -- a hand-repaired manifest, or one written before the
// field existed -- never matches anything, so ConsumeRepairEntry removes
// nothing, unlinks nothing, and returns nil. That is a silent success that did
// not do the thing, on the path whose whole job is bookkeeping the operator's
// only remaining copy.
func storedNameOf(e RepairEntry) string {
	if e.StoredName != "" {
		return e.StoredName
	}
	return repairStoredName(e.SlotIndex, e.FileName)
}

// repairOpDir returns the per-operation quarantine subdirectory, validating the
// op id against repairOpIDPattern first. See that pattern's guard comment.
func repairOpDir(dir, opID string) (string, error) {
	if opID == "" || len(opID) > maxRepairOpIDLen || !repairOpIDPattern.MatchString(opID) {
		return "", fmt.Errorf("invalid repair op id %q", opID)
	}
	return filepath.Join(dir, RepairDirName, opID), nil
}

// repairStoredName namespaces the quarantined copy by slot so two slots that
// share an original basename cannot overwrite one another inside the op dir.
// The basename is reduced to its own base to strip any directory component a
// caller may have passed.
func repairStoredName(slotIndex int, fileName string) string {
	return fmt.Sprintf("%03d-%s", slotIndex, filepath.Base(fileName))
}

// QuarantineImage copies srcPath's bytes into the operation's quarantine and
// appends entry to the manifest. It is COPY-then-record, never move: the source
// file is left untouched for the caller to stage and commit separately.
//
// This ordering is the feature's safety hinge. The bytes must be durably
// somewhere else BEFORE the removal path touches the original, so that a crash
// at any instant leaves the artwork recoverable from either the quarantine or
// the still-present original -- never from neither. A move would make the
// quarantine and the removal the same non-atomic step and open exactly that
// window.
//
// The manifest is rewritten atomically on every append. That is O(n) writes for
// n slots, which is irrelevant at this scale (a handful of slots per artist) and
// buys ordering in ONE direction: the manifest never advertises an entry whose
// bytes are not already on disk, so no reader is handed a reference it cannot
// serve. The converse is NOT guaranteed. The bytes are written before the
// manifest is read, so a failure between the two -- an unreadable manifest, a
// crash -- leaves the stored bytes with no entry referencing them. That is the
// safe direction to fail in: this returns an error, the caller keeps the
// original and removes nothing, and a retry rewrites the same bytes and appends
// the entry. The orphan is inert, not lost work.
//
// CONCURRENCY: calls for the SAME (dir, opID) are serialized on repairOpMutex,
// and callers may safely fan slots of one operation across goroutines. That
// serialization is load-bearing, not defensive -- see repairOpMu for what an
// unguarded read-modify-write on the shared manifest destroys. Different
// operations, and different artists, proceed in parallel.
func QuarantineImage(dir, opID, srcPath string, entry RepairEntry) error {
	opDir, err := repairOpDir(dir, opID)
	if err != nil {
		return err
	}

	mu := repairOpMutex(dir, opID)
	mu.Lock()
	defer mu.Unlock()

	if err := os.MkdirAll(opDir, 0o750); err != nil {
		return fmt.Errorf("creating quarantine dir: %w", err)
	}

	// gosec G304: srcPath is caller-supplied and this function does NOT
	// validate it -- unlike opID, which repairOpDir sanitizes against a closed
	// pattern. It is a documented TRUST ASSUMPTION, not an enforced property:
	// every caller in this repo passes a DiscoverFanart result rooted at the
	// artist directory, and none of them derive it from request input. A caller
	// that ever passes an untrusted path would read whatever it names, so a
	// future exported entry point taking a user-supplied source must validate
	// before calling here rather than inherit this assumption.
	data, err := os.ReadFile(srcPath) //nolint:gosec // G304: caller-trusted path; see the trust-assumption comment above
	if err != nil {
		return fmt.Errorf("reading %s for quarantine: %w", filepath.Base(srcPath), err)
	}

	entry.StoredName = repairStoredName(entry.SlotIndex, entry.FileName)
	if entry.QuarantinedAt.IsZero() {
		entry.QuarantinedAt = time.Now().UTC()
	}

	if err := filesystem.WriteFileAtomic(filepath.Join(opDir, entry.StoredName), data, 0o644); err != nil {
		return fmt.Errorf("writing quarantined bytes for %s: %w", entry.FileName, err)
	}

	m, err := ReadRepairManifest(dir, opID)
	if err != nil {
		return err
	}
	if m == nil {
		m = &RepairManifest{OpID: opID, CreatedAt: time.Now().UTC()}
	}
	m.Entries = append(m.Entries, entry)
	return writeRepairManifest(dir, opID, m)
}

// writeRepairManifest atomically replaces the operation's manifest.
func writeRepairManifest(dir, opID string, m *RepairManifest) error {
	opDir, err := repairOpDir(dir, opID)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding repair manifest: %w", err)
	}
	if err := filesystem.WriteFileAtomic(filepath.Join(opDir, repairManifestName), data, 0o644); err != nil {
		return fmt.Errorf("writing repair manifest: %w", err)
	}
	return nil
}

// ReadRepairManifest returns the operation's manifest, or (nil, nil) when the
// operation does not exist. A malformed manifest is an ERROR, never an empty
// one: silently reporting "nothing to restore" over unreadable JSON would hide
// recoverable artwork behind a green light, and the bytes are still on disk to
// be recovered by hand.
func ReadRepairManifest(dir, opID string) (*RepairManifest, error) {
	opDir, err := repairOpDir(dir, opID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(opDir, repairManifestName)) //nolint:gosec // opID validated by repairOpDir
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading repair manifest: %w", err)
	}
	var m RepairManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("decoding repair manifest for op %s: %w", opID, err)
	}
	return &m, nil
}

// ListRepairOps returns the artist directory's quarantine op ids, sorted
// LEXICOGRAPHICALLY.
//
// That is deliberately not a claim about age. sort.Strings orders bytes, so this
// is chronological only for an id scheme whose lexicographic order happens to
// match its time order, and no op-id minter exists in this package to guarantee
// one. A caller that needs newest-first must sort by the manifest's CreatedAt,
// which is recorded for exactly that reason -- not by this slice's order.
//
// Ops whose id does not match repairOpIDPattern are skipped: nothing this
// package writes can produce one, so such a directory was not created here and
// must not be fed back into a path.
func ListRepairOps(dir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(dir, RepairDirName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading repair dir: %w", err)
	}
	var ops []string
	for _, e := range entries {
		if e.IsDir() && repairOpIDPattern.MatchString(e.Name()) && len(e.Name()) <= maxRepairOpIDLen {
			ops = append(ops, e.Name())
		}
	}
	sort.Strings(ops)
	return ops, nil
}

// RepairEntryBytes returns the quarantined bytes for one manifest entry.
func RepairEntryBytes(dir, opID string, entry RepairEntry) ([]byte, error) {
	opDir, err := repairOpDir(dir, opID)
	if err != nil {
		return nil, err
	}
	stored := storedNameOf(entry)
	// The stored name is derived here or read from a manifest this package
	// wrote; reduce it to a basename anyway so a hand-edited manifest cannot
	// steer the read out of the op dir.
	data, err := os.ReadFile(filepath.Join(opDir, filepath.Base(stored))) //nolint:gosec // opID validated, name reduced to basename
	if err != nil {
		return nil, fmt.Errorf("reading quarantined bytes for %s: %w", entry.FileName, err)
	}
	return data, nil
}

// ConsumeRepairEntry removes one entry's bytes and drops it from the manifest,
// after a restore has put the image back. The manifest is rewritten BEFORE the
// bytes are unlinked so a crash between the two leaves an orphaned file (inert,
// ignorable) rather than a manifest entry pointing at bytes that are gone --
// which would make every later read of this op fail on an entry nobody can
// serve. When the last entry goes, the op directory is removed.
//
// A no-op consume (no matching entry) is not an error: restore is idempotent by
// design, so a retried restore must be able to reach a clean end state rather
// than fail on the second attempt.
// CONCURRENCY: serialized per (dir, opID) on the same mutex QuarantineImage
// takes. This is a read-modify-write on the same shared manifest and races it
// identically -- an unguarded consume running against a concurrent quarantine
// drops the entry that quarantine just appended, orphaning its bytes.
func ConsumeRepairEntry(dir, opID string, entry RepairEntry) error {
	opDir, err := repairOpDir(dir, opID)
	if err != nil {
		return err
	}

	mu := repairOpMutex(dir, opID)
	mu.Lock()
	defer mu.Unlock()

	m, err := ReadRepairManifest(dir, opID)
	if err != nil || m == nil {
		return err
	}

	// BOTH sides derived: matching a derived name against the manifest's raw
	// field silently matches nothing when the stored side is empty. See
	// storedNameOf.
	stored := storedNameOf(entry)
	remaining := make([]RepairEntry, 0, len(m.Entries))
	for i := range m.Entries {
		if storedNameOf(m.Entries[i]) != stored {
			remaining = append(remaining, m.Entries[i])
		}
	}
	if len(remaining) == len(m.Entries) {
		return nil
	}
	m.Entries = remaining

	if len(m.Entries) > 0 {
		if err := writeRepairManifest(dir, opID, m); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(opDir, filepath.Base(stored))); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing consumed quarantine bytes: %w", err)
		}
		return nil
	}

	if err := os.RemoveAll(opDir); err != nil {
		return fmt.Errorf("removing emptied quarantine op dir: %w", err)
	}
	// Drop the .sw-repair root too once the last op is gone, so a clean
	// library carries no empty scaffolding. Best-effort and ordering-safe:
	// Remove on a non-empty dir fails, which is exactly the desired no-op
	// when a concurrent op still holds entries.
	repairRoot := filepath.Join(dir, RepairDirName)
	if err := os.Remove(repairRoot); err != nil && !os.IsNotExist(err) && !isDirNotEmpty(err) {
		return fmt.Errorf("removing emptied repair root: %w", err)
	}
	return nil
}

// WriteFanartBytes atomically writes restored fanart bytes to target.
//
// It deliberately does NOT go through Save. Save is the ACQUISITION pipeline: it
// re-encodes, cleans up conflicting formats, and injects fresh EXIF provenance
// describing where an image was just fetched from. Every one of those is wrong
// for a restore. The bytes being written are the exact bytes that were removed
// from this artist -- re-encoding would silently alter the artwork and shift its
// perceptual hash away from the manifest's, breaking the content-addressed
// idempotency check that makes restore safe to retry; and stamping "fetched
// now, from nowhere" over the original provenance would erase the very history a
// recovery is meant to reinstate. A restore returns bytes; it does not acquire
// an image.
func WriteFanartBytes(target string, data []byte) error {
	if err := filesystem.WriteFileAtomic(target, data, 0o644); err != nil {
		return fmt.Errorf("restoring %s: %w", filepath.Base(target), err)
	}
	return nil
}

// isDirNotEmpty reports whether err is the "directory not empty" a Remove
// returns for a populated directory. Matched on the errno's string rather than
// syscall.ENOTEMPTY to keep this file free of a build-tagged syscall import; the
// only caller treats a true result as "leave it alone", so a miss degrades to a
// wrapped error rather than a wrong action.
func isDirNotEmpty(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "not empty")
}
