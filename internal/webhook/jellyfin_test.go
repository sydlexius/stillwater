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
	// Jellyfin sends MusicAlbum items (not MusicArtist); MBID is the album artist ID.
	// Confirmed via UAT against Jellyfin webhook plugin v18.
	raw := `{
		"NotificationType": "ItemAdded",
		"ItemId": "def456",
		"ItemType": "MusicAlbum",
		"Name": "Dummy's Guide to Danger",
		"Artist": "Fade Runner",
		"Provider_musicbrainzalbumartist": "aa95e459-3cce-42a8-b9e3-2526b4359297"
	}`
	var p JellyfinPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.NotificationType != JellyfinEventItemAdded {
		t.Errorf("NotificationType = %q", p.NotificationType)
	}
	if p.Name != "Dummy's Guide to Danger" {
		t.Errorf("Name = %q", p.Name)
	}
	if p.ItemType != "MusicAlbum" {
		t.Errorf("ItemType = %q", p.ItemType)
	}
	if p.Artist != "Fade Runner" {
		t.Errorf("Artist = %q", p.Artist)
	}
	if got := p.MBID(); got != "aa95e459-3cce-42a8-b9e3-2526b4359297" {
		t.Errorf("MBID() = %q", got)
	}
}

func TestJellyfinPayload_MBID_EmptyWhenAbsent(t *testing.T) {
	p := JellyfinPayload{
		NotificationType: JellyfinEventItemAdded,
		ItemType:         "MusicAlbum",
		Name:             "Unknown",
	}
	if got := p.MBID(); got != "" {
		t.Errorf("MBID() = %q, want empty string", got)
	}
}

func TestJellyfinPayload_NonMusicAlbumType(t *testing.T) {
	raw := `{
		"NotificationType": "ItemAdded",
		"ItemId": "xyz",
		"ItemType": "Movie",
		"Name": "Some Movie"
	}`
	var p JellyfinPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.ItemType == "MusicAlbum" {
		t.Error("expected non-MusicAlbum type")
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
		"ItemType": "MusicAlbum",
		"Name": "Some Album",
		"Artist": "The National",
		"Provider_musicbrainzalbumartist": "2ae57d56-c96c-4ead-a9d2-4b3b5c47bba5"
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
