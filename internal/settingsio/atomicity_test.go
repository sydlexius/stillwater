package settingsio

// atomicity_test.go pins the #1693 invariant: when ImportWithOptions fails
// mid-stream, none of the prior sections' rows commit to the target. The
// pre-fix code had a two-phase split where the first phase's writes
// (connections, platform profiles, webhooks, provider keys, priorities,
// rules, scraper preferences) persisted even if the second phase aborted,
// leaving the operator with a half-applied configuration.

import (
	"context"
	"errors"
	"testing"

	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/internal/scraper"
	"github.com/sydlexius/stillwater/internal/webhook"

	"log/slog"
)

// TestImport_AtomicAcrossAllSections is the headline test for #1693. The
// envelope exercises every section the orchestrator drives (connections,
// platform profiles, webhooks, provider keys, priorities, rules, scraper,
// settings, users, user_preferences). The target is pre-seeded so the
// envelope's user collides with a DIFFERENT id under the same username,
// which makes importUsers fail with ErrUserIDCollision. Because importUsers
// runs AFTER the previously-pre-tx sections (connections, platform
// profiles, webhooks, etc.), this test would have passed on the old code
// for the post-importUsers sections only -- the pre-tx sections would have
// committed and the assertions below would fail. The single-tx orchestrator
// must roll all of them back.
func TestImport_AtomicAcrossAllSections(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	ruleSvc := rule.NewService(db)
	scraperSvc := scraper.NewService(db, slog.Default())
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	if err := scraperSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding scraper: %v", err)
	}

	// Seed the source DB with one row of every export-able shape.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)`,
		"atomicity.test.key", "atomicity.test.value",
	); err != nil {
		t.Fatalf("seeding settings: %v", err)
	}
	if err := connSvc.Create(ctx, &connection.Connection{
		Name: "AtomicSrcConn", Type: "emby", URL: "http://atomic.example:8096",
		APIKey: "atomic-src-key", Enabled: true,
	}); err != nil {
		t.Fatalf("seeding source connection: %v", err)
	}
	if err := platSvc.Create(ctx, &platform.Profile{
		Name: "AtomicSrcProfile", NFOEnabled: true, NFOFormat: "kodi",
	}); err != nil {
		t.Fatalf("seeding source profile: %v", err)
	}
	if err := whSvc.Create(ctx, &webhook.Webhook{
		Name: "AtomicSrcHook", URL: "https://atomic.example/hook", Type: "generic",
		Events: []string{"artist.new"}, Enabled: true,
	}); err != nil {
		t.Fatalf("seeding source webhook: %v", err)
	}
	if err := provSettings.SetAPIKey(ctx, provider.NameFanartTV, "atomic-fanart-key"); err != nil {
		t.Fatalf("seeding source provider key: %v", err)
	}
	if err := provSettings.SetPriority(ctx, "biography",
		[]provider.ProviderName{provider.NameMusicBrainz, provider.NameWikipedia}); err != nil {
		t.Fatalf("seeding source priority: %v", err)
	}
	// Seed the source user that will collide on the target. The exported
	// envelope brings id=src-collide-id under username=alice.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO users (id, username, role) VALUES (?, ?, ?)`,
		"src-collide-id", "alice", "administrator",
	); err != nil {
		t.Fatalf("seeding source user: %v", err)
	}

	svc := NewService(db, provSettings, connSvc, platSvc, whSvc).
		WithRuleService(ruleSvc).
		WithScraperService(scraperSvc)
	envelope, err := svc.Export(ctx, "atomicity-passphrase")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	// Build a fresh target. Pre-seed alice under a DIFFERENT id so
	// importUsers fails with ErrUserIDCollision after every other section
	// has already executed inside the orchestrator's tx.
	db2 := setupTestDB(t)
	provSettings2, connSvc2, platSvc2, whSvc2 := newTestServices(t, db2)
	ruleSvc2 := rule.NewService(db2)
	scraperSvc2 := scraper.NewService(db2, slog.Default())
	if err := ruleSvc2.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding target rules: %v", err)
	}
	if err := scraperSvc2.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding target scraper: %v", err)
	}
	if _, err := db2.ExecContext(ctx,
		`INSERT INTO users (id, username, role) VALUES (?, ?, ?)`,
		"target-collide-id", "alice", "operator",
	); err != nil {
		t.Fatalf("seeding target user: %v", err)
	}

	svc2 := NewService(db2, provSettings2, connSvc2, platSvc2, whSvc2).
		WithRuleService(ruleSvc2).
		WithScraperService(scraperSvc2)

	result, err := svc2.Import(ctx, envelope, "atomicity-passphrase")
	if err == nil {
		t.Fatal("Import: expected ErrUserIDCollision, got nil")
	}
	if !errors.Is(err, ErrUserIDCollision) {
		t.Fatalf("Import: expected ErrUserIDCollision wrapped, got %v", err)
	}

	// Single-tx atomicity: ImportResult counters must reset to zero on
	// rollback so callers do not see partial counts.
	if result != nil && (*result != ImportResult{}) {
		t.Errorf("ImportResult must reset to zero on rollback; got %+v", *result)
	}

	// Now the actual atomicity assertions: every section that ran BEFORE
	// importUsers must have rolled back. The target should be in its
	// pre-import state.
	t.Run("connections rolled back", func(t *testing.T) {
		var count int
		if err := db2.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM connections WHERE name = 'AtomicSrcConn'`).Scan(&count); err != nil {
			t.Fatalf("counting connections: %v", err)
		}
		if count != 0 {
			t.Errorf("connection committed despite import failure: count=%d", count)
		}
	})
	t.Run("platform profiles rolled back", func(t *testing.T) {
		var count int
		if err := db2.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM platform_profiles WHERE name = 'AtomicSrcProfile'`).Scan(&count); err != nil {
			t.Fatalf("counting profiles: %v", err)
		}
		if count != 0 {
			t.Errorf("platform profile committed despite import failure: count=%d", count)
		}
	})
	t.Run("webhooks rolled back", func(t *testing.T) {
		var count int
		if err := db2.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM webhooks WHERE name = 'AtomicSrcHook'`).Scan(&count); err != nil {
			t.Fatalf("counting webhooks: %v", err)
		}
		if count != 0 {
			t.Errorf("webhook committed despite import failure: count=%d", count)
		}
	})
	t.Run("provider key rolled back", func(t *testing.T) {
		// The key may or may not be present depending on what the target
		// already had. We seeded nothing on the target so the row must be
		// absent.
		has, err := provSettings2.HasAPIKey(ctx, provider.NameFanartTV)
		if err != nil {
			t.Fatalf("HasAPIKey: %v", err)
		}
		if has {
			t.Error("provider key committed despite import failure")
		}
	})
	t.Run("provider priority rolled back", func(t *testing.T) {
		// Migration 001 seeds default priority rows so absence-of-row is
		// the wrong probe. Capture the post-rollback value and assert it
		// matches the migration default, not the envelope's
		// "musicbrainz,wikipedia" list.
		var got string
		if err := db2.QueryRowContext(ctx,
			`SELECT value FROM settings WHERE key = 'provider.priority.biography'`).Scan(&got); err != nil {
			t.Fatalf("reading priority row: %v", err)
		}
		// The migration seeds biography with musicbrainz/lastfm/audiodb/discogs.
		// PR #1438 removed wikidata; the envelope was musicbrainz/wikipedia
		// which is clearly distinct from the migration default.
		if got != `["musicbrainz","lastfm","audiodb","discogs"]` {
			t.Errorf("provider priority committed despite import failure: got %q", got)
		}
	})
	t.Run("settings KV rolled back", func(t *testing.T) {
		var count int
		if err := db2.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM settings WHERE key = 'atomicity.test.key'`).Scan(&count); err != nil {
			t.Fatalf("counting setting: %v", err)
		}
		if count != 0 {
			t.Errorf("setting committed despite import failure: count=%d", count)
		}
	})
	t.Run("target alice unchanged", func(t *testing.T) {
		var id, role string
		if err := db2.QueryRowContext(ctx,
			`SELECT id, role FROM users WHERE username = 'alice'`).Scan(&id, &role); err != nil {
			t.Fatalf("scanning target alice: %v", err)
		}
		if id != "target-collide-id" || role != "operator" {
			t.Errorf("target alice mutated despite import failure: id=%q role=%q", id, role)
		}
	})
}

