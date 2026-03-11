package jellyfin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/sydlexius/stillwater/internal/connection"
)

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

	// Merge Stillwater's fields into the existing item.
	existing["Name"] = data.Name
	if data.Biography != "" {
		existing["Overview"] = data.Biography
	}
	if data.SortName != "" {
		existing["ForcedSortName"] = data.SortName
	}
	if len(data.Genres) > 0 {
		existing["Genres"] = data.Genres
	}

	// Styles and Moods map to Tags (flat string array on Jellyfin).
	tags := append([]string{}, data.Styles...)
	tags = append(tags, data.Moods...)
	if len(tags) > 0 {
		existing["Tags"] = tags
	}

	if data.MusicBrainzID != "" {
		// Merge into existing ProviderIds to preserve IDs managed by Jellyfin
		// (e.g. TheAudioDb, Discogs). The fetched JSON deserializes inner maps
		// as map[string]any, not map[string]string.
		providerIds, _ := existing["ProviderIds"].(map[string]any)
		if providerIds == nil {
			providerIds = make(map[string]any)
		}
		providerIds["MusicBrainzArtist"] = data.MusicBrainzID
		existing["ProviderIds"] = providerIds
	}

	// Normalize dates to yyyy-MM-dd so Jellyfin does not silently discard.
	// Only set when normalization succeeds; an empty result would overwrite a
	// valid existing date with "" since the map-based merge has no omitempty.
	if raw := data.Born; raw != "" {
		normalized := connection.NormalizeDateForPlatform(raw)
		c.logDateNormalization("premiere_date", raw, normalized, platformArtistID)
		if normalized != "" {
			existing["PremiereDate"] = normalized
		}
	} else if raw := data.Formed; raw != "" {
		normalized := connection.NormalizeDateForPlatform(raw)
		c.logDateNormalization("premiere_date", raw, normalized, platformArtistID)
		if normalized != "" {
			existing["PremiereDate"] = normalized
		}
	}
	if raw := data.Died; raw != "" {
		normalized := connection.NormalizeDateForPlatform(raw)
		c.logDateNormalization("end_date", raw, normalized, platformArtistID)
		if normalized != "" {
			existing["EndDate"] = normalized
		}
	} else if raw := data.Disbanded; raw != "" {
		normalized := connection.NormalizeDateForPlatform(raw)
		c.logDateNormalization("end_date", raw, normalized, platformArtistID)
		if normalized != "" {
			existing["EndDate"] = normalized
		}
	}

	// Strip read-only fields that Jellyfin rejects in a POST.
	for _, key := range jellyfinReadOnlyFields {
		delete(existing, key)
	}

	payload, err := json.Marshal(existing)
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

	resp, err := c.HTTPClient.Do(req) //nolint:gosec // URL constructed from trusted base + artist ID
	if err != nil {
		return fmt.Errorf("executing push request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode >= 300 {
		const maxErrBody = 1 << 20 // 1 MB
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		_, _ = io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("push failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	c.Logger.Debug("metadata pushed to jellyfin", "artist_id", platformArtistID)
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
// ensure metadata fields are populated.
func (c *Client) fetchItem(ctx context.Context, itemID string) (map[string]any, error) {
	path := fmt.Sprintf("/Items?Ids=%s&Fields=Overview,ProviderIds,PremiereDate,EndDate,Genres,Tags", url.QueryEscape(itemID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, connection.BuildRequestURL(c.BaseURL, path), nil)
	if err != nil {
		return nil, fmt.Errorf("creating fetch request: %w", err)
	}
	c.AuthFunc(req)

	resp, err := c.HTTPClient.Do(req) //nolint:gosec // URL constructed from trusted base + item ID
	if err != nil {
		return nil, fmt.Errorf("executing fetch request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode >= 300 {
		const maxErrBody = 1 << 20
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("fetch failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Items []map[string]any `json:"Items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding fetch response: %w", err)
	}
	if len(result.Items) == 0 {
		return nil, fmt.Errorf("item %s not found", itemID)
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

	resp, err := c.HTTPClient.Do(req) //nolint:gosec // URL constructed from trusted base + artist ID
	if err != nil {
		return fmt.Errorf("executing image upload: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode >= 300 {
		const maxErrBody = 1 << 20 // 1 MB
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		_, _ = io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("image upload failed with status %d: %s", resp.StatusCode, string(respBody))
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

	resp, err := c.HTTPClient.Do(req) //nolint:gosec // URL constructed from trusted base + artist ID
	if err != nil {
		return fmt.Errorf("executing indexed image upload: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode >= 300 {
		const maxErrBody = 1 << 20 // 1 MB
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		_, _ = io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("indexed image upload failed with status %d: %s", resp.StatusCode, string(respBody))
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
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, connection.BuildRequestURL(c.BaseURL, path), nil)
	if err != nil {
		return fmt.Errorf("creating image delete request: %w", err)
	}
	c.AuthFunc(req)

	resp, err := c.HTTPClient.Do(req) //nolint:gosec // URL constructed from trusted base + artist ID
	if err != nil {
		return fmt.Errorf("executing image delete: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode >= 300 {
		const maxErrBody = 1 << 20 // 1 MB
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		_, _ = io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("image delete failed with status %d: %s", resp.StatusCode, string(respBody))
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
