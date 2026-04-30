package nfo

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/filesystem"
)

// WriteBackArtistNFO writes the artist's current metadata to an artist.nfo file
// using the default (Kodi-compatible) field mapping. If ss is non-nil and an
// existing NFO file is present, a snapshot of the old content is saved before
// overwriting (best effort -- snapshot failure does not prevent the write). The
// write uses the atomic tmp/bak/rename pattern via filesystem.WriteFileAtomic.
//
// LockData defaults to false: the resulting NFO does NOT carry
// <lockdata>true</lockdata>, so Emby/Jellyfin metadata refreshes will not be
// blocked. Callers that want to opt into the lock semantic must use
// WriteBackArtistNFOWithFieldMap with lockNFO=true (issue #1264).
//
// The returned error is non-nil only when the NFO file itself could not be
// written. Snapshot errors are logged at Warn level when a logger is provided
// but never prevent the write. When logger is nil, snapshot errors are
// swallowed silently.
func WriteBackArtistNFO(ctx context.Context, a *artist.Artist, ss *SnapshotService, logger *slog.Logger) error {
	return WriteBackArtistNFOWithFieldMap(ctx, a, ss, logger, DefaultFieldMap(), false)
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
func WriteBackArtistNFOWithFieldMap(ctx context.Context, a *artist.Artist, ss *SnapshotService, logger *slog.Logger, fm NFOFieldMap, lockNFO bool) error {
	if a == nil {
		return fmt.Errorf("write artist nfo: artist is nil")
	}
	if a.Path == "" {
		return fmt.Errorf("write artist nfo: artist path is empty")
	}

	target := filepath.Join(a.Path, "artist.nfo")

	// Save a snapshot of the existing NFO before overwriting (best effort)
	if ss != nil {
		if existing, err := os.ReadFile(target); err == nil && len(existing) > 0 { //nolint:gosec // G304: path from trusted artist.Path
			if _, snapErr := ss.Save(ctx, a.ID, string(existing)); snapErr != nil {
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
	}

	nfoData := FromArtistWithFieldMap(a, fm)

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
