package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/conflict"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/i18n"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/rule"
)

// artworkDetailRequest issues GET /next/artists/{id} with both an authed user
// and the embedded English translator in context, so the rendered page contains
// real i18n copy (not the empty fallback). Mirrors nextDetailRequest but adds
// the translator the reconciliation status strings need.
func artworkDetailRequest(t *testing.T, r *Router, id string) *httptest.ResponseRecorder {
	t.Helper()
	bundle, err := i18n.LoadEmbedded()
	if err != nil {
		t.Fatalf("loading i18n bundle: %v", err)
	}
	ctx := middleware.WithTestUserID(context.Background(), "test-user")
	ctx = i18n.WithTranslator(ctx, bundle.Translator("en"))
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/artists/"+id, nil)
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	middleware.UX("next", "")(http.HandlerFunc(r.handleNextArtistDetailPage)).ServeHTTP(w, req)
	return w
}

// artworkTestRouter builds a Router wired with the connection + provider
// services that the next/ Artwork section needs: connectionService (so the
// reconciliation status line resolves per-connection managed/mirror state) and
// providerSettings (buildArtistDetailData reads provider priorities). The
// conflict detector/gate are the no-op test variants so connection fixtures
// without live peers do not trip the fail-closed gate.
func artworkTestRouter(t *testing.T) (*Router, *artist.Service) {
	t.Helper()

	db := newTestDB(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}

	authSvc := auth.NewService(db)
	artistSvc := artist.NewService(db)
	connSvc := connection.NewService(db, enc)
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	nfoSnapSvc := nfo.NewSnapshotService(db)

	r := NewRouter(RouterDeps{
		AuthService:        authSvc,
		ArtistService:      artistSvc,
		ConnectionService:  connSvc,
		RuleService:        ruleSvc,
		NFOSnapshotService: nfoSnapSvc,
		DB:                 db,
		Logger:             logger,
		StaticFS:           os.DirFS("../../web/static"),
	})
	r.providerSettings = provider.NewSettingsService(r.db, nil)
	r.conflictDetector = conflict.NewForTest(connSvc, logger)
	r.conflictGate = conflict.NewGate(r.conflictDetector)

	return r, artistSvc
}

// seedArtworkConnection creates a connection (optionally Stillwater-managed) and
// links it to the artist so it appears in the detail page's connection list.
func seedArtworkConnection(t *testing.T, r *Router, artistSvc *artist.Service, artistID, id, connType string, managed bool) {
	t.Helper()
	c := &connection.Connection{
		ID:                       id,
		Name:                     id,
		Type:                     connType,
		URL:                      "http://localhost:8096",
		APIKey:                   "test-key",
		Enabled:                  true,
		Status:                   "ok",
		FeatureManageServerFiles: managed,
	}
	if err := r.connectionService.Create(context.Background(), c); err != nil {
		t.Fatalf("creating connection %s: %v", id, err)
	}
	if err := artistSvc.SetPlatformID(context.Background(), artistID, id, "platform-"+id); err != nil {
		t.Fatalf("linking platform id for %s: %v", id, err)
	}
}

// TestHandleNextArtistDetailPage_ArtworkSection verifies the next/ page renders
// the 4B Artwork section end to end from real SQLite: the section card, identity
// tiles, the single Manage trigger, and -- with no connections -- the local-only
// reconciliation status.
func TestHandleNextArtistDetailPage_ArtworkSection(t *testing.T) {
	t.Parallel()
	r, artistSvc := artworkTestRouter(t)
	id := seedDetailArtist(t, artistSvc, "Artwork Probe")

	w := artworkDetailRequest(t, r, id)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	for label, want := range map[string]string{
		"artwork section card": `id="next-artwork-` + id,
		"section nav key":      `data-sw-section="artwork"`,
		"manage trigger":       "data-sw-artwork-open",
		"reconciliation line":  "Reconciliation status",
		"local-only status":    "only source of truth",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("artwork section missing %s (%q)", label, want)
		}
	}
}

// TestHandleNextArtistDetailPage_ArtworkReconciliation verifies the per-connection
// reconciliation status reflects each connection's managed flag: a managed Emby
// reads "Managed by Stillwater"; an unmanaged Lidarr reads as a plain mirror.
func TestHandleNextArtistDetailPage_ArtworkReconciliation(t *testing.T) {
	t.Parallel()
	r, artistSvc := artworkTestRouter(t)
	id := seedDetailArtist(t, artistSvc, "Recon Probe")
	seedArtworkConnection(t, r, artistSvc, id, "conn-emby", "emby", true)
	seedArtworkConnection(t, r, artistSvc, id, "conn-lidarr", "lidarr", false)

	w := artworkDetailRequest(t, r, id)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	for label, want := range map[string]string{
		"managed connection name": "conn-emby",
		"managed status":          "Managed by Stillwater",
		"mirror connection name":  "conn-lidarr",
		"plain mirror status":     "Mirror of the shared library folder",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("reconciliation status missing %s (%q)", label, want)
		}
	}
	// With connections present, the local-only line must not appear.
	if strings.Contains(body, "only source of truth") {
		t.Error("local-only reconciliation line should be absent when connections exist")
	}
}

// TestHandleNextArtworkModal_RendersEditorPerKind verifies the modal-body
// fragment endpoint renders the reused ArtworkManageEditor scoped to the
// requested kind (primary -> thumb), including the crop modal the editor hosts.
func TestHandleNextArtworkModal_RendersEditorPerKind(t *testing.T) {
	t.Parallel()
	r, artistSvc := artworkTestRouter(t)
	id := seedDetailArtist(t, artistSvc, "Modal Editor")

	ctx := middleware.WithTestUXChannel(context.Background(), middleware.UXNext)
	ctx = middleware.WithTestUserID(ctx, "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/artists/"+id+"/artwork-modal?kind=primary", nil)
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	r.handleNextArtworkModal(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// The editor hosts the crop modal and targets the thumb (primary) image.
	for label, want := range map[string]string{
		"crop modal":      `id="crop-modal"`,
		"primary type":    `data-image-type="thumb"`,
		"image drop zone": `id="image-drop-zone"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("modal editor fragment (kind=primary) missing %s (%q)", label, want)
		}
	}
	// The fragment must NOT re-include cropper assets (the modal shell loads them).
	if strings.Contains(body, "cropper.min.js") {
		t.Error("editor fragment should not re-include cropper assets; the shell loads them once")
	}
}

// TestHandleNextArtworkModal_UnknownArtist verifies a missing artist 404s.
func TestHandleNextArtworkModal_UnknownArtist(t *testing.T) {
	t.Parallel()
	r, _ := artworkTestRouter(t)
	ctx := middleware.WithTestUXChannel(context.Background(), middleware.UXNext)
	ctx = middleware.WithTestUserID(ctx, "test-user")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/next/artists/nope/artwork-modal?kind=primary", nil)
	req.SetPathValue("id", "nope")
	w := httptest.NewRecorder()
	r.handleNextArtworkModal(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// TestArtworkKindToType pins the modal kind -> API image-type mapping for every
// kind plus the unknown/default fallback. A regression here (e.g. backdrops
// resolving to thumb) would make the modal's Backdrops tab edit the wrong slot.
func TestArtworkKindToType(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"primary":   "thumb",
		"logo":      "logo",
		"banner":    "banner",
		"backdrops": "fanart",
		"":          "thumb", // unknown falls to the primary/thumb default
		"bogus":     "thumb",
	}
	for kind, want := range cases {
		if got := artworkKindToType(kind); got != want {
			t.Errorf("artworkKindToType(%q) = %q, want %q", kind, got, want)
		}
	}
}
