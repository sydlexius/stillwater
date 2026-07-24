package emby

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/httpclient"
	"github.com/sydlexius/stillwater/internal/connection/mediabrowser"
)

// tagItem is a named tag for Emby's TagItems field.
// Emby uses {Name, Id} objects instead of flat strings; only Name is required
// when writing.
type tagItem struct {
	Name string `json:"Name"`
}

// itemUpdateBody is the payload for POST /Items/{id}.
type itemUpdateBody struct {
	Name           string            `json:"Name"`
	ForcedSortName string            `json:"ForcedSortName,omitempty"`
	Overview       string            `json:"Overview,omitempty"`
	Genres         []string          `json:"Genres,omitempty"`
	TagItems       []tagItem         `json:"TagItems,omitempty"`
	ProviderIds    map[string]string `json:"ProviderIds,omitempty"`
	PremiereDate   string            `json:"PremiereDate,omitempty"`
	EndDate        string            `json:"EndDate,omitempty"`
	// LockedFields is included ONLY when Stillwater needs to pin a
	// derived value on the Emby side (currently: derived numeric-prefix
	// SortName per #1083). The omitempty tag is what keeps the existing
	// pure-metadata push behavior unchanged for all other artists: an
	// empty slice is dropped from the JSON body, so we never clobber
	// existing platform-side locks for the non-derived case.
	LockedFields []string `json:"LockedFields,omitempty"`
}

