package jellyfin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/sydlexius/stillwater/internal/connection"
)

// itemUpdateBody is the payload for POST /Items/{id}.
type itemUpdateBody struct {
	Name           string            `json:"Name"`
	ForcedSortName string            `json:"ForcedSortName,omitempty"`
	Overview       string            `json:"Overview,omitempty"`
	Genres         []string          `json:"Genres,omitempty"`
	Tags           []string          `json:"Tags,omitempty"`
	ProviderIds    map[string]string `json:"ProviderIds,omitempty"`
	PremiereDate   string            `json:"PremiereDate,omitempty"`
	EndDate        string            `json:"EndDate,omitempty"`
}

// PushMetadata updates metadata for an artist item on the Jellyfin server.
func (c *Client) PushMetadata(ctx context.Context, platformArtistID string, data connection.ArtistPushData) error {
	// Styles map to Tags; Moods are appended as additional tags since
	// Jellyfin has no dedicated moods field. Disambiguation and YearsActive
	// have no corresponding Jellyfin fields and are omitted.
	tags := append([]string{}, data.Styles...)
	tags = append(tags, data.Moods...)
	body := itemUpdateBody{
		Name:     data.Name,
		Overview: data.Biography,
		Genres:   data.Genres,
		Tags:     tags,
	}
	if data.SortName != "" {
		body.ForcedSortName = data.SortName
	}
	if data.MusicBrainzID != "" {
		body.ProviderIds = map[string]string{
			"MusicBrainzArtist": data.MusicBrainzID,
		}
	}
	if data.Born != "" {
		body.PremiereDate = data.Born
	} else if data.Formed != "" {
		body.PremiereDate = data.Formed
	}
	if data.Died != "" {
		body.EndDate = data.Died
	} else if data.Disbanded != "" {
		body.EndDate = data.Disbanded
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshaling push body: %w", err)
	}

	path := fmt.Sprintf("/Items/%s", platformArtistID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("creating push request: %w", err)
	}
	c.setAuth(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req) //nolint:gosec // URL constructed from trusted base + artist ID
	if err != nil {
		return fmt.Errorf("executing push request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("push failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	c.logger.Debug("metadata pushed to jellyfin", "artist_id", platformArtistID)
	return nil
}

// UploadImage uploads an image to the Jellyfin server for the given artist.
// POST /Items/{id}/Images/{type}
func (c *Client) UploadImage(ctx context.Context, platformArtistID string, imageType string, data []byte, contentType string) error {
	jfType := mapImageType(imageType)
	if jfType == "" {
		return fmt.Errorf("unsupported image type: %s", imageType)
	}

	path := fmt.Sprintf("/Items/%s/Images/%s", platformArtistID, jfType)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("creating image upload request: %w", err)
	}
	c.setAuth(req)
	req.Header.Set("Content-Type", contentType)

	resp, err := c.httpClient.Do(req) //nolint:gosec // URL constructed from trusted base + artist ID
	if err != nil {
		return fmt.Errorf("executing image upload: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode >= 300 {
		const maxErrBody = 1 << 20 // 1 MB
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		return fmt.Errorf("image upload failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	c.logger.Debug("image uploaded to jellyfin", "artist_id", platformArtistID, "type", jfType)
	return nil
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
