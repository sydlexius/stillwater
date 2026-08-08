package settingsio

import (
	"context"
	"testing"

	"github.com/sydlexius/stillwater/internal/connection"
)

// #2579, import half. A settings envelope whose Lidarr connection carries
// feature_image_write (or metadata_push / trigger_refresh) reported a plain
// success: `connections: 1`, every other field landing correctly, and the
// feature value silently dropped because applyExportConfig has no Lidarr
// assignment for it.
//
// Unlike the API half this surface does NOT reject. An envelope is a batch
// restore: refusing the whole import over one inapplicable field would block
// an operator recovering a backup, which is a worse outcome than the one being
// fixed. The requirement here is that the drop stops being SILENT -- it is
// counted in ImportResult so the operator's response names it.
//
// The gate keys on the value being TRUE, not merely present: ConnectionExport
// carries plain bools, so an absent field and an explicit false are
// indistinguishable on the wire, and export itself emits false for every
// Lidarr row. Keying on presence would make every legitimate Lidarr envelope
// report an ignored field.

// lidarrFeatureEnvelope builds a one-connection envelope for a Lidarr peer
// with the named feature fields set true.
func lidarrFeatureEnvelope(imageWrite, metadataPush, triggerRefresh bool) []ConnectionExport {
	return []ConnectionExport{{
		Name: "Lidarr A", Type: "lidarr", URL: "http://lidarr.local:8686",
		APIKey: "key1", Enabled: true,
		FeatureImageWrite:     imageWrite,
		FeatureMetadataPush:   metadataPush,
		FeatureTriggerRefresh: triggerRefresh,
	}}
}

// TestImportConnections_LidarrFeatureFlagsCountedNotSilent covers the fresh
// INSERT path: the connection is still created, but the dropped fields are
// reported rather than vanishing.
func TestImportConnections_LidarrFeatureFlagsCountedNotSilent(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)

	result := &ImportResult{}
	if err := svc.importConnections(ctx, db, lidarrFeatureEnvelope(true, true, true), result, true, true); err != nil {
		t.Fatalf("importConnections: %v", err)
	}

	// The import must still SUCCEED -- this is the deliberate difference
	// from the API surface, and asserting it stops a future change from
	// "fixing" this by rejecting the envelope.
	if result.Connections != 1 {
		t.Errorf("Connections = %d, want 1 (the import must not be refused)", result.Connections)
	}
	if result.ConnectionFeaturesIgnored != 3 {
		t.Errorf("ConnectionFeaturesIgnored = %d, want 3", result.ConnectionFeaturesIgnored)
	}

	// The connection itself must be intact in every applicable respect --
	// the issue notes name/url/enabled/api_key all landed correctly before
	// the fix, and that must stay true.
	all, err := connSvc.List(ctx)
	if err != nil {
		t.Fatalf("listing connections: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 connection, got %d", len(all))
	}
	got := all[0]
	if got.Name != "Lidarr A" || got.URL != "http://lidarr.local:8686" || got.APIKey != "key1" || !got.Enabled {
		t.Errorf("connection fields not imported: %+v", got)
	}
	if got.Lidarr == nil {
		t.Error("Lidarr sub-config not materialized")
	}
}

// TestImportConnections_LidarrFeatureFlagsCountedOnUpdate covers the UPDATE
// path independently. The two branches build the connection differently (one
// from the envelope, one from the stored row) and only share applyExportConfig,
// so a counter wired into just one branch would leave the other silent.
func TestImportConnections_LidarrFeatureFlagsCountedOnUpdate(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)

	seed := &connection.Connection{
		Name: "Lidarr A", Type: "lidarr", URL: "http://lidarr.local:8686",
		APIKey: "old", Enabled: true,
	}
	if err := connSvc.Create(ctx, seed); err != nil {
		t.Fatalf("seeding target connection: %v", err)
	}

	result := &ImportResult{}
	if err := svc.importConnections(ctx, db, lidarrFeatureEnvelope(true, true, true), result, true, true); err != nil {
		t.Fatalf("importConnections: %v", err)
	}
	if result.ConnectionFeaturesIgnored != 3 {
		t.Errorf("ConnectionFeaturesIgnored = %d, want 3 (update path)", result.ConnectionFeaturesIgnored)
	}

	all, err := connSvc.List(ctx)
	if err != nil {
		t.Fatalf("listing connections: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 connection, got %d", len(all))
	}
	if all[0].APIKey != "key1" {
		t.Errorf("APIKey = %q, want key1 (the update must still apply)", all[0].APIKey)
	}
}

