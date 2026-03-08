package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/sydlexius/stillwater/internal/event"
	"github.com/sydlexius/stillwater/internal/scanner"
	"github.com/sydlexius/stillwater/internal/webhook"
)

// handleLidarrWebhook receives inbound webhook events from Lidarr.
// POST /api/v1/webhooks/inbound/lidarr
func (r *Router) handleLidarrWebhook(w http.ResponseWriter, req *http.Request) {
	// Limit request body to 1 MB
	req.Body = http.MaxBytesReader(w, req.Body, 1<<20)

	var payload webhook.LidarrPayload
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON payload"})
		return
	}

	if payload.EventType == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "eventType is required"})
		return
	}

	r.logger.Info("received lidarr webhook",
		"event_type", payload.EventType,
		"artist", artistNameFromPayload(payload),
	)

	// Respond immediately
	writeJSON(w, http.StatusOK, map[string]string{"status": "accepted"})

	// Process asynchronously with a bounded context.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute) //nolint:gosec // G118: cancel is deferred inside the goroutine below
	go func() {
		defer cancel()
		defer func() {
			if v := recover(); v != nil {
				r.logger.Error("panic in lidarr webhook handler",
					"event_type", payload.EventType,
					"panic", v,
					"stack", string(debug.Stack()),
				)
			}
		}()
		r.processLidarrEvent(ctx, payload)
	}()
}

func (r *Router) processLidarrEvent(ctx context.Context, payload webhook.LidarrPayload) {
	switch payload.EventType {
	case webhook.LidarrEventTest:
		r.logger.Info("lidarr test event received")
		return

	case webhook.LidarrEventArtistAdd:
		r.handleLidarrArtistAdd(ctx, payload)

	case webhook.LidarrEventDownload, webhook.LidarrEventAlbumImport, webhook.LidarrEventGrab:
		r.handleLidarrDownload(ctx, payload)

	default:
		r.logger.Debug("unhandled lidarr event type", "event_type", payload.EventType)
	}
}

func (r *Router) handleLidarrArtistAdd(ctx context.Context, payload webhook.LidarrPayload) {
	if payload.Artist == nil {
		r.logger.Warn("lidarr ArtistAdded event missing artist data")
		return
	}

	mbid := payload.Artist.MBID()
	if mbid == "" {
		r.logger.Warn("lidarr ArtistAdded event missing MBID", "artist", payload.Artist.Name)
		return
	}

	// Publish event
	if r.eventBus != nil {
		r.eventBus.Publish(event.Event{
			Type:      event.LidarrArtistAdd,
			Timestamp: time.Now().UTC(),
			Data: map[string]any{
				"artist_name": payload.Artist.Name,
				"mbid":        mbid,
			},
		})
	}

	// Check if artist already exists
	existing, err := r.artistService.GetByMBID(ctx, mbid)
	if err != nil {
		r.logger.Error("looking up artist by MBID for lidarr webhook", "mbid", mbid, "error", err)
		return
	}

	if existing != nil {
		// Artist known: re-evaluate rules for this artist only
		r.logger.Info("lidarr ArtistAdded: artist already tracked, evaluating rules",
			"artist", existing.Name, "mbid", mbid)
		if _, err := r.pipeline.RunForArtist(ctx, existing); err != nil {
			r.logger.Error("rule evaluation after lidarr ArtistAdded failed", "artist", existing.Name, "error", err)
		}
		return
	}

	// Artist unknown: trigger a scan to discover the new directory
	r.logger.Info("lidarr ArtistAdded: new artist, triggering scan",
		"artist", payload.Artist.Name, "mbid", mbid)
	if _, err := r.scannerService.Run(ctx); err != nil {
		if errors.Is(err, scanner.ErrScanInProgress) {
			r.logger.Info("scan after lidarr ArtistAdded skipped: scan already in progress")
		} else {
			r.logger.Error("scan after lidarr ArtistAdded failed", "error", err)
		}
	}
}

func (r *Router) handleLidarrDownload(ctx context.Context, payload webhook.LidarrPayload) {
	if payload.Artist == nil {
		r.logger.Warn("lidarr download event missing artist data")
		return
	}

	mbid := payload.Artist.MBID()
	if mbid == "" {
		r.logger.Warn("lidarr download event missing MBID", "artist", payload.Artist.Name)
		return
	}

	// Publish event
	if r.eventBus != nil {
		r.eventBus.Publish(event.Event{
			Type:      event.LidarrDownload,
			Timestamp: time.Now().UTC(),
			Data: map[string]any{
				"artist_name": payload.Artist.Name,
				"mbid":        mbid,
			},
		})
	}

	// Lookup artist and re-evaluate rules
	existing, err := r.artistService.GetByMBID(ctx, mbid)
	if err != nil {
		r.logger.Error("looking up artist for lidarr download event", "mbid", mbid, "error", err)
		return
	}

	if existing == nil {
		r.logger.Info("lidarr download event for unknown artist, skipping",
			"artist", payload.Artist.Name, "mbid", mbid)
		return
	}

	r.logger.Info("lidarr download event: re-evaluating rules for artist",
		"artist", existing.Name, "mbid", mbid)
	if _, err := r.pipeline.RunForArtist(ctx, existing); err != nil {
		r.logger.Error("rule evaluation after lidarr download failed", "artist", existing.Name, "mbid", mbid, "error", err)
	}
}

