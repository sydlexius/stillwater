package nfo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/filesystem"
)

// WriteNFOAtomic serializes n to XML and writes it to path using the atomic
// tmp/bak/rename pattern from filesystem.WriteFileAtomic. Unlike
// WriteBackArtistNFOWithFieldMap, it operates directly on an ArtistNFO value --
// the caller owns all mutation (e.g. merging albums) before passing the final
// struct in. No snapshot is taken; call WriteBackArtistNFOWithFieldMap when
// snapshot tracking is needed.
func WriteNFOAtomic(path string, n *ArtistNFO) error {
	if path == "" {
		return fmt.Errorf("write nfo: path is empty")
	}
	if n == nil {
		return fmt.Errorf("write nfo: nfo is nil")
	}
	var buf bytes.Buffer
	if err := Write(&buf, n); err != nil {
		return fmt.Errorf("serializing nfo: %w", err)
	}
	if err := filesystem.WriteFileAtomic(path, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("writing nfo file: %w", err)
	}
	return nil
}

// WriteBackArtistNFOWithFieldMap writes the artist's current metadata to an
// artist.nfo file, applying the given NFOFieldMap to determine how genres,
// styles, and moods are mapped to NFO XML elements. This enables
// platform-specific output (e.g., writing moods as <style> for Emby/Jellyfin).
//
// lockNFO controls whether <lockdata>true</lockdata> is stamped into the
// output. The publisher resolves this from the owning library's NFOLockData
// setting (default false). Stamping lockdata tells Emby/Jellyfin to refuse
// metadata refreshes for the artist; opt in only when that is desired.
//
// The <stillwater> provenance element is always written so external
// overwrites can be detected on read regardless of the lock setting.
//
// Discography (<album> entries) is preserved from the existing on-disk NFO
// (#1616). Discography is not stored in the database, so a write-back driven
// by a DB-loaded artist would otherwise erase it; see the inline comment at
// the preservation step for details.
func WriteBackArtistNFOWithFieldMap(ctx context.Context, a *artist.Artist, ss *SnapshotService, logger *slog.Logger, fm NFOFieldMap, lockNFO bool) error {
	if a == nil {
		return fmt.Errorf("write artist nfo: artist is nil")
	}
	if a.Path == "" {
		return fmt.Errorf("write artist nfo: artist path is empty")
	}

	target := filepath.Join(a.Path, "artist.nfo")

	// Read the existing on-disk NFO once. The bytes are needed for two
	// independent best-effort purposes below: the optional pre-write snapshot
	// and discography preservation. The write-back stays best-effort, but the
	// two read-failure cases are not equivalent: a missing file is benign
	// (nothing to preserve), while a permission or I/O error means an NFO
	// very likely exists and the write below will overwrite it without its
	// discography. Log the latter loudly so it is not a silent data loss.
	existingContent, readErr := os.ReadFile(target) //nolint:gosec // G304: path from trusted artist.Path
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		log := logger
		if log == nil {
			log = slog.Default()
		}
		log.Warn("could not read existing NFO before write-back; discography (if any) will not be preserved",
			slog.String("artist_id", a.ID),
			slog.String("nfo_path", target),
			slog.String("error", readErr.Error()),
		)
	}

	// Save a snapshot of the existing NFO before overwriting (best effort).
	if ss != nil && readErr == nil && len(existingContent) > 0 {
		if _, snapErr := ss.Save(ctx, a.ID, string(existingContent)); snapErr != nil {
			log := logger
			if log == nil {
				log = slog.Default()
			}
			log.Warn("NFO snapshot save failed (proceeding with write)",
				slog.String("artist_id", a.ID),
				slog.String("error", snapErr.Error()),
			)
		}
	}

	nfoData := FromArtistWithFieldMap(a, fm)

	// Preserve the existing discography (#1616).
	//
	// Discography (<album> entries) is NFO-only: it is never stored in the
	// database, and the artist.Artist model loaded from the DB carries an
	// empty Discography slice. FromArtistWithFieldMap therefore produces an
	// ArtistNFO with no <album> entries. Without the merge below, every
	// write-back triggered from a DB-loaded artist (connection auto-push,
	// metadata refresh) would silently wipe whatever discography the on-disk
	// NFO already contained -- including entries written by the "Fetch
	// discography" button (#1065) or the discography rule (#1063).
	//
	// To avoid that, carry the existing on-disk discography forward whenever
	// nfoData itself does not already supply one. If the in-memory artist
	// does carry discography (e.g. freshly scanned from the same NFO), that
	// value wins and the on-disk parse is not needed.
	if len(nfoData.Albums) == 0 && readErr == nil && len(existingContent) > 0 {
		if existingNFO, parseErr := Parse(bytes.NewReader(existingContent)); parseErr == nil {
			if len(existingNFO.Albums) > 0 {
				nfoData.Albums = existingNFO.Albums
			}
		} else {
			// An unparsable existing NFO must not block the write-back; the
			// write is best-effort and proceeds without discography
			// preservation. Log at Warn, not Debug: this branch drops every
			// <album> entry the old NFO held, and discography is NFO-only with
			// no DB copy to recover from, so operators must see it on a
			// default log level.
			log := logger
			if log == nil {
				log = slog.Default()
			}
			log.Warn("existing NFO could not be parsed; its discography entries will not be carried into the rewritten file",
				slog.String("artist_id", a.ID),
				slog.String("nfo_path", target),
				slog.String("error", parseErr.Error()),
			)
		}
	}

	// Lockdata is opt-in per library (issue #1264). The previous unconditional
	// stamp blocked Emby/Jellyfin refreshes for every artist Stillwater wrote.
	nfoData.LockData = lockNFO

	// Stamp provenance so external overwrites can be detected. Independent of
	// the lock setting -- harmless metadata that lets future detect-and-rewrite
	// flows recognize a Stillwater-authored file.
	nfoData.Stillwater = &StillwaterMeta{
		Version: StillwaterVersion,
		Written: time.Now().UTC().Format(time.RFC3339),
	}

	var buf bytes.Buffer
	if err := Write(&buf, nfoData); err != nil {
		return fmt.Errorf("serializing nfo: %w", err)
	}

	if err := filesystem.WriteFileAtomic(target, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("writing nfo file: %w", err)
	}

	return nil
}
