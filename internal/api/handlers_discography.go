package api

import (
	"cmp"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/event"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/web/templates"
)

// writeDiscographyJSONError writes a JSON error payload matching the
// OpenAPI Error contract for the discography tab endpoint.
func writeDiscographyJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// handleArtistDiscographyTab renders the Discography tab fragment for the
// artist detail page. Album entries are sourced from the on-disk artist.nfo
// so what the user sees matches exactly what Kodi/Emby/Jellyfin will read.
// GET /artists/{id}/discography/tab
func (r *Router) handleArtistDiscographyTab(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		if errors.Is(err, artist.ErrNotFound) {
			writeDiscographyJSONError(w, http.StatusNotFound, "artist not found")
			return
		}
		r.logger.Error("loading artist for discography tab", "artist_id", artistID, "error", err)
		writeDiscographyJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Parse the on-disk NFO directly so the tab reflects persisted state.
	// The cached artist.NFOExists flag is intentionally not consulted here:
	// if the file was added or restored out-of-band, the tab should still
	// reflect reality rather than waiting for a separate scan to refresh
	// the DB flag. ErrNotExist is treated as an empty-state signal; all
	// other read/parse errors are warned so operators can diagnose.
	var albums []artist.DiscographyAlbum
	if a.Path != "" {
		nfoPath := filepath.Join(a.Path, "artist.nfo")
		parsed, err := parseNFOFile(nfoPath)
		switch {
		case err == nil:
			albums = discographyFromNFO(parsed)
		case errors.Is(err, os.ErrNotExist):
			// No file on disk: render empty state silently.
		default:
			r.logger.Warn("failed to parse artist.nfo for discography tab",
				"artist_id", artistID, "path", nfoPath, "error", err)
		}
	}

	// Parse search/sort/order query params for client-side filtering.
	// Defaults: no search, no sort (NFO order), ascending.
	search := req.URL.Query().Get("search")
	sortBy := req.URL.Query().Get("sort") // "title" or "year"; empty = NFO order
	order := req.URL.Query().Get("order") // "asc" or "desc"; default "asc"
	if order != "desc" {
		order = "asc"
	}

	// Apply case-insensitive title filter.
	if search != "" {
		lower := strings.ToLower(search)
		filtered := make([]artist.DiscographyAlbum, 0, len(albums))
		for _, alb := range albums {
			if strings.Contains(strings.ToLower(alb.Title), lower) {
				filtered = append(filtered, alb)
			}
		}
		albums = filtered
	}

	// Apply sort when a sort field is requested. Stable sort preserves NFO
	// order as a secondary key so entries with identical values stay consistent.
	switch sortBy {
	case "title":
		slices.SortStableFunc(albums, func(a, b artist.DiscographyAlbum) int {
			c := cmp.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title))
			if order == "desc" {
				return -c
			}
			return c
		})
	case "year":
		// Empty year strings sort after all real years regardless of direction.
		// Using a sentinel ("9999") breaks under desc because negating the
		// comparison flips undated albums to the top.  Instead, handle the
		// empty case explicitly so undated entries always land last.
		slices.SortStableFunc(albums, func(a, b artist.DiscographyAlbum) int {
			aEmpty := a.Year == ""
			bEmpty := b.Year == ""
			switch {
			case aEmpty && bEmpty:
				return 0
			case aEmpty:
				return 1 // undated always after dated
			case bEmpty:
				return -1 // dated always before undated
			}
			c := cmp.Compare(a.Year, b.Year)
			if order == "desc" {
				return -c
			}
			return c
		})
	}

	renderTempl(w, req, templates.ArtistDiscographyTab(templates.DiscographyTabData{
		ArtistID:      artistID,
		MusicBrainzID: a.MusicBrainzID,
		Albums:        albums,
		Search:        search,
		Sort:          sortBy,
		Order:         order,
	}))
}

// DiscographyFetchResult is the JSON response for POST /api/v1/artists/{id}/discography/fetch.
type DiscographyFetchResult struct {
	Added   int `json:"added"`
	Kept    int `json:"kept"`
	Skipped int `json:"skipped"`
	Total   int `json:"total"`
}

