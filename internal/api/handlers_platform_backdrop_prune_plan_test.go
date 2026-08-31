package api

import (
	"encoding/json"
	"testing"

	"github.com/sydlexius/stillwater/internal/publish"
)

// TestPlatformPruneResponse_CarriesEveryPlanFieldOnTheWire pins the plan's
// JSON shape against a result that actually HAS entries.
//
// Exercised through platformPruneResponse directly rather than through the
// HTTP handler, and that is the point: the router's fixture is an empty
// library, so a handler-level test can only assert "every entry that appears
// carries an outcome" -- which is vacuously true of zero entries and would
// pass with the field deleted from the encoder entirely. Feeding a populated
// result is what gives the assertion teeth.
func TestPlatformPruneResponse_CarriesEveryPlanFieldOnTheWire(t *testing.T) {
	t.Parallel()
	body := platformPruneResponse(publish.PlatformBackdropPruneResult{
		ArtistsProcessed: 1,
		BackdropsRemoved: 1,
		SkippedChanged:   1,
		Plan: []publish.PlatformBackdropPrunePlanEntry{
			{ArtistID: "a1", ConnectionID: "c1", Index: 2, Survivor: 0, Outcome: publish.PrunePlanDeleted},
			{ArtistID: "a1", ConnectionID: "c1", Index: 1, Survivor: 0, Outcome: publish.PrunePlanSkipped},
		},
	})
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshaling the response: %v", err)
	}
	var got struct {
		Plan []struct {
			ArtistID     string `json:"artist_id"`
			ConnectionID string `json:"connection_id"`
			Index        int    `json:"index"`
			Survivor     int    `json:"survivor"`
			Outcome      string `json:"outcome"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	if len(got.Plan) != 2 {
		t.Fatalf("plan has %d entries on the wire, want 2", len(got.Plan))
	}
	wantOutcomes := []string{publish.PrunePlanDeleted, publish.PrunePlanSkipped}
	for i, e := range got.Plan {
		if e.ArtistID != "a1" || e.ConnectionID != "c1" {
			t.Errorf("entry %d lost its identity on the wire: %+v", i, e)
		}
		if e.Outcome != wantOutcomes[i] {
			t.Errorf("entry %d: outcome %q, want %q; without it a caller cannot tell which entries describe their library", i, e.Outcome, wantOutcomes[i])
		}
		if e.Survivor != 0 {
			t.Errorf("entry %d: survivor %d, want 0", i, e.Survivor)
		}
	}
	if got.Plan[0].Index != 2 || got.Plan[1].Index != 1 {
		t.Errorf("plan indices = %d,%d, want 2,1", got.Plan[0].Index, got.Plan[1].Index)
	}
}