// TestImport_AtomicSameSectionRollback verifies that writes performed by
// a tx-aware importer helper do not persist when the surrounding
// transaction is rolled back without committing. This isolates the
// rollback boundary on a single helper (importConnections) using an
// explicit BeginTx / Rollback, complementing the broader cross-section
// test in TestImport_AtomicAcrossAllSections.
func TestImport_AtomicSameSectionRollback(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	// Use importConnections directly inside our own tx, then deliberately
	// fail the import by rolling back the tx -- this proves the tx-aware
	// helpers honor the rollback boundary.
	conns := []ConnectionExport{
		{Name: "TxBoundConn", Type: "emby", URL: "http://txbound.example:8096", APIKey: "k", Enabled: true},
	}
	if err := svc.importConnections(ctx, tx, conns, &ImportResult{}, true, true); err != nil {
		_ = tx.Rollback()
		t.Fatalf("importConnections inside tx: %v", err)
	}
	// Roll back without committing.
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// The connection must not be visible outside the tx because the tx
	// rolled back.
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM connections WHERE name = 'TxBoundConn'`).Scan(&count); err != nil {
		t.Fatalf("counting connections: %v", err)
	}
	if count != 0 {
		t.Errorf("connection committed despite tx rollback: count=%d", count)
	}
}
