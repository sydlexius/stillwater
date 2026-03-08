package webhook

import (
	"encoding/json"
	"testing"
)

func TestEmbyPayload_Decode_TestEvent(t *testing.T) {
	raw := `{"NotificationType":"test"}`
	var p EmbyPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.NotificationType != EmbyEventTest {
		t.Errorf("NotificationType = %q, want %q", p.NotificationType, EmbyEventTest)
	}
	if p.Item != nil {
		t.Error("Item should be nil for test event")
	}
}

func TestEmbyPayload_Decode_ItemAdded_WithMBID(t *testing.T) {
	raw := `{
		"NotificationType": "ItemAdded",
		"Item": {
			"Id": "abc123",
			"Name": "Radiohead",
			"Type": "MusicArtist",
			"ProviderIds": {"MusicBrainzArtist": "a74b1b7f-71a5-4011-9441-d0b5e4122711"},
			"Path": "/music/Radiohead"
		}
	}`
	var p EmbyPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.NotificationType != EmbyEventItemAdded {
		t.Errorf("NotificationType = %q", p.NotificationType)
	}
	if p.Item == nil {
		t.Fatal("Item is nil")
	}
	if p.Item.Name != "Radiohead" {
		t.Errorf("Name = %q", p.Item.Name)
	}
	if p.Item.Type != "MusicArtist" {
		t.Errorf("Type = %q", p.Item.Type)
	}
	if got := p.Item.MBID(); got != "a74b1b7f-71a5-4011-9441-d0b5e4122711" {
		t.Errorf("MBID() = %q", got)
	}
}

func TestEmbyPayload_MBID_EmptyWhenNoProviderIds(t *testing.T) {
	item := &EmbyItem{Name: "Unknown Artist", Type: "MusicArtist"}
	if got := item.MBID(); got != "" {
		t.Errorf("MBID() = %q, want empty string", got)
	}
}

func TestEmbyPayload_MBID_EmptyWhenKeyAbsent(t *testing.T) {
	item := &EmbyItem{
		Name:        "Unknown Artist",
		Type:        "MusicArtist",
		ProviderIds: map[string]string{"Imvdb": "someid"},
	}
	if got := item.MBID(); got != "" {
		t.Errorf("MBID() = %q, want empty string", got)
	}
}

func TestEmbyPayload_NonMusicArtistType(t *testing.T) {
	raw := `{
		"NotificationType": "ItemAdded",
		"Item": {
			"Id": "xyz",
			"Name": "Some Album",
			"Type": "MusicAlbum",
			"ProviderIds": {"MusicBrainzArtist": "should-not-match"}
		}
	}`
	var p EmbyPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Item.Type == "MusicArtist" {
		t.Error("expected non-MusicArtist type to not be treated as artist")
	}
}

func TestEmbyPayload_LibraryChanged(t *testing.T) {
	raw := `{"NotificationType":"LibraryChanged"}`
	var p EmbyPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.NotificationType != EmbyEventLibraryChanged {
		t.Errorf("NotificationType = %q", p.NotificationType)
	}
}
