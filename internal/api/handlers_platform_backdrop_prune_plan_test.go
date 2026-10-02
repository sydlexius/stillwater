package api

import (
	"encoding/json"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

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
			{ArtistID: "a1", ConnectionID: "c1", Index: 2, Survivor: 0, Tier: publish.PruneTierExact, Outcome: publish.PrunePlanDeleted},
			{ArtistID: "a1", ConnectionID: "c1", Index: 1, Survivor: 3, Tier: publish.PruneTierPerceptual, Outcome: publish.PrunePlanSkipped},
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
			Tier         string `json:"tier"`
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
	wantTiers := []string{publish.PruneTierExact, publish.PruneTierPerceptual}
	wantSurvivors := []int{0, 3}
	for i, e := range got.Plan {
		if e.ArtistID != "a1" || e.ConnectionID != "c1" {
			t.Errorf("entry %d lost its identity on the wire: %+v", i, e)
		}
		if e.Outcome != wantOutcomes[i] {
			t.Errorf("entry %d: outcome %q, want %q; without it a caller cannot tell which entries describe their library", i, e.Outcome, wantOutcomes[i])
		}
		if e.Survivor != wantSurvivors[i] {
			t.Errorf("entry %d: survivor %d, want %d", i, e.Survivor, wantSurvivors[i])
		}
		if e.Tier != wantTiers[i] {
			t.Errorf("entry %d: tier %q, want %q; the operator must see which deletes rest on a similarity judgement", i, e.Tier, wantTiers[i])
		}
	}
	if got.Plan[0].Index != 2 || got.Plan[1].Index != 1 {
		t.Errorf("plan indices = %d,%d, want 2,1", got.Plan[0].Index, got.Plan[1].Index)
	}
}

// TestPlatformPruneSpec_TierEnumMatchesThePublisher pins the published `tier`
// enum to the publisher's own constants, so neither can change without the
// other: a client generated from the spec must accept every tier the server
// emits, and nothing else.
func TestPlatformPruneSpec_TierEnumMatchesThePublisher(t *testing.T) {
	t.Parallel()
	doc, err := openapi3.NewLoader().LoadFromData(openapiSpec)
	if err != nil {
		t.Fatalf("loading the embedded spec: %v", err)
	}
	op := doc.Paths.Find("/reports/platform-backdrop-duplicates/prune").Post
	// The 500 body carries the same plan as the 200 (#3328 review), so both
	// declare the same tier enum.
	for _, status := range []int{200, 500} {
		plan := op.Responses.Status(status).Value.Content.Get("application/json").Schema.Value.Properties["plan"].Value
		tier := plan.Items.Value.Properties["tier"]
		if tier == nil {
			t.Fatalf("%d: plan items carry no `tier` property in the spec", status)
		}
		got := map[any]bool{}
		for _, v := range tier.Value.Enum {
			got[v] = true
		}
		want := map[any]bool{publish.PruneTierExact: true, publish.PruneTierPerceptual: true}
		if len(got) != len(want) {
			t.Fatalf("%d: tier enum %v, want exactly %v", status, tier.Value.Enum, want)
		}
		for v := range want {
			if !got[v] {
				t.Errorf("%d: tier enum %v is missing %q", status, tier.Value.Enum, v)
			}
		}
	}
}