func artistNameFromPayload(p webhook.LidarrPayload) string {
	if p.Artist != nil {
		return p.Artist.Name
	}
	return ""
}

// handleEmbyWebhook receives inbound webhook events from Emby.
// POST /api/v1/webhooks/inbound/emby
func (r *Router) handleEmbyWebhook(w http.ResponseWriter, req *http.Request) {
	req.Body = http.MaxBytesReader(w, req.Body, 1<<20)

	var payload webhook.EmbyPayload
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		r.logger.Warn("emby webhook: failed to decode payload", "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON payload"})
		return
	}

	if payload.Event == "" {
		r.logger.Warn("emby webhook: missing Event field")
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Event is required"})
		return
	}

	r.logger.Info("received emby webhook", "event", payload.Event)

	writeJSON(w, http.StatusOK, map[string]string{"status": "accepted"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute) //nolint:gosec // G118: cancel is deferred inside the goroutine below
	go func() {
		defer cancel()
		defer func() {
			if v := recover(); v != nil {
				r.logger.Error("panic in emby webhook handler",
					"notification_type", payload.Event,
					"panic", v,
					"stack", string(debug.Stack()),
				)
			}
		}()
		r.processEmbyEvent(ctx, payload)
	}()
}

func (r *Router) processEmbyEvent(ctx context.Context, payload webhook.EmbyPayload) {
	switch payload.Event {
	case webhook.EmbyEventTest:
		r.logger.Info("emby test event received")
		return

	case webhook.EmbyEventItemAdded, webhook.EmbyEventItemUpdated:
		r.handleEmbyArtistUpdate(ctx, payload, payload.Event)

	case webhook.EmbyEventLibraryChanged:
		r.handleEmbyLibraryScan(ctx)

	default:
		r.logger.Debug("unhandled emby event type", "notification_type", payload.Event)
	}
}

func (r *Router) handleEmbyArtistUpdate(ctx context.Context, payload webhook.EmbyPayload, notificationType string) {
	if payload.Item == nil {
		r.logger.Warn("emby artist update: payload has no item data, skipping")
		return
	}
	// Emby 4.9 sends MusicAlbum items (not MusicArtist); artist info is in ArtistItems.
	if payload.Item.Type != "MusicAlbum" {
		r.logger.Debug("emby item update: skipping non-album item",
			"item_type", payload.Item.Type,
			"item_name", payload.Item.Name)
		return
	}

	mbids := payload.Item.ArtistMBIDs()
	if len(mbids) == 0 {
		r.logger.Warn("emby artist update: no artist MBIDs in payload, skipping",
			"item_name", payload.Item.Name)
		return
	}

	if r.eventBus != nil {
		r.eventBus.Publish(event.Event{
			Type:      event.EmbyArtistUpdate,
			Timestamp: time.Now().UTC(),
			Data:      map[string]any{"album_name": payload.Item.Name},
		})
	}

	for _, mbid := range mbids {
		existing, err := r.artistService.GetByMBID(ctx, mbid)
		if err != nil {
			r.logger.Error("looking up artist by MBID for emby webhook", "mbid", mbid, "error", err)
			continue
		}
		if existing == nil {
			// Emby item events do not imply a new directory was created, so we do not
			// trigger a scan here. Unknown artists will be picked up on the next scan.
			r.logger.Info("emby artist update: artist not tracked, skipping",
				"notification_type", notificationType, "mbid", mbid)
			continue
		}
		if r.pipeline == nil {
			r.logger.Warn("emby artist update: rule pipeline not configured, skipping evaluation",
				"artist", existing.Name)
			continue
		}
		r.logger.Info("emby artist update: artist tracked, evaluating rules",
			"notification_type", notificationType, "artist", existing.Name, "mbid", mbid)
		if _, err := r.pipeline.RunForArtist(ctx, existing); err != nil {
			r.logger.Error("rule evaluation after emby artist update failed", "artist", existing.Name, "error", err)
		}
	}
}

func (r *Router) handleEmbyLibraryScan(ctx context.Context) {
	if r.eventBus != nil {
		r.eventBus.Publish(event.Event{
			Type:      event.EmbyLibraryScan,
			Timestamp: time.Now().UTC(),
		})
	}

	if r.scannerService == nil {
		r.logger.Warn("emby library changed: scanner service not configured, skipping scan")
		return
	}

	if _, err := r.scannerService.Run(ctx); err != nil {
		if errors.Is(err, scanner.ErrScanInProgress) {
			r.logger.Info("scan after emby library changed skipped: scan already in progress")
		} else {
			r.logger.Error("scan after emby library changed failed", "error", err)
		}
	}
}

