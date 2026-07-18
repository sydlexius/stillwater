package image

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRepairEntry_PlatformTargets_BackCompat proves the additive
// PlatformTargets field does not break manifests written before it existed:
// a legacy manifest with no "platform_targets" key must still decode, with the
// field left nil (not an error, not a synthesized entry).
func TestRepairEntry_PlatformTargets_BackCompat(t *testing.T) {
	// A manifest exactly as an earlier Stillwater version would have written it:
	// every documented field EXCEPT platform_targets, which did not exist yet.
	legacy := `{
	  "op_id": "op-legacy",
	  "created_at": "2026-01-01T00:00:00Z",
	  "entries": [
	    {
	      "artist_id": "a1",
	      "artist_name": "Legacy Artist",
	      "image_type": "fanart",
	      "slot_index": 2,
	      "file_name": "fanart2.jpg",
	      "stored_name": "002-fanart2.jpg",
	      "phash": "deadbeefdeadbeef",
	      "matched_artist_id": "b1",
	      "matched_artist_name": "Other Artist",
	      "similarity": 0.97,
	      "quarantined_at": "2026-01-01T00:00:00Z"
	    }
	  ]
	}`

	var m RepairManifest
	if err := json.Unmarshal([]byte(legacy), &m); err != nil {
		t.Fatalf("legacy manifest without platform_targets failed to decode: %v", err)
	}
	if len(m.Entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(m.Entries))
	}
	e := m.Entries[0]
	// The new field must be absent (nil), never a zero-length non-nil or a
	// fabricated target -- a legacy entry names no platform to restore into.
	if e.PlatformTargets != nil {
		t.Errorf("PlatformTargets: want nil for a legacy manifest, got %#v", e.PlatformTargets)
	}
	// A spot-check that the rest of the entry still decoded, so the assertion
	// above is proven against a genuinely-parsed entry and not an empty struct
	// produced by a silent decode failure.
	if e.FileName != "fanart2.jpg" || e.PHash != "deadbeefdeadbeef" || e.SlotIndex != 2 {
		t.Errorf("legacy entry decoded wrong: %+v", e)
	}
}

// TestRepairEntry_PlatformTargets_RoundTrip proves the field survives a
// marshal/unmarshal cycle intact and that an entry carrying no targets encodes
// WITHOUT the key (omitempty), so writing a manifest for a purely-local removal
// stays byte-compatible with the legacy shape above.
func TestRepairEntry_PlatformTargets_RoundTrip(t *testing.T) {
	withTargets := RepairEntry{
		ArtistID:  "a1",
		ImageType: "fanart",
		SlotIndex: 0,
		FileName:  "fanart.jpg",
		PlatformTargets: []RepairPlatformTarget{
			{ConnectionID: "c-emby", PlatformArtistID: "p1"},
			{ConnectionID: "c-jf", PlatformArtistID: "p2"},
		},
	}
	data, err := json.Marshal(withTargets)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back RepairEntry
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(back.PlatformTargets) != 2 ||
		back.PlatformTargets[0] != (RepairPlatformTarget{ConnectionID: "c-emby", PlatformArtistID: "p1"}) ||
		back.PlatformTargets[1] != (RepairPlatformTarget{ConnectionID: "c-jf", PlatformArtistID: "p2"}) {
		t.Errorf("round-trip lost targets: %#v", back.PlatformTargets)
	}

	// omitempty: an entry with no targets must not emit the key at all.
	noTargets := RepairEntry{ArtistID: "a1", ImageType: "fanart", FileName: "fanart.jpg"}
	data, err = json.Marshal(noTargets)
	if err != nil {
		t.Fatalf("marshal no-targets: %v", err)
	}
	if got := string(data); strings.Contains(got, `"platform_targets"`) {
		t.Errorf("empty PlatformTargets must be omitted, got %s", got)
	}
}
