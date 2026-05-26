package jellyfin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/httpclient"
)

// readBoundedStatusError reads a bounded snippet of the peer error body and
// returns a typed httpclient.StatusError. Used by every hand-rolled HTTP
// path in this file so write-method errors carry the status code for
// wrapAuthIfStatusAuth (errors.As + ErrAuthRequired wrap on 401/403).
func readBoundedStatusError(resp *http.Response) *httpclient.StatusError {
	const maxErrBody = 1 << 20 // 1 MB
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
	_, _ = io.Copy(io.Discard, resp.Body)
	return &httpclient.StatusError{StatusCode: resp.StatusCode, Body: string(respBody)}
}

// PushMetadata updates metadata for an artist item on the Jellyfin server.
//
// Jellyfin's POST /Items/{id} requires the full item body; partial updates
// with only the changed fields return 400. This method fetches the current
// item, merges Stillwater's fields into it, strips read-only fields that
// Jellyfin rejects, and POSTs the complete body back.
func (c *Client) PushMetadata(ctx context.Context, platformArtistID string, data connection.ArtistPushData) error {
	// Fetch the current item from Jellyfin to get the full body.
	existing, err := c.fetchItem(ctx, platformArtistID)
	if err != nil {
		return fmt.Errorf("fetching current item for merge: %w", err)
	}

	// Merge Stillwater's fields into the existing item. Empty values are
	// written unconditionally so that field clears in Stillwater propagate
	// to Jellyfin (the fetch-merge pattern preserves all other fields).
	existing["Name"] = data.Name
	existing["Overview"] = data.Biography
	existing["ForcedSortName"] = data.SortName
	existing["Genres"] = append([]string{}, data.Genres...)

	// Styles and Moods map to Tags (flat string array on Jellyfin).
	tags := append([]string{}, data.Styles...)
	tags = append(tags, data.Moods...)
	existing["Tags"] = tags

	// Merge every external ID Stillwater has into ProviderIds, preserving any
	// IDs Jellyfin already manages (the fetched JSON deserializes inner maps
	// as map[string]any). Empty IDs are not written so a clear on the
	// Stillwater side does not silently overwrite a Jellyfin-side value;
	// explicit ID removal lives outside this push path.
	providerUpdates := buildProviderIDUpdates(data)
	if len(providerUpdates) > 0 {
		// Capture the type-assertion ok bool so a malformed fetch (Jellyfin
		// returning ProviderIds as something other than a JSON object) is
		// surfaced as an error instead of silently allocating a fresh map
		// and wiping every Jellyfin-managed ID.
		existingProviderIds := existing["ProviderIds"]
		var providerIds map[string]any
		if existingProviderIds == nil {
			providerIds = make(map[string]any, len(providerUpdates))
		} else {
			var ok bool
			providerIds, ok = existingProviderIds.(map[string]any)
			if !ok {
				return fmt.Errorf("jellyfin returned ProviderIds in an unexpected shape (%T); refusing to overwrite", existingProviderIds)
			}
		}
		for k, v := range providerUpdates {
			providerIds[k] = v
		}
		existing["ProviderIds"] = providerIds
	}

	// Map band members into Jellyfin's People array. Each entry becomes a
	// PersonInfo with Type=Person and a Role summarizing vocal type +
	// instruments. The write is gated on a non-nil BandMembers slice, which
	// only happens when the caller actually fetched members AND at least one
	// has a non-empty name; an empty Stillwater member list does NOT clear
	// the Jellyfin-side People array (same no-clobber invariant as
	// ProviderIds above -- explicit removal lives outside this push path).
	if data.BandMembers != nil {
		existing["People"] = buildPeopleEntries(data.BandMembers)
	}

	// Normalize dates to yyyy-MM-dd so Jellyfin does not silently discard.
	// Only set when normalization succeeds; an empty result would overwrite a
	// valid existing date with "" since the map-based merge has no omitempty.
	// The default branch clears the date when all source fields are empty,
	// ensuring that date clears in Stillwater propagate to Jellyfin.
	switch {
	case data.Born != "":
		normalized := connection.NormalizeDateForPlatform(data.Born)
		c.logDateNormalization("premiere_date", data.Born, normalized, platformArtistID)
		if normalized != "" {
			existing["PremiereDate"] = normalized
		}
	case data.Formed != "":
		normalized := connection.NormalizeDateForPlatform(data.Formed)
		c.logDateNormalization("premiere_date", data.Formed, normalized, platformArtistID)
		if normalized != "" {
			existing["PremiereDate"] = normalized
		}
	default:
		existing["PremiereDate"] = ""
	}
	switch {
	case data.Died != "":
		normalized := connection.NormalizeDateForPlatform(data.Died)
		c.logDateNormalization("end_date", data.Died, normalized, platformArtistID)
		if normalized != "" {
			existing["EndDate"] = normalized
		}
	case data.Disbanded != "":
		normalized := connection.NormalizeDateForPlatform(data.Disbanded)
		c.logDateNormalization("end_date", data.Disbanded, normalized, platformArtistID)
		if normalized != "" {
			existing["EndDate"] = normalized
		}
	default:
		existing["EndDate"] = ""
	}

	if err := c.postFullItem(ctx, platformArtistID, existing, "push"); err != nil {
		return err
	}

	c.Logger.Debug("metadata pushed to jellyfin", "artist_id", platformArtistID)
	return nil
}