// TestImportConnections_LidarrCountsEachFieldIndependently prevents a counter
// wired to image_write alone from passing the combined assertions above.
func TestImportConnections_LidarrCountsEachFieldIndependently(t *testing.T) {
	cases := []struct {
		name                                     string
		imageWrite, metadataPush, triggerRefresh bool
	}{
		{"image_write", true, false, false},
		{"metadata_push", false, true, false},
		{"trigger_refresh", false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := setupTestDB(t)
			ctx := context.Background()
			provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
			svc := NewService(db, provSettings, connSvc, platSvc, whSvc)

			result := &ImportResult{}
			conns := lidarrFeatureEnvelope(tc.imageWrite, tc.metadataPush, tc.triggerRefresh)
			if err := svc.importConnections(ctx, db, conns, result, true, true); err != nil {
				t.Fatalf("importConnections: %v", err)
			}
			if result.ConnectionFeaturesIgnored != 1 {
				t.Errorf("ConnectionFeaturesIgnored = %d, want 1", result.ConnectionFeaturesIgnored)
			}
		})
	}
}

// TestImportConnections_LidarrWithoutFeatureFlagsCountsZero is the
// false-positive guard. Export emits false for all three on every Lidarr row,
// so a gate keyed on field PRESENCE rather than on a true value would report
// an ignored field for every ordinary Lidarr envelope -- noise that would
// train operators to ignore the counter.
func TestImportConnections_LidarrWithoutFeatureFlagsCountsZero(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)

	result := &ImportResult{}
	if err := svc.importConnections(ctx, db, lidarrFeatureEnvelope(false, false, false), result, true, true); err != nil {
		t.Fatalf("importConnections: %v", err)
	}
	if result.ConnectionFeaturesIgnored != 0 {
		t.Errorf("ConnectionFeaturesIgnored = %d, want 0 for a plain lidarr envelope", result.ConnectionFeaturesIgnored)
	}
	if result.Connections != 1 {
		t.Errorf("Connections = %d, want 1", result.Connections)
	}
}

// TestImportConnections_SupportedTypesNotCounted is the direction guard: a
// predicate widened past Lidarr would count fields that were in fact applied,
// and every Lidarr assertion above would still pass.
func TestImportConnections_SupportedTypesNotCounted(t *testing.T) {
	for _, connType := range []string{"emby", "jellyfin"} {
		t.Run(connType, func(t *testing.T) {
			db := setupTestDB(t)
			ctx := context.Background()
			provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
			svc := NewService(db, provSettings, connSvc, platSvc, whSvc)

			conns := []ConnectionExport{{
				Name: "Peer", Type: connType, URL: "http://" + connType + ".local:8096",
				APIKey: "key1", Enabled: true,
				FeatureImageWrite:     true,
				FeatureMetadataPush:   true,
				FeatureTriggerRefresh: true,
			}}
			result := &ImportResult{}
			if err := svc.importConnections(ctx, db, conns, result, true, true); err != nil {
				t.Fatalf("importConnections: %v", err)
			}
			if result.ConnectionFeaturesIgnored != 0 {
				t.Errorf("ConnectionFeaturesIgnored = %d, want 0 for %s", result.ConnectionFeaturesIgnored, connType)
			}
			// Precondition on the guard itself: the fields must actually
			// have been applied, or "not counted" would be trivially true.
			all, err := connSvc.List(ctx)
			if err != nil {
				t.Fatalf("listing connections: %v", err)
			}
			if len(all) != 1 || !all[0].GetFeatureImageWrite() ||
				!all[0].GetFeatureMetadataPush() || !all[0].GetFeatureTriggerRefresh() {
				t.Errorf("%s features not applied, so the not-counted assertion is vacuous: %+v", connType, all)
			}
		})
	}
}
