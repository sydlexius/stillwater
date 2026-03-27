package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider/musicbrainz"
)

// handleGetMBDiffs returns the computed diffs between Stillwater metadata and
// the last-known MusicBrainz values for the given artist.
// GET /api/v1/artists/{id}/musicbrainz/diffs
func (r *Router) handleGetMBDiffs(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	ctx := req.Context()

	a, err := r.artistService.GetByID(ctx, artistID)
	if err != nil {
		if errors.Is(err, artist.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
			return
		}
		r.logger.Error("failed to get artist for diffs", "artist_id", artistID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	if a.MusicBrainzID == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist has no MusicBrainz ID"})
		return
	}

	snapshots, err := r.artistService.GetMBSnapshots(ctx, artistID)
	if err != nil {
		r.logger.Error("failed to get MB snapshots", "artist_id", artistID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	diffs := musicbrainz.ComputeDiffs(a, snapshots, a.MetadataSources)

	contributionMode := r.getStringSetting(ctx, "musicbrainz.contributions", "disabled")

	// Find the most recent snapshot fetch time for the "last_fetched_at" field.
	var lastFetched *time.Time
	for _, s := range snapshots {
		if lastFetched == nil || s.FetchedAt.After(*lastFetched) {
			t := s.FetchedAt
			lastFetched = &t
		}
	}

	var lastFetchedStr string
	if lastFetched != nil {
		lastFetchedStr = lastFetched.UTC().Format("2006-01-02T15:04:05Z")
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"artist_id":         a.ID,
		"musicbrainz_id":    a.MusicBrainzID,
		"diffs":             diffs,
		"contribution_mode": contributionMode,
		"last_fetched_at":   lastFetchedStr,
	})
}
