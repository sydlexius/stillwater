// Package imagebridge resolves Stillwater artist IDs to platform-specific
// clients (Emby, Jellyfin) and delegates image fetch/upload operations. It
// lives outside internal/connection to avoid an import cycle: connection/emby
// and connection/jellyfin both import connection, so connection itself cannot
// import them.
package imagebridge

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
)

// ArtistPlatformIDProvider is the subset of artist.Service needed to resolve
// platform IDs for an artist. Using a narrow interface avoids a circular
// dependency on the full artist service.
type ArtistPlatformIDProvider interface {
	GetPlatformIDs(ctx context.Context, artistID string) ([]artist.PlatformID, error)
}

// Bridge fetches and uploads artist images through configured platform
// connections (Emby, Jellyfin). It resolves the Stillwater artist ID to
// platform-specific IDs and delegates to the appropriate client.
type Bridge struct {
	connService   *connection.Service
	artistService ArtistPlatformIDProvider
	logger        *slog.Logger
}

// New creates a Bridge.
func New(connService *connection.Service, artistService ArtistPlatformIDProvider, logger *slog.Logger) *Bridge {
	return &Bridge{
		connService:   connService,
		artistService: artistService,
		logger:        logger.With(slog.String("component", "image-bridge")),
	}
}

// FetchArtistImage downloads the image bytes for the given Stillwater artist ID
// and image type from the first responsive platform connection. It iterates
// through all platform IDs for the artist, builds the appropriate client for
// each connection, and returns the first successful result.
func (b *Bridge) FetchArtistImage(ctx context.Context, artistID, imageType string) ([]byte, string, error) {
	platformIDs, err := b.artistService.GetPlatformIDs(ctx, artistID)
	if err != nil {
		return nil, "", fmt.Errorf("resolving platform IDs for artist %s: %w", artistID, err)
	}
	if len(platformIDs) == 0 {
		return nil, "", fmt.Errorf("artist %s has no platform ID mappings", artistID)
	}

	var lastErr error
	for _, pid := range platformIDs {
		conn, connErr := b.connService.GetByID(ctx, pid.ConnectionID)
		if connErr != nil {
			b.logger.Debug("skipping connection: lookup failed",
				slog.String("connection_id", pid.ConnectionID),
				slog.String("error", connErr.Error()))
			lastErr = connErr
			continue
		}
		if !conn.Enabled {
			continue
		}

		data, contentType, fetchErr := b.fetchFromConnection(ctx, conn, pid.PlatformArtistID, imageType)
		if fetchErr != nil {
			b.logger.Debug("image fetch failed for connection",
				slog.String("connection_id", conn.ID),
				slog.String("type", conn.Type),
				slog.String("error", fetchErr.Error()))
			lastErr = fetchErr
			continue
		}
		return data, contentType, nil
	}

	if lastErr != nil {
		return nil, "", fmt.Errorf("all platform connections failed; last error: %w", lastErr)
	}
	return nil, "", fmt.Errorf("no enabled platform connections for artist %s", artistID)
}

// UploadArtistImage pushes image bytes to all enabled platform connections with
// image_write capability for the given Stillwater artist ID and image type.
// Returns the first error encountered, or nil if all uploads succeed.
func (b *Bridge) UploadArtistImage(ctx context.Context, artistID, imageType string, data []byte, contentType string) error {
	platformIDs, err := b.artistService.GetPlatformIDs(ctx, artistID)
	if err != nil {
		return fmt.Errorf("resolving platform IDs for artist %s: %w", artistID, err)
	}
	if len(platformIDs) == 0 {
		return fmt.Errorf("artist %s has no platform ID mappings", artistID)
	}

	var uploaded int
	var lastErr error
	for _, pid := range platformIDs {
		conn, connErr := b.connService.GetByID(ctx, pid.ConnectionID)
		if connErr != nil {
			b.logger.Debug("skipping connection: lookup failed",
				slog.String("connection_id", pid.ConnectionID),
				slog.String("error", connErr.Error()))
			lastErr = connErr
			continue
		}
		if !conn.Enabled || !conn.FeatureImageWrite {
			continue
		}

		if uploadErr := b.uploadToConnection(ctx, conn, pid.PlatformArtistID, imageType, data, contentType); uploadErr != nil {
			b.logger.Warn("image upload failed for connection",
				slog.String("connection_id", conn.ID),
				slog.String("type", conn.Type),
				slog.String("error", uploadErr.Error()))
			lastErr = uploadErr
			continue
		}
		uploaded++
	}

	if uploaded == 0 {
		if lastErr != nil {
			return fmt.Errorf("all platform uploads failed; last error: %w", lastErr)
		}
		return fmt.Errorf("no enabled platform connections with image_write for artist %s", artistID)
	}
	if lastErr != nil {
		return fmt.Errorf("partial upload failure (%d succeeded); last error: %w", uploaded, lastErr)
	}
	return nil
}

// fetchFromConnection builds the appropriate platform client and fetches the image.
func (b *Bridge) fetchFromConnection(ctx context.Context, conn *connection.Connection, platformArtistID, imageType string) ([]byte, string, error) {
	switch conn.Type {
	case connection.TypeEmby:
		c := emby.New(conn.URL, conn.APIKey, conn.PlatformUserID, b.logger)
		return c.GetArtistImage(ctx, platformArtistID, imageType)
	case connection.TypeJellyfin:
		c := jellyfin.New(conn.URL, conn.APIKey, conn.PlatformUserID, b.logger)
		return c.GetArtistImage(ctx, platformArtistID, imageType)
	default:
		return nil, "", fmt.Errorf("connection type %q does not support image fetch", conn.Type)
	}
}

// uploadToConnection builds the appropriate platform client and uploads the image.
func (b *Bridge) uploadToConnection(ctx context.Context, conn *connection.Connection, platformArtistID, imageType string, data []byte, contentType string) error {
	switch conn.Type {
	case connection.TypeEmby:
		c := emby.New(conn.URL, conn.APIKey, conn.PlatformUserID, b.logger)
		return c.UploadImage(ctx, platformArtistID, imageType, data, contentType)
	case connection.TypeJellyfin:
		c := jellyfin.New(conn.URL, conn.APIKey, conn.PlatformUserID, b.logger)
		return c.UploadImage(ctx, platformArtistID, imageType, data, contentType)
	default:
		return fmt.Errorf("connection type %q does not support image upload", conn.Type)
	}
}
