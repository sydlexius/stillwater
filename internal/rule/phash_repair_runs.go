// Package rule -- phash_repair_runs.go
//
// Read-only listing of an artist's cross-artist backdrop back-outs (#2882).
//
// The back-out quarantines each removed backdrop under
// <artist dir>/.sw-repair/<op_id>/manifest.json. The op id needed to restore a
// run used to be returned only by the remediate response, so once that response
// was gone an operator had no way to find it. This file lists the runs straight
// from those manifests. There is deliberately no database table for runs: the
// manifests are the restore's own source of truth, so a second record could
// only drift from it.
package rule

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	img "github.com/sydlexius/stillwater/internal/image"
)

// PHashRepairRunEntry is one quarantined backdrop within a back-out run, limited
// to what an operator needs to recognize it before confirming a restore.
type PHashRepairRunEntry struct {
	// ImageType is the kind of image removed (always a backdrop today).
	ImageType string `json:"image_type"`
	// FileName is the original basename of the removed file.
	FileName string `json:"file_name"`
	// StoredName is the basename of the quarantined copy inside the run.
	StoredName string `json:"stored_name"`
	// SlotIndex is where the image sat when it was removed. Provenance only: it
	// is never a restore target (see image.RepairEntry.SlotIndex).
	SlotIndex int `json:"slot_index"`
	// PHash is the removed image's perceptual hash (hex), empty when unknown.
	PHash string `json:"phash,omitempty"`
	// MatchedArtistID and MatchedArtistName name the other side of the
	// collision that caused the removal.
	MatchedArtistID   string `json:"matched_artist_id,omitempty"`
	MatchedArtistName string `json:"matched_artist_name,omitempty"`
	// Similarity is how close the collision was (0..1).
	Similarity    float64   `json:"similarity"`
	QuarantinedAt time.Time `json:"quarantined_at"`
}

// PHashRepairRun is one back-out operation that still has a manifest on disk.
type PHashRepairRun struct {
	OpID      string                `json:"op_id"`
	CreatedAt time.Time             `json:"created_at"`
	Entries   []PHashRepairRunEntry `json:"entries"`
}

// ListPHashRepairRuns lists the artist's back-out runs, newest first.
//
// Runs whose directory name or manifest is unusable (a foreign directory, a
// malformed or half-written manifest) are skipped with a warning rather than
// failing the whole list: one bad run must not hide the ids of the good ones.
// A real I/O failure on the .sw-repair directory itself IS returned, and a
// missing .sw-repair directory is simply an empty list. An unknown artist
// returns an error wrapping artist.ErrNotFound.
//
// The result is never nil, so it encodes as [] rather than null.
func (p *Pipeline) ListPHashRepairRuns(ctx context.Context, artistID string) ([]PHashRepairRun, error) {
	if p.artistService == nil {
		return nil, fmt.Errorf("list phash repair runs: pipeline not fully wired")
	}
	a, err := p.artistService.GetByID(ctx, artistID)
	if err != nil {
		return nil, fmt.Errorf("loading artist %s: %w", artistID, err)
	}
	if a.Path == "" {
		// No directory means nothing can have been quarantined for this artist.
		return []PHashRepairRun{}, nil
	}

	// If the artist folder itself is gone (a library share that is unmounted),
	// the back-outs may still exist but cannot be seen. Saying "none" would be
	// a false answer, so report an error. A present folder with no .sw-repair
	// is the genuine "never backed out" case and stays an empty list below.
	if _, err := os.Stat(a.Path); err != nil {
		return nil, fmt.Errorf("artist folder for %s is not readable: %w", artistID, err)
	}

	opIDs, err := img.ListRepairOps(a.Path)
	if err != nil {
		return nil, err
	}

	runs := make([]PHashRepairRun, 0, len(opIDs))
	for _, opID := range opIDs {
		m, err := img.ReadRepairManifest(ctx, a.Path, opID)
		if err != nil {
			// A canceled context or a stalled library mount (the process-wide
			// abandoned-read cap) is a fact about the whole mount, not about
			// this one manifest. Skipping would list fewer back-outs and
			// report them as absent, so abort with the cause (#2933). Only a
			// genuinely bad manifest falls through to warn-and-skip.
			if abort := img.ReadFailureDistrustsLoop(ctx, err); abort != nil {
				return nil, fmt.Errorf("listing back-outs for %s: %w", artistID, abort)
			}
			p.logger.Warn("skipping unreadable back-out manifest",
				slog.String("artist_id", artistID),
				slog.String("op_id", opID),
				slog.String("error", err.Error()))
			continue
		}
		if m == nil {
			// Directory exists but holds no manifest (e.g. a crashed first write).
			p.logger.Warn("skipping back-out directory with no manifest",
				slog.String("artist_id", artistID),
				slog.String("op_id", opID))
			continue
		}
		if len(m.Entries) == 0 {
			// Nothing left to restore (an interrupted run left the manifest
			// behind after its entries were consumed). The list shows only
			// back-outs that still hold quarantined backdrops.
			p.logger.Warn("skipping back-out with no quarantined entries",
				slog.String("artist_id", artistID),
				slog.String("op_id", opID))
			continue
		}
		runs = append(runs, toPHashRepairRun(opID, m))
	}

	// Newest first by the manifest's own timestamp. ListRepairOps returns ids in
	// lexical order, which says nothing about age, so it cannot be trusted here.
	// The op id breaks exact-timestamp ties so the order is deterministic. A
	// zero (missing) timestamp is the oldest possible, so it sorts last.
	sort.SliceStable(runs, func(i, j int) bool {
		if !runs[i].CreatedAt.Equal(runs[j].CreatedAt) {
			return runs[i].CreatedAt.After(runs[j].CreatedAt)
		}
		return runs[i].OpID < runs[j].OpID
	})
	return runs, nil
}

// IsArtistNotFound reports whether err came from an unknown artist, so the API
// layer can map it to 404 without importing the artist package's sentinel.
func IsArtistNotFound(err error) bool {
	return errors.Is(err, artist.ErrNotFound)
}

// toPHashRepairRun projects a manifest onto the list shape. It uses the
// directory's op id (already validated by ListRepairOps) rather than the id
// recorded inside the file, so the id returned is always one a restore accepts.
func toPHashRepairRun(opID string, m *img.RepairManifest) PHashRepairRun {
	run := PHashRepairRun{
		OpID:      opID,
		CreatedAt: m.CreatedAt,
		Entries:   make([]PHashRepairRunEntry, 0, len(m.Entries)),
	}
	for i := range m.Entries {
		e := &m.Entries[i]
		run.Entries = append(run.Entries, PHashRepairRunEntry{
			ImageType:         e.ImageType,
			FileName:          e.FileName,
			StoredName:        e.StoredName,
			SlotIndex:         e.SlotIndex,
			PHash:             e.PHash,
			MatchedArtistID:   e.MatchedArtistID,
			MatchedArtistName: e.MatchedArtistName,
			Similarity:        e.Similarity,
			QuarantinedAt:     e.QuarantinedAt,
		})
	}
	return run
}
