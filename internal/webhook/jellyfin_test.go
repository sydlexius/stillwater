package webhook

import (
	"encoding/json"
	"testing"
)

func TestJellyfinPayload_Decode_TestEvent(t *testing.T) {
	raw := `{"NotificationType":"Test"}`
	var p JellyfinPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.NotificationType != JellyfinEventTest {
		t.Errorf("NotificationType = %q, want %q", p.NotificationType, JellyfinEventTest)
	}
}

func TestJellyfinPayload_Decode_ItemAdded_WithMBID(t *testing.T) {
	raw := `{
		"NotificationType": "ItemAdded",
		"ItemId": "def456",
		"ItemType": "MusicArtist",
		"Name": "Portishead",
		"Provider_musicbrainzartist": "8f6bd1e4-fbe1-45ad-a2d6-b5c12da4c4a0"
	}`
	var p JellyfinPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.NotificationType != JellyfinEventItemAdded {
		t.Errorf("NotificationType = %q", p.NotificationType)
	}
	if p.Name != "Portishead" {
		t.Errorf("Name = %q", p.Name)
	}
	if p.ItemType != "MusicArtist" {
		t.Errorf("ItemType = %q", p.ItemType)
	}
	if got := p.MBID(); got != "8f6bd1e4-fbe1-45ad-a2d6-b5c12da4c4a0" {
		t.Errorf("MBID() = %q", got)
	}
}

func TestJellyfinPayload_MBID_EmptyWhenAbsent(t *testing.T) {
	p := JellyfinPayload{
		NotificationType: JellyfinEventItemAdded,
		ItemType:         "MusicArtist",
		Name:             "Unknown",
	}
	if got := p.MBID(); got != "" {
		t.Errorf("MBID() = %q, want empty string", got)
	}
}

func TestJellyfinPayload_NonMusicArtistType(t *testing.T) {
	raw := `{
		"NotificationType": "ItemAdded",
		"ItemId": "xyz",
		"ItemType": "MusicAlbum",
		"Name": "Some Album",
		"Provider_musicbrainzartist": "should-not-match"
	}`
	var p JellyfinPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.ItemType == "MusicArtist" {
		t.Error("expected non-MusicArtist type to not be treated as artist")
	}
}

func TestJellyfinPayload_LibraryChanged(t *testing.T) {
	raw := `{"NotificationType":"LibraryChanged"}`
	var p JellyfinPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.NotificationType != JellyfinEventLibraryChanged {
		t.Errorf("NotificationType = %q", p.NotificationType)
	}
}

func TestJellyfinPayload_ItemUpdated(t *testing.T) {
	raw := `{
		"NotificationType": "ItemUpdated",
		"ItemType": "MusicArtist",
		"Name": "The National",
		"Provider_musicbrainzartist": "2ae57d56-c96c-4ead-a9d2-4b3b5c47bba5"
	}`
	var p JellyfinPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.NotificationType != JellyfinEventItemUpdated {
		t.Errorf("NotificationType = %q", p.NotificationType)
	}
	if got := p.MBID(); got != "2ae57d56-c96c-4ead-a9d2-4b3b5c47bba5" {
		t.Errorf("MBID() = %q", got)
	}
}
