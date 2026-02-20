package emby

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/sydlexius/stillwater/internal/connection"
)

// itemUpdateBody is the payload for PUT /Items/{id}.
type itemUpdateBody struct {
	Name           string   `json:"Name"`
	ForcedSortName string   `json:"ForcedSortName,omitempty"`
	Overview       string   `json:"Overview,omitempty"`
	Genres         []string `json:"Genres,omitempty"`
}

// PushMetadata updates metadata for an artist item on the Emby server.
func (c *Client) PushMetadata(ctx context.Context, platformArtistID string, data connection.ArtistPushData) error {
	body := itemUpdateBody{
		Name:     data.Name,
		Overview: data.Biography,
		Genres:   data.Genres,
	}
	if data.SortName != "" {
		body.ForcedSortName = data.SortName
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

	c.logger.Debug("metadata pushed to emby", "artist_id", platformArtistID)
	return nil
}
