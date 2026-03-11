package emby

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
}

// PushMetadata updates metadata for an artist item on the Emby server.
func (c *Client) PushMetadata(ctx context.Context, platformArtistID string, data connection.ArtistPushData) error {
	// Styles and Moods map to TagItems (Emby uses {Name} objects, not flat
	// strings). Disambiguation and YearsActive have no corresponding Emby
	// fields and are omitted.
	var items []tagItem
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
	if data.MusicBrainzID != "" {
		body.ProviderIds = map[string]string{
			"MusicBrainzArtist": data.MusicBrainzID,
		}
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
	c.Logger.Debug("metadata pushed to emby", "artist_id", platformArtistID)

	// Emby does not write NFO files immediately after POST /Items/{id}.
	// Trigger a metadata refresh so the on-disk NFO reflects the update.
	// Failure is non-fatal and only logged.
	c.refreshItem(ctx, platformArtistID)

	return nil
}

// refreshItem triggers a metadata refresh for a single item on the Emby server.
// This forces Emby to persist updated metadata to NFO files on disk. The call
// is fire-and-forget: errors are logged but do not fail the parent operation.
func (c *Client) refreshItem(ctx context.Context, platformArtistID string) {
	path := fmt.Sprintf("/Items/%s/Refresh", url.PathEscape(platformArtistID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		connection.BuildRequestURL(c.BaseURL, path+"?ReplaceAllMetadata=false&ReplaceAllImages=false"), nil)
	if err != nil {
		c.Logger.Warn("creating emby refresh request", "artist_id", platformArtistID, "error", err)
		return
	}
	c.AuthFunc(req)

	resp, err := c.HTTPClient.Do(req) //nolint:gosec // URL constructed from trusted base + artist ID
	if err != nil {
		c.Logger.Warn("emby refresh request failed", "artist_id", platformArtistID, "error", err)
		return
	}
	defer resp.Body.Close() //nolint:errcheck
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

	// Emby expects the image body to be base64-encoded plain text, identical to
	// the Jellyfin API contract. The Content-Type header still declares the image
	// format; Emby uses it to determine the save format after decoding.
	encoded := base64.StdEncoding.EncodeToString(data)

	path := fmt.Sprintf("/Items/%s/Images/%s", url.PathEscape(platformArtistID), embyType)
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
	c.Logger.Debug("image uploaded to emby", "artist_id", platformArtistID, "type", embyType)
	return nil
}

// UploadImageAtIndex uploads an image at a specific index to the Emby server.
// POST /Items/{id}/Images/{type}/{index}
// This is used for backdrop images where Emby supports multiple images per artist.
func (c *Client) UploadImageAtIndex(ctx context.Context, platformArtistID string, imageType string, index int, data []byte, contentType string) error {
	if index < 0 {
		return fmt.Errorf("invalid image index: %d", index)
	}
	embyType := mapImageType(imageType)
	if embyType == "" {
		return fmt.Errorf("unsupported image type: %s", imageType)
	}

	encoded := base64.StdEncoding.EncodeToString(data)

	path := fmt.Sprintf("/Items/%s/Images/%s/%d", url.PathEscape(platformArtistID), embyType, index)
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
	c.Logger.Debug("image uploaded to emby at index", "artist_id", platformArtistID, "type", embyType, "index", index)
	return nil
}

// DeleteImage deletes an image from the Emby server for the given artist.
// DELETE /Items/{id}/Images/{type}
func (c *Client) DeleteImage(ctx context.Context, platformArtistID string, imageType string) error {
	embyType := mapImageType(imageType)
	if embyType == "" {
		return fmt.Errorf("unsupported image type: %s", imageType)
	}

	path := fmt.Sprintf("/Items/%s/Images/%s", url.PathEscape(platformArtistID), embyType)
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
	c.Logger.Debug("image deleted from emby", "artist_id", platformArtistID, "type", embyType)
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
