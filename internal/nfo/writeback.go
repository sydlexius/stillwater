package nfo

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/filesystem"
)

// WriteBackArtistNFO writes the artist's current metadata to an artist.nfo file.
// If ss is non-nil and an existing NFO file is present, a snapshot of the old
// content is saved before overwriting. The write uses the atomic tmp/bak/rename
// pattern via filesystem.WriteFileAtomic.
func WriteBackArtistNFO(ctx context.Context, a *artist.Artist, ss *SnapshotService) error {
	target := filepath.Join(a.Path, "artist.nfo")

	// Save a snapshot of the existing NFO before overwriting
	if ss != nil {
		if existing, err := os.ReadFile(target); err == nil && len(existing) > 0 { //nolint:gosec // G304: path from trusted artist.Path
			if _, err := ss.Save(ctx, a.ID, string(existing)); err != nil {
				return fmt.Errorf("saving nfo snapshot: %w", err)
			}
		}
	}

	nfoData := FromArtist(a)
	var buf bytes.Buffer
	if err := Write(&buf, nfoData); err != nil {
		return fmt.Errorf("serializing nfo: %w", err)
	}

	if err := filesystem.WriteFileAtomic(target, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("writing nfo file: %w", err)
	}

	return nil
}