// PushMetadata updates metadata for an artist item on the Emby server.
func (c *Client) PushMetadata(ctx context.Context, platformArtistID string, data connection.ArtistPushData) error {
	// Styles and Moods map to TagItems (Emby uses {Name} objects, not flat
	// strings). Disambiguation and YearsActive have no corresponding Emby
	// fields and are omitted.
	items := make([]tagItem, 0, len(data.Styles)+len(data.Moods))
	for _, s := range data.Styles {
		items = append(items, tagItem{Name: s})
	}
	for _, m := range data.Moods {
		items = append(items, tagItem{Name: m})
	}
	body := itemUpdateBody{
		Name:     data.Name,
		Overview: data.Biography,
		Genres:   data.Genres,
		TagItems: items,
	}
	if data.SortName != "" {
		body.ForcedSortName = data.SortName
	}
	// When Stillwater derived the SortName (#1083), Emby will reset it
	// on the next refresh unless "SortName" is in LockedFields. Fetch
	// the current locks and merge so user-set per-field locks survive
	// the push. Failure here downgrades to a warn-and-continue: we
	// prefer to ship the metadata even if locks could not be merged,
	// because the alternative is silently failing the entire push for
	// every numeric-prefix artist. The next push will retry the merge.
	if data.LockSortName {
		if merged, err := c.fetchAndMergeLockedFields(ctx, platformArtistID, "SortName"); err == nil {
			body.LockedFields = merged
		} else {
			c.Logger.Warn("emby: locked-fields merge failed, pushing without SortName lock",
				"artist_id", platformArtistID, "error", err)
		}
	}
	// Populate ProviderIds with every external ID Stillwater has. Empty IDs
	// are omitted so we never overwrite an existing platform-side value with
	// "". Key naming matches the convention used by Emby's metadata fetcher
	// plugins.
	providerIDs := buildProviderIDs(data)
	if len(providerIDs) > 0 {
		body.ProviderIds = providerIDs
	}
	// Use Born for persons, Formed for groups as premiere date.
	// Normalize to yyyy-MM-dd so Emby does not silently discard partial dates.
	// Only set when normalization succeeds to avoid sending empty strings.
	if raw := data.Born; raw != "" {
		normalized := connection.NormalizeDateForPlatform(raw)
		c.logDateNormalization("premiere_date", raw, normalized, platformArtistID)
		if normalized != "" {
			body.PremiereDate = normalized
		}
	} else if raw := data.Formed; raw != "" {
		normalized := connection.NormalizeDateForPlatform(raw)
		c.logDateNormalization("premiere_date", raw, normalized, platformArtistID)
		if normalized != "" {
			body.PremiereDate = normalized
		}
	}
	// Use Died for persons, Disbanded for groups as end date.
	if raw := data.Died; raw != "" {
		normalized := connection.NormalizeDateForPlatform(raw)
		c.logDateNormalization("end_date", raw, normalized, platformArtistID)
		if normalized != "" {
			body.EndDate = normalized
		}
	} else if raw := data.Disbanded; raw != "" {
		normalized := connection.NormalizeDateForPlatform(raw)
		c.logDateNormalization("end_date", raw, normalized, platformArtistID)
		if normalized != "" {
			body.EndDate = normalized
		}
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshaling push body: %w", err)
	}

	path := fmt.Sprintf("/Items/%s", url.PathEscape(platformArtistID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, connection.BuildRequestURL(c.BaseURL, path), bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("creating push request: %w", err)
	}
	c.AuthFunc(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("executing push request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup

	if resp.StatusCode >= 300 {
		statusErr := httpclient.ReadBoundedStatusError(resp)
		// Historical error wording preserved ("push failed with status N: body")
		// for test fixtures and operator log familiarity; errors.Join attaches
		// the typed StatusError as a sibling in the error tree so
		// wrapAuthIfStatusAuth (via errors.As) can still detect 401/403 and
		// route to ErrAuthRequired without duplicating the status string in
		// Error().
		formatted := fmt.Errorf("push failed with status %d: %s", statusErr.StatusCode, statusErr.Body)
		return wrapAuthIfStatusAuth(errors.Join(formatted, statusErr))
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	c.Logger.Debug("metadata pushed to emby", "artist_id", platformArtistID)

	// Emby does not write NFO files immediately after POST /Items/{id}.
	// Trigger a metadata refresh so the on-disk NFO reflects the update.
	// Failure is non-fatal and only logged.
	c.refreshItem(ctx, platformArtistID)

	return nil
}

// fetchAndMergeLockedFields pulls the current LockedFields list from
// Emby for the named artist and returns it with `addition` appended
// (dedup-preserving, PascalCase-canonicalized to match Emby's
// MetadataFields enum). Used by PushMetadata to honor the
// derived-SortName lock contract (#1083) without overwriting per-field
// locks the user set in the Emby UI.
//
// Failure modes (no userID, GET fails, item null) are surfaced as
// errors so the caller can downgrade to "ship without lock" instead of
// silently writing a fresh single-element LockedFields list that would
// clobber every prior lock. An empty fetched list is fine: the
// addition still wins and the body carries it alone.
func (c *Client) fetchAndMergeLockedFields(ctx context.Context, platformArtistID, addition string) ([]string, error) {
	if c.UserID == "" {
		return nil, fmt.Errorf("no user ID configured for this connection")
	}
	// Escape both path segments: Emby user IDs and artist IDs can include
	// characters that would otherwise be misinterpreted as path separators
	// (slashes) or query delimiters (question marks, ampersands). Without
	// url.PathEscape the GET could land on the wrong route and silently
	// degrade the lock merge to "ship without lock" via the not-found
	// branch -- a quiet correctness regression. Mirrors the same fix that
	// already applies to other Emby write paths (see emby/client.go).
	getPath := fmt.Sprintf(
		"/Users/%s/Items/%s?Fields=LockedFields",
		url.PathEscape(c.UserID),
		url.PathEscape(platformArtistID),
	)
	var item map[string]any
	if err := c.Get(ctx, getPath, &item); err != nil {
		return nil, fmt.Errorf("fetching locked fields: %w", err)
	}
	if item == nil {
		return nil, fmt.Errorf("emby returned null item body")
	}
	// Existing entries on the wire are PascalCase already; pass them
	// through the canonicalizer so any non-canonical input (e.g. a
	// historical write from an older Stillwater version) lands on the
	// canonical form. canonicalizeLockedFields drops unmapped entries
	// silently; that matches the existing UpdateArtistLocks contract.
	existing := lockedFieldsFromRaw(item["LockedFields"])
	existing = append(existing, addition)
	return canonicalizeLockedFields(existing), nil
}

// lockedFieldsFromRaw normalizes the LockedFields value Emby returns
// (deserialized as []any from generic JSON decoding) into a []string
// the canonicalizer accepts. Non-string entries are dropped silently.
func lockedFieldsFromRaw(raw any) []string {
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// buildProviderIDs assembles the ProviderIds map for the Emby push payload
// from the platform-agnostic ArtistPushData. Only non-empty IDs are
// included; the map is keyed using Emby's canonical metadata-fetcher names
// (MusicBrainzArtist, TheAudioDb, Discogs, Spotify) so the values land in
// the fields Emby's MetadataFields enum already understands. Returns an
// empty (non-nil) map if no IDs are set; the caller decides whether to
// attach it to the body.
func buildProviderIDs(data connection.ArtistPushData) map[string]string {
	ids := make(map[string]string, 4)
	if data.MusicBrainzID != "" {
		ids["MusicBrainzArtist"] = data.MusicBrainzID
	}
	if data.AudioDBID != "" {
		ids["TheAudioDb"] = data.AudioDBID
	}
	if data.DiscogsID != "" {
		ids["Discogs"] = data.DiscogsID
	}
	if data.SpotifyID != "" {
		ids["Spotify"] = data.SpotifyID
	}
	return ids
}

// refreshItem triggers a metadata refresh for a single item on the Emby server.
// This forces Emby to persist updated metadata to NFO files on disk. The call
// is fire-and-forget: errors are logged but do not fail the parent operation.
func (c *Client) refreshItem(ctx context.Context, platformArtistID string) {
	path := fmt.Sprintf("/Items/%s/Refresh", url.PathEscape(platformArtistID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		connection.BuildRequestURL(c.BaseURL, path+"?ReplaceAllMetadata=false&ReplaceAllImages=false"), http.NoBody)
	if err != nil {
		c.Logger.Warn("creating emby refresh request", "artist_id", platformArtistID, "error", err)
		return
	}
	c.AuthFunc(req)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		c.Logger.Warn("emby refresh request failed", "artist_id", platformArtistID, "error", err)
		return
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 300 {
		c.Logger.Warn("emby refresh returned error", "artist_id", platformArtistID, "status", resp.StatusCode)
	}
}

// UploadImage uploads an image to the Emby server for the given artist.
// POST /Items/{id}/Images/{type}
func (c *Client) UploadImage(ctx context.Context, platformArtistID string, imageType string, data []byte, contentType string) error {
	embyType := mapImageType(imageType)
	if embyType == "" {
		return fmt.Errorf("unsupported image type: %s", imageType)
	}
	return mediabrowser.UploadImageRaw(ctx, c.Client, c.Logger, "emby", url.PathEscape(platformArtistID), embyType, imageType, data, contentType, wrapAuthIfStatusAuth)
}

// UploadImageAtIndex uploads an image at a specific index to the Emby server.
// POST /Items/{id}/Images/{type}/{index}
// This is used for backdrop images where Emby supports multiple images per artist.
func (c *Client) UploadImageAtIndex(ctx context.Context, platformArtistID string, imageType string, index int, data []byte, contentType string) error {
	embyType := mapImageType(imageType)
	return mediabrowser.UploadImageAtIndexRaw(ctx, c.Client, c.Logger, "emby", url.PathEscape(platformArtistID), embyType, imageType, index, data, contentType, wrapAuthIfStatusAuth)
}

// DeleteImage deletes an image from the Emby server for the given artist.
// DELETE /Items/{id}/Images/{type}
func (c *Client) DeleteImage(ctx context.Context, platformArtistID string, imageType string) error {
	embyType := mapImageType(imageType)
	return mediabrowser.DeleteImageRaw(ctx, c.Client, c.Logger, "emby", url.PathEscape(platformArtistID), embyType, imageType, wrapAuthIfStatusAuth)
}

// DeleteImageAtIndex deletes the image at a specific index for the given
// artist. DELETE /Items/{id}/Images/{type}/{index}. Used to prune redundant
// backdrops on the platform (#2540 remote prune). Emby re-indexes remaining
// backdrops after a delete, so callers pruning multiple indices MUST delete
// high-index-first.
func (c *Client) DeleteImageAtIndex(ctx context.Context, platformArtistID string, imageType string, index int) error {
	embyType := mapImageType(imageType)
	return mediabrowser.DeleteImageAtIndexRaw(ctx, c.Client, c.Logger, "emby", url.PathEscape(platformArtistID), embyType, imageType, index, wrapAuthIfStatusAuth)
}

// logDateNormalization logs the result of normalizing a date field for push.
func (c *Client) logDateNormalization(field, raw, normalized, artistID string) {
	if normalized == "" {
		c.Logger.Warn("unparsable date dropped from push",
			"field", field, "raw", raw, "artist_id", artistID)
	} else if normalized != raw {
		c.Logger.Debug("date normalized for push",
			"field", field, "raw", raw, "normalized", normalized, "artist_id", artistID)
	}
}

// mapImageType converts a Stillwater image type to an Emby image type.
func mapImageType(imageType string) string {
	switch imageType {
	case "thumb":
		return "Primary"
	case "fanart":
		return "Backdrop"
	case "logo":
		return "Logo"
	case "banner":
		return "Banner"
	default:
		return ""
	}
}