// buildProviderIDUpdates builds the set of external-ID key/value pairs
// Stillwater wants merged into Jellyfin's ProviderIds map. Empty IDs are
// omitted so a missing-in-Stillwater value does not overwrite an
// existing-in-Jellyfin value. Key naming matches Jellyfin's metadata
// fetcher conventions: MusicBrainzArtist, TheAudioDb, Discogs, Spotify.
func buildProviderIDUpdates(data connection.ArtistPushData) map[string]string {
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

// buildPeopleEntries maps Stillwater's band members into Jellyfin's People
// array shape. Each entry is a map[string]any (matching how the fetched
// item body deserializes) with Name, Role, and Type=Person; Jellyfin
// treats Type=Person as a generic credited contributor, which is the
// closest existing match for a band member.
func buildPeopleEntries(members []connection.ArtistPersonRef) []map[string]any {
	out := make([]map[string]any, 0, len(members))
	for _, m := range members {
		if m.Name == "" {
			continue
		}
		entry := map[string]any{
			"Name": m.Name,
			"Type": "Person",
		}
		if m.Role != "" {
			entry["Role"] = m.Role
		}
		out = append(out, entry)
	}
	return out
}

// UpdateArtistLocks persists the overall lock state for the given Jellyfin
// artist without touching content metadata. Jellyfin does NOT honor per-field
// LockedFields at the item level, so this method only syncs the whole-item
// LockData flag; the lockedFields argument is accepted for interface
// conformance and logged at debug but otherwise discarded. The item POST is a
// full replacement, so this method fetches the current item map, overwrites
// LockData, strips read-only fields, and POSTs the merged body back.
// Keeping lock sync on its own HTTP cycle (rather than piggybacking on
// PushMetadata) prevents accidental re-scrapes when LockData toggles through
// the normal metadata push path.
func (c *Client) UpdateArtistLocks(ctx context.Context, platformArtistID string, lockData bool, lockedFields []string) error {
	existing, err := c.fetchItem(ctx, platformArtistID)
	if err != nil {
		return fmt.Errorf("fetching artist for lock update: %w", err)
	}
	existing["LockData"] = lockData
	if len(lockedFields) > 0 {
		c.Logger.Debug("jellyfin: per-field locks ignored (not supported at item level)",
			"artist_id", platformArtistID, "field_count", len(lockedFields))
	}

	return c.postFullItem(ctx, platformArtistID, existing, "lock update")
}

// postFullItem strips read-only fields from item, marshals it, and POSTs the
// full body to /Items/{platformArtistID}. Jellyfin's POST /Items/{id} requires
// a complete item body (not a delta), so both PushMetadata and
// UpdateArtistLocks share this request/response cycle. The op label appears in
// error messages so callers can distinguish failures (e.g. "push failed with
// status 500", "lock update failed with status 500") without each call site
// re-implementing the request boilerplate.
func (c *Client) postFullItem(ctx context.Context, platformArtistID string, item map[string]any, op string) error {
	// Strip read-only fields that Jellyfin rejects in a POST. Done here (not
	// at each call site) so a future addition to jellyfinReadOnlyFields cannot
	// silently slip through one path while protecting the other.
	//
	// Operate on a shallow copy so callers that retain `item` after this call
	// (for example to log it on error or pass it to a retry) see their
	// original map unchanged.
	cleanItem := make(map[string]any, len(item))
	for k, v := range item {
		cleanItem[k] = v
	}
	for _, key := range jellyfinReadOnlyFields {
		delete(cleanItem, key)
	}

	payload, err := json.Marshal(cleanItem)
	if err != nil {
		return fmt.Errorf("marshaling %s body: %w", op, err)
	}

	path := fmt.Sprintf("/Items/%s", url.PathEscape(platformArtistID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, connection.BuildRequestURL(c.BaseURL, path), bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("creating %s request: %w", op, err)
	}
	c.AuthFunc(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("executing %s request: %w", op, err)
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup

	if resp.StatusCode >= 300 {
		statusErr := readBoundedStatusError(resp)
		formatted := fmt.Errorf("%s failed with status %d: %s", op, statusErr.StatusCode, statusErr.Body)
		return wrapAuthIfStatusAuth(errors.Join(formatted, statusErr))
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// jellyfinReadOnlyFields are item fields that Jellyfin rejects in POST
// /Items/{id}. These must be stripped from the GET response before POSTing.
var jellyfinReadOnlyFields = []string{
	"ServerId", "ImageBlurHashes", "ImageTags", "BackdropImageTags",
	"LocationType", "MediaType", "ChannelId",
}

// fetchItem retrieves a single item from Jellyfin by ID, returning the
// full item body as a generic map. Uses /Items?Ids={id}&Fields=... to
// ensure metadata fields are populated. LockData and LockedFields are NOT
// returned by default on Jellyfin's item endpoints; they must be listed
// explicitly or the subsequent full-replacement POST from UpdateArtistLocks
// / PushMetadata will silently clear server-side locks.
func (c *Client) fetchItem(ctx context.Context, itemID string) (map[string]any, error) {
	// Empty or whitespace-only itemID would build "Ids=" which Jellyfin
	// accepts and returns the library's first item. Returning the wrong
	// artist here corrupts every downstream write (including lock state),
	// so reject at the boundary before issuing the request.
	if strings.TrimSpace(itemID) == "" {
		return nil, fmt.Errorf("item id is required")
	}
	path := fmt.Sprintf("/Items?Ids=%s&Fields=Overview,ProviderIds,PremiereDate,EndDate,Genres,Tags,LockData,LockedFields", url.QueryEscape(itemID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, connection.BuildRequestURL(c.BaseURL, path), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("creating fetch request: %w", err)
	}
	c.AuthFunc(req)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing fetch request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup

	if resp.StatusCode >= 300 {
		statusErr := readBoundedStatusError(resp)
		formatted := fmt.Errorf("fetch failed with status %d: %s", statusErr.StatusCode, statusErr.Body)
		return nil, wrapAuthIfStatusAuth(errors.Join(formatted, statusErr))
	}

	var result struct {
		Items []map[string]any `json:"Items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding fetch response: %w", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	if len(result.Items) == 0 {
		return nil, fmt.Errorf("item %s not found", itemID)
	}
	// Jellyfin can legitimately return a null Items[0] for tombstoned or
	// access-denied records. Treat it as "not found" rather than returning
	// a nil map that would panic on the caller's first write.
	if result.Items[0] == nil {
		return nil, fmt.Errorf("item %s returned null payload", itemID)
	}
	return result.Items[0], nil
}

// UploadImage uploads an image to the Jellyfin server for the given artist.
// POST /Items/{id}/Images/{type}
func (c *Client) UploadImage(ctx context.Context, platformArtistID string, imageType string, data []byte, contentType string) error {
	jfType := mapImageType(imageType)
	if jfType == "" {
		return fmt.Errorf("unsupported image type: %s", imageType)
	}

	// Jellyfin 10.x expects the image body to be base64-encoded plain text.
	// The Content-Type header still declares the image format (image/jpeg or
	// image/png); Jellyfin uses it to determine the save format after decoding.
	encoded := base64.StdEncoding.EncodeToString(data)

	path := fmt.Sprintf("/Items/%s/Images/%s", url.PathEscape(platformArtistID), jfType)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, connection.BuildRequestURL(c.BaseURL, path), strings.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("creating image upload request: %w", err)
	}
	c.AuthFunc(req)
	req.Header.Set("Content-Type", contentType)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("executing image upload: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup

	if resp.StatusCode >= 300 {
		statusErr := readBoundedStatusError(resp)
		formatted := fmt.Errorf("image upload failed with status %d: %s", statusErr.StatusCode, statusErr.Body)
		return wrapAuthIfStatusAuth(errors.Join(formatted, statusErr))
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	c.Logger.Debug("image uploaded to jellyfin", "artist_id", platformArtistID, "type", jfType)
	return nil
}

// UploadImageAtIndex uploads an image at a specific index to the Jellyfin server.
// POST /Items/{id}/Images/{type}/{index}
// This is used for backdrop images where Jellyfin supports multiple images per artist.
func (c *Client) UploadImageAtIndex(ctx context.Context, platformArtistID string, imageType string, index int, data []byte, contentType string) error {
	if index < 0 {
		return fmt.Errorf("invalid image index: %d", index)
	}
	jfType := mapImageType(imageType)
	if jfType == "" {
		return fmt.Errorf("unsupported image type: %s", imageType)
	}

	encoded := base64.StdEncoding.EncodeToString(data)

	path := fmt.Sprintf("/Items/%s/Images/%s/%d", url.PathEscape(platformArtistID), jfType, index)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, connection.BuildRequestURL(c.BaseURL, path), strings.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("creating indexed image upload request: %w", err)
	}
	c.AuthFunc(req)
	req.Header.Set("Content-Type", contentType)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("executing indexed image upload: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup

	if resp.StatusCode >= 300 {
		statusErr := readBoundedStatusError(resp)
		formatted := fmt.Errorf("indexed image upload failed with status %d: %s", statusErr.StatusCode, statusErr.Body)
		return wrapAuthIfStatusAuth(errors.Join(formatted, statusErr))
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	c.Logger.Debug("image uploaded to jellyfin at index", "artist_id", platformArtistID, "type", jfType, "index", index)
	return nil
}

// DeleteImage deletes an image from the Jellyfin server for the given artist.
// DELETE /Items/{id}/Images/{type}
func (c *Client) DeleteImage(ctx context.Context, platformArtistID string, imageType string) error {
	jfType := mapImageType(imageType)
	if jfType == "" {
		return fmt.Errorf("unsupported image type: %s", imageType)
	}

	path := fmt.Sprintf("/Items/%s/Images/%s", url.PathEscape(platformArtistID), jfType)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, connection.BuildRequestURL(c.BaseURL, path), http.NoBody)
	if err != nil {
		return fmt.Errorf("creating image delete request: %w", err)
	}
	c.AuthFunc(req)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("executing image delete: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup

	if resp.StatusCode >= 300 {
		statusErr := readBoundedStatusError(resp)
		formatted := fmt.Errorf("image delete failed with status %d: %s", statusErr.StatusCode, statusErr.Body)
		return wrapAuthIfStatusAuth(errors.Join(formatted, statusErr))
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	c.Logger.Debug("image deleted from jellyfin", "artist_id", platformArtistID, "type", jfType)
	return nil
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

// mapImageType converts a Stillwater image type to a Jellyfin image type.
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
