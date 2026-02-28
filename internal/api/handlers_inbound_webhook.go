package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/event"
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	go func() {
		defer cancel()
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
		if strings.Contains(err.Error(), "already in progress") {
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