// handleJellyfinWebhook receives inbound webhook events from the Jellyfin webhook plugin.
// POST /api/v1/webhooks/inbound/jellyfin
func (r *Router) handleJellyfinWebhook(w http.ResponseWriter, req *http.Request) {
	req.Body = http.MaxBytesReader(w, req.Body, 1<<20)

	var payload webhook.JellyfinPayload
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		r.logger.Warn("jellyfin webhook: failed to decode payload", "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON payload"})
		return
	}

	if payload.NotificationType == "" {
		r.logger.Warn("jellyfin webhook: missing NotificationType")
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "NotificationType is required"})
		return
	}

	r.logger.Info("received jellyfin webhook", "notification_type", payload.NotificationType)

	writeJSON(w, http.StatusOK, map[string]string{"status": "accepted"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute) //nolint:gosec // G118: cancel is deferred inside the goroutine below
	go func() {
		defer cancel()
		defer func() {
			if v := recover(); v != nil {
				r.logger.Error("panic in jellyfin webhook handler",
					"notification_type", payload.NotificationType,
					"panic", v,
					"stack", string(debug.Stack()),
				)
			}
		}()
		r.processJellyfinEvent(ctx, payload)
	}()
}

func (r *Router) processJellyfinEvent(ctx context.Context, payload webhook.JellyfinPayload) {
	switch payload.NotificationType {
	case webhook.JellyfinEventTest:
		r.logger.Info("jellyfin test event received")
		return

	case webhook.JellyfinEventItemAdded, webhook.JellyfinEventItemUpdated:
		r.handleJellyfinArtistUpdate(ctx, payload, payload.NotificationType)

	case webhook.JellyfinEventLibraryChanged:
		r.handleJellyfinLibraryScan(ctx)

	default:
		r.logger.Debug("unhandled jellyfin event type", "notification_type", payload.NotificationType)
	}
}

func (r *Router) handleJellyfinArtistUpdate(ctx context.Context, payload webhook.JellyfinPayload, notificationType string) {
	// Jellyfin sends MusicAlbum items (not MusicArtist); artist MBID is in ProviderMusicBrainzAlbumArtist.
	if payload.ItemType != "MusicAlbum" {
		r.logger.Debug("jellyfin item update: skipping non-album item",
			"item_type", payload.ItemType,
			"item_name", payload.Name)
		return
	}

	mbid := payload.MBID()
	if mbid == "" {
		r.logger.Warn("jellyfin artist update: no MBID in payload, skipping",
			"item_name", payload.Name)
		return
	}

	if r.eventBus != nil {
		r.eventBus.Publish(event.Event{
			Type:      event.JellyfinArtistUpdate,
			Timestamp: time.Now().UTC(),
			Data: map[string]any{
				"artist_name": payload.Name,
				"mbid":        mbid,
			},
		})
	}

	existing, err := r.artistService.GetByMBID(ctx, mbid)
	if err != nil {
		r.logger.Error("looking up artist by MBID for jellyfin webhook", "mbid", mbid, "error", err)
		return
	}

	if existing == nil {
		// Jellyfin item events do not imply a new directory was created, so we do not
		// trigger a scan here. Unknown artists will be picked up on the next scan.
		r.logger.Info("jellyfin artist update: artist not tracked, skipping",
			"notification_type", notificationType, "artist", payload.Name, "mbid", mbid)
		return
	}

	if r.pipeline == nil {
		r.logger.Warn("jellyfin artist update: rule pipeline not configured, skipping evaluation",
			"artist", existing.Name)
		return
	}

	r.logger.Info("jellyfin artist update: artist tracked, evaluating rules",
		"notification_type", notificationType, "artist", existing.Name, "mbid", mbid)
	if _, err := r.pipeline.RunForArtist(ctx, existing); err != nil {
		r.logger.Error("rule evaluation after jellyfin artist update failed", "artist", existing.Name, "error", err)
	}
}

func (r *Router) handleJellyfinLibraryScan(ctx context.Context) {
	if r.eventBus != nil {
		r.eventBus.Publish(event.Event{
			Type:      event.JellyfinLibraryScan,
			Timestamp: time.Now().UTC(),
		})
	}

	if r.scannerService == nil {
		r.logger.Warn("jellyfin library changed: scanner service not configured, skipping scan")
		return
	}

	if _, err := r.scannerService.Run(ctx); err != nil {
		if errors.Is(err, scanner.ErrScanInProgress) {
			r.logger.Info("scan after jellyfin library changed skipped: scan already in progress")
		} else {
			r.logger.Error("scan after jellyfin library changed failed", "error", err)
		}
	}
}