// handleFetchDiscography fetches release groups from MusicBrainz for an artist,
// merges them into the on-disk NFO, and writes the result atomically.
//
// POST /api/v1/artists/{id}/discography/fetch
//
// Query/body params:
//   - include: comma-separated release types (default "Album,EP")
//
// Returns DiscographyFetchResult as JSON.
func (r *Router) handleFetchDiscography(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		if errors.Is(err, artist.ErrNotFound) {
			writeDiscographyJSONError(w, http.StatusNotFound, "artist not found")
			return
		}
		r.logger.Error("loading artist for discography fetch", "artist_id", artistID, "error", err)
		writeDiscographyJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if a.MusicBrainzID == "" {
		writeDiscographyJSONError(w, http.StatusBadRequest,
			"artist has no MusicBrainz ID; use Re-identify to link this artist to MusicBrainz first")
		return
	}

	if a.Path == "" {
		writeDiscographyJSONError(w, http.StatusBadRequest,
			"artist has no filesystem path; discography cannot be written without a path")
		return
	}

	// Atomic check-and-set: reject if a fetch for this artist is already
	// running, otherwise claim the slot before the MusicBrainz round-trip so
	// two interleaved read-modify-write cycles cannot race the NFO write.
	r.discographyFetchMu.Lock()
	if r.discographyFetchInFlight[artistID] {
		r.discographyFetchMu.Unlock()
		writeDiscographyJSONError(w, http.StatusConflict,
			"a discography fetch for this artist is already in progress")
		return
	}
	r.discographyFetchInFlight[artistID] = true
	r.discographyFetchMu.Unlock()
	defer func() {
		r.discographyFetchMu.Lock()
		delete(r.discographyFetchInFlight, artistID)
		r.discographyFetchMu.Unlock()
	}()

	// Query parameter takes precedence; a JSON body is the fallback.
	filter := nfo.ParseReleaseTypeFilter(resolveIncludeParam(req))

	// Resolve the MB provider from the registry. The MB adapter implements
	// provider.ReleaseGroupFetcher.
	mbAdapter := r.resolveMBAdapter()
	if mbAdapter == nil {
		writeDiscographyJSONError(w, http.StatusServiceUnavailable,
			"MusicBrainz provider is not available")
		return
	}

	// Fetch release groups from MusicBrainz. The adapter respects the shared
	// rate limiter so this call honors the 1 req/sec policy.
	groups, err := mbAdapter.GetReleaseGroups(req.Context(), a.MusicBrainzID)
	if err != nil {
		r.logger.Warn("fetching release groups from MusicBrainz",
			"artist_id", artistID,
			"mbid", a.MusicBrainzID,
			"error", err)
		writeDiscographyJSONError(w, http.StatusBadGateway,
			"MusicBrainz fetch failed")
		return
	}

	// Parse the existing on-disk NFO so user-added entries are preserved.
	nfoPath := filepath.Join(a.Path, "artist.nfo")
	var existingNFO *nfo.ArtistNFO
	parsed, parseErr := parseNFOFile(nfoPath)
	switch {
	case parseErr == nil:
		existingNFO = parsed
	case errors.Is(parseErr, os.ErrNotExist):
		// No file yet: start from an empty NFO seeded from the DB artist.
		existingNFO = nfo.FromArtist(a)
	default:
		// Corrupt or unreadable NFO: refuse to overwrite. Falling back and
		// overwriting would silently destroy user-added <album> entries and
		// any other hand-edited content. The operator must fix or remove the
		// file before a fetch can proceed.
		r.logger.Error("failed to parse existing artist.nfo for discography fetch",
			"artist_id", artistID, "path", nfoPath, "error", parseErr)
		writeDiscographyJSONError(w, http.StatusUnprocessableEntity,
			"existing artist.nfo could not be parsed; fix or remove it before fetching")
		return
	}

	// Merge the incoming release groups into the existing album list.
	mergedAlbums, mergeResult := nfo.MergeDiscographyFromMBReleaseGroups(
		existingNFO.Albums,
		groups,
		filter,
	)

	// Only write to disk when something actually changed.
	if mergeResult.Added > 0 {
		existingNFO.Albums = mergedAlbums

		// Stamp provenance so external overwrites can be detected.
		existingNFO.Stillwater = &nfo.StillwaterMeta{
			Version: nfo.StillwaterVersion,
			Written: time.Now().UTC().Format(time.RFC3339),
		}

		// Take a snapshot of the existing NFO before overwriting, so the
		// user has a recovery path if the write produces unexpected output.
		// Mirrors the pattern in WriteBackArtistNFOWithFieldMap: snapshot
		// errors are logged at Warn but never block the write.
		if r.nfoSnapshotService != nil {
			if existing, readErr := os.ReadFile(filepath.Clean(nfoPath)); readErr == nil && len(existing) > 0 {
				if _, snapErr := r.nfoSnapshotService.Save(req.Context(), artistID, string(existing)); snapErr != nil {
					r.logger.Warn("NFO snapshot save failed before discography write",
						"artist_id", artistID,
						"path", nfoPath,
						"error", snapErr)
				}
			}
		}

		if err := nfo.WriteNFOAtomic(nfoPath, existingNFO); err != nil {
			r.logger.Error("writing NFO after discography fetch",
				"artist_id", artistID,
				"path", nfoPath,
				"error", err)
			writeDiscographyJSONError(w, http.StatusInternalServerError,
				"failed to write NFO file")
			return
		}

		r.logger.Info("discography fetched and written",
			"artist_id", artistID,
			"mbid", a.MusicBrainzID,
			"added", mergeResult.Added,
			"kept", mergeResult.Kept,
			"total", mergeResult.Total)

		// Notify SSE subscribers so event-driven clients refresh the artist
		// view after the on-disk discography changes. Mirrors the post-write
		// notification in the image handlers.
		if r.eventBus != nil {
			r.eventBus.Publish(event.Event{
				Type: event.ArtistUpdated,
				Data: map[string]any{"artist_id": artistID},
			})
		}
	}

	result := DiscographyFetchResult{
		Added:   mergeResult.Added,
		Kept:    mergeResult.Kept,
		Skipped: mergeResult.Skipped,
		Total:   mergeResult.Total,
	}

	// HX-Request callers receive a re-rendered tab partial so the album list
	// updates in place. The HTMX button uses hx-target="#discography-tab-content"
	// + hx-swap="outerHTML", so this partial replaces the entire tab div.
	// Non-HX callers (API / curl) receive the JSON DiscographyFetchResult so the
	// OpenAPI contract is preserved.
	if req.Header.Get("HX-Request") == "true" {
		// Re-read the merged albums for the partial. When nothing was added the
		// existingNFO albums are unchanged; when something was added the slice
		// was already updated above. FetchAdded/FetchTotal wire the count into
		// the "artist.discography.fetch.summary" i18n key rendered in the partial.
		renderTempl(w, req, templates.ArtistDiscographyTab(templates.DiscographyTabData{
			ArtistID:      artistID,
			MusicBrainzID: a.MusicBrainzID,
			Albums:        discographyFromNFO(existingNFO),
			FetchAdded:    result.Added,
			FetchTotal:    result.Total,
		}))
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// resolveIncludeParam returns the release-type "include" parameter for a
// discography fetch. The query string takes precedence; a JSON request body
// is consulted as a fallback only when no query value is present and the
// Content-Type indicates JSON. A missing or malformed body yields "".
func resolveIncludeParam(req *http.Request) string {
	if q := req.URL.Query().Get("include"); q != "" {
		return q
	}
	ct := req.Header.Get("Content-Type")
	if ct == "application/json" || (len(ct) > 16 && ct[:16] == "application/json") {
		var body struct {
			Include string `json:"include"`
		}
		// Best-effort decode; a missing or invalid body must not fail the request.
		_ = json.NewDecoder(req.Body).Decode(&body)
		return body.Include
	}
	return ""
}

// resolveMBAdapter returns the MusicBrainz adapter from the provider registry
// when it implements provider.ReleaseGroupFetcher. Returns nil when unavailable.
func (r *Router) resolveMBAdapter() provider.ReleaseGroupFetcher {
	if r.providerRegistry == nil {
		return nil
	}
	p := r.providerRegistry.Get(provider.NameMusicBrainz)
	if p == nil {
		return nil
	}
	// Cast via the interface rather than the concrete type so test stubs can
	// also satisfy the contract without importing the musicbrainz package.
	fetcher, ok := p.(provider.ReleaseGroupFetcher)
	if !ok {
		return nil
	}
	return fetcher
}

// discographyFromNFO maps NFO album entries into the artist-domain type used
// by the template. Kept as a small local helper to avoid a cross-package
// dependency from templates on the nfo package.
func discographyFromNFO(n *nfo.ArtistNFO) []artist.DiscographyAlbum {
	if n == nil || len(n.Albums) == 0 {
		return nil
	}
	out := make([]artist.DiscographyAlbum, 0, len(n.Albums))
	for _, alb := range n.Albums {
		out = append(out, artist.DiscographyAlbum{
			Title:                     alb.Title,
			Year:                      alb.Year,
			MusicBrainzReleaseGroupID: alb.MusicBrainzReleaseGroupID,
		})
	}
	return out
}
