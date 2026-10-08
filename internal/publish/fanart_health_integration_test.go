package publish_test

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/publish"
	"github.com/sydlexius/stillwater/internal/rule"
)

// The rule service must satisfy the publisher's reporter interface as is. publish
// cannot import rule, so this external test package is where a signature drift
// between the two fails the build.
var _ publish.FanartHealthReporter = (*rule.Service)(nil)

// TestFanartUnreadable_RealProducerAndRealRuleService drives the real snapshot
// (a real unreadable file on disk) through the real publisher sync into the real
// rule service on SQLite: the finding opens with the slot's position in its
// message, and resolves once the file is readable again. Not covered here: the
// reconciler and Jellyfin paths (fanart_health_report_test.go covers them with a
// fake reporter), and the HTTP rendering of the finding.
func TestFanartUnreadable_RealProducerAndRealRuleService(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(filepath.Join(t.TempDir(), "sw.db"))
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := database.Migrate(db); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("encryptor: %v", err)
	}
	artistSvc := artist.NewService(db)
	connSvc := connection.NewService(db, enc)

	// A peer that answers every request with an error: the push fails after the
	// snapshot, which is all this test needs.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	t.Cleanup(srv.Close)
	conn := &connection.Connection{Name: "peer", Type: connection.TypeEmby, URL: srv.URL, APIKey: "k", Enabled: true, Status: "ok",
		Emby: &connection.EmbyConfig{PlatformUserID: "u1", FeatureImageWrite: true}}
	if err := connSvc.Create(ctx, conn); err != nil {
		t.Fatalf("creating connection: %v", err)
	}

	dir := t.TempDir()
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	img.Set(1, 1, color.RGBA{R: 200, A: 255})
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"fanart.jpg", "fanart2.jpg", "fanart3.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	a := &artist.Artist{Name: "Integration Artist", SortName: "Integration Artist", Path: dir}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if err := artistSvc.SetPlatformID(ctx, a.ID, conn.ID, "p1"); err != nil {
		t.Fatalf("mapping artist: %v", err)
	}

	bad := filepath.Join(dir, "fanart3.jpg") // slot index 2, shown as position 3
	if err := os.Chmod(bad, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(bad, 0o600) })
	if _, err := os.ReadFile(bad); err == nil {
		t.Skip("chmod 0 does not block reads on this runner (running as root?)")
	}

	p := publish.New(publish.Deps{
		Logger:            slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		ArtistService:     artistSvc,
		ConnectionService: connSvc,
	})
	p.SetFanartHealthReporter(ruleSvc)

	finding := func() (status, message string, n int) {
		t.Helper()
		if err := db.QueryRow(`SELECT COUNT(*) FROM rule_violations WHERE rule_id = ? AND artist_id = ?`, rule.RuleFanartUnreadable, a.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			if err := db.QueryRow(`SELECT status, message FROM rule_violations WHERE rule_id = ? AND artist_id = ?`, rule.RuleFanartUnreadable, a.ID).Scan(&status, &message); err != nil {
				t.Fatal(err)
			}
		}
		return status, message, n
	}

	p.SyncAllFanartToPlatforms(ctx, a)
	status, message, n := finding()
	if n != 1 || status != rule.ViolationStatusOpen {
		t.Fatalf("after syncing with an unreadable file: %d rows, status %q, want 1 open", n, status)
	}
	if !strings.Contains(message, "file(s) 3 could not be read") {
		t.Errorf("message = %q, want it to name backdrop position 3", message)
	}

	if err := os.Chmod(bad, 0o600); err != nil {
		t.Fatal(err)
	}
	p.SyncAllFanartToPlatforms(ctx, a)
	if status, _, n := finding(); n != 1 || status != rule.ViolationStatusResolved {
		t.Errorf("after the file became readable: %d rows, status %q, want 1 resolved", n, status)
	}
}
