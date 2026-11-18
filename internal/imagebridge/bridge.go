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
	"slices"
	"strings"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
	"github.com/sydlexius/stillwater/internal/logging"
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
		logger:        logging.WithComponent(logger, "image-bridge"),
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

// FetchArtistImageSlot downloads one slot of an artist image from the artist's
// pinned connection (pinnedConnection), with no fall-through: an image held only
// elsewhere is never hashed (a missed hash, never a wrong one). Only slot 0 is
// served (indexed fanart is #3602). Pure transport.
func (b *Bridge) FetchArtistImageSlot(ctx context.Context, artistID, imageType string, slotIndex int) ([]byte, string, error) {
	if slotIndex != 0 {
		return nil, "", fmt.Errorf("image type %q: slot %d is not fetchable (only slot 0)", imageType, slotIndex)
	}
	conn, platformArtistID, err := b.pinnedConnection(ctx, artistID)
	if err != nil {
		return nil, "", err
	}
	return b.fetchFromConnection(ctx, conn, platformArtistID, imageType)
}

// pinnedConnection picks the ONE connection used for slot reads: the lowest ID
// of the artist's enabled Emby and Jellyfin connections (never Lidarr). A failed
// lookup is an error, never skipped, so a transient failure cannot move the
// artist onto another connection.
func (b *Bridge) pinnedConnection(ctx context.Context, artistID string) (*connection.Connection, string, error) {
	platformIDs, err := b.artistService.GetPlatformIDs(ctx, artistID)
	if err != nil {
		return nil, "", fmt.Errorf("resolving platform IDs for artist %s: %w", artistID, err)
	}
	sorted := slices.SortedFunc(slices.Values(platformIDs), func(x, y artist.PlatformID) int {
		return strings.Compare(x.ConnectionID, y.ConnectionID)
	})
	for _, pid := range sorted {
		conn, connErr := b.connService.GetByID(ctx, pid.ConnectionID)
		if connErr != nil {
			return nil, "", fmt.Errorf("looking up connection %s: %w", pid.ConnectionID, connErr)
		}
		if conn.Enabled && (conn.Type == connection.TypeEmby || conn.Type == connection.TypeJellyfin) {
			return conn, pid.PlatformArtistID, nil
		}
	}
	return nil, "", fmt.Errorf("no enabled emby or jellyfin connection for artist %s", artistID)
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
		if !conn.Enabled || !conn.GetFeatureImageWrite() {
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

// ListArtistImageSlots returns the image types and slot counts that a connected
// platform reports for the given Stillwater artist ID. It resolves platform IDs,
// builds the appropriate client, and calls GetArtistDetail to inspect the
// platform-side image state. Returns the first successful result.
func (b *Bridge) ListArtistImageSlots(ctx context.Context, artistID string) (map[string]int, error) {
	platformIDs, err := b.artistService.GetPlatformIDs(ctx, artistID)
	if err != nil {
		return nil, fmt.Errorf("resolving platform IDs for artist %s: %w", artistID, err)
	}
	if len(platformIDs) == 0 {
		return nil, fmt.Errorf("artist %s has no platform ID mappings", artistID)
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

		state, detailErr := b.getArtistDetailFromConnection(ctx, conn, pid.PlatformArtistID)
		if detailErr != nil {
			b.logger.Debug("artist detail failed for connection",
				slog.String("connection_id", conn.ID),
				slog.String("type", conn.Type),
				slog.String("error", detailErr.Error()))
			lastErr = detailErr
			continue
		}

		slots := make(map[string]int)
		if state.HasThumb {
			slots["thumb"] = 1
		}
		if state.HasFanart {
			count := state.BackdropCount
			if count < 1 {
				count = 1
			}
			slots["fanart"] = count
		}
		if state.HasLogo {
			slots["logo"] = 1
		}
		if state.HasBanner {
			slots["banner"] = 1
		}
		return slots, nil
	}

	if lastErr != nil {
		return nil, fmt.Errorf("all platform connections failed; last error: %w", lastErr)
	}
	return nil, fmt.Errorf("no enabled platform connections for artist %s", artistID)
}

// getArtistDetailFromConnection builds the appropriate platform client and
// fetches the artist detail state.
func (b *Bridge) getArtistDetailFromConnection(ctx context.Context, conn *connection.Connection, platformArtistID string) (*connection.ArtistPlatformState, error) {
	switch conn.Type {
	case connection.TypeEmby:
		c := emby.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), b.logger)
		return c.GetArtistDetail(ctx, platformArtistID)
	case connection.TypeJellyfin:
		c := jellyfin.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), b.logger)
		return c.GetArtistDetail(ctx, platformArtistID)
	default:
		return nil, fmt.Errorf("connection type %q does not support artist detail", conn.Type)
	}
}

// fetchFromConnection builds the appropriate platform client and fetches the image.
func (b *Bridge) fetchFromConnection(ctx context.Context, conn *connection.Connection, platformArtistID, imageType string) ([]byte, string, error) {
	switch conn.Type {
	case connection.TypeEmby:
		c := emby.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), b.logger)
		return c.GetArtistImage(ctx, platformArtistID, imageType)
	case connection.TypeJellyfin:
		c := jellyfin.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), b.logger)
		return c.GetArtistImage(ctx, platformArtistID, imageType)
	default:
		return nil, "", fmt.Errorf("connection type %q does not support image fetch", conn.Type)
	}
}

// uploadToConnection builds the appropriate platform client and uploads the image.
func (b *Bridge) uploadToConnection(ctx context.Context, conn *connection.Connection, platformArtistID, imageType string, data []byte, contentType string) error {
	switch conn.Type {
	case connection.TypeEmby:
		c := emby.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), b.logger)
		return c.UploadImage(ctx, platformArtistID, imageType, data, contentType)
	case connection.TypeJellyfin:
		c := jellyfin.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), b.logger)
		return c.UploadImage(ctx, platformArtistID, imageType, data, contentType)
	default:
		return fmt.Errorf("connection type %q does not support image upload", conn.Type)
	}
}
