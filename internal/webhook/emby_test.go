package webhook

import (
	"encoding/json"
	"testing"
)

func TestEmbyPayload_Decode_TestEvent(t *testing.T) {
	raw := `{"Event":"system.notificationtest"}`
	var p EmbyPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Event != EmbyEventTest {
		t.Errorf("Event = %q, want %q", p.Event, EmbyEventTest)
	}
	if p.Item != nil {
		t.Error("Item should be nil for test event")
	}
}

func TestEmbyPayload_Decode_ItemAdded_WithMBIDs(t *testing.T) {
	raw := `{
		"Event": "library.new",
		"Item": {
			"Id": "abc123",
			"Name": "Wall You Need Is Love",
			"Type": "MusicAlbum",
			"ProviderIds": {"MusicBrainzAlbumArtist": "265349d7-376f-4b0d-98bb-6a4791ba9f4c/aa95e459-3cce-42a8-b9e3-2526b4359297"},
			"ArtistItems": [{"Id": "10000", "Name": "???"}, {"Id": "10001", "Name": "Fade Runner"}]
		}
	}`
	var p EmbyPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Event != EmbyEventItemAdded {
		t.Errorf("Event = %q", p.Event)
	}
	if p.Item == nil {
		t.Fatal("Item is nil")
	}
	if p.Item.Type != "MusicAlbum" {
		t.Errorf("Type = %q", p.Item.Type)
	}
	mbids := p.Item.ArtistMBIDs()
	if len(mbids) != 2 {
		t.Fatalf("ArtistMBIDs() len = %d, want 2", len(mbids))
	}
	if mbids[0] != "265349d7-376f-4b0d-98bb-6a4791ba9f4c" {
		t.Errorf("mbids[0] = %q", mbids[0])
	}
	if mbids[1] != "aa95e459-3cce-42a8-b9e3-2526b4359297" {
		t.Errorf("mbids[1] = %q", mbids[1])
	}
	if len(p.Item.ArtistItems) != 2 {
		t.Errorf("ArtistItems len = %d, want 2", len(p.Item.ArtistItems))
	}
}

func TestEmbyPayload_ArtistMBIDs_EmptyWhenNoProviderIds(t *testing.T) {
	item := EmbyItem{Name: "Some Album", Type: "MusicAlbum"}
	if got := item.ArtistMBIDs(); got != nil {
		t.Errorf("ArtistMBIDs() = %v, want nil", got)
	}
}

func TestEmbyPayload_ArtistMBIDs_EmptyWhenKeyAbsent(t *testing.T) {
	item := EmbyItem{
		Name:        "Some Album",
		Type:        "MusicAlbum",
		ProviderIds: map[string]string{"MusicBrainzAlbum": "some-album-id"},
	}
	if got := item.ArtistMBIDs(); got != nil {
		t.Errorf("ArtistMBIDs() = %v, want nil", got)
	}
}

func TestEmbyPayload_ArtistMBIDs_SingleArtist(t *testing.T) {
	item := EmbyItem{
		Name:        "Some Album",
		Type:        "MusicAlbum",
		ProviderIds: map[string]string{"MusicBrainzAlbumArtist": "a74b1b7f-71a5-4011-9441-d0b5e4122711"},
	}
	mbids := item.ArtistMBIDs()
	if len(mbids) != 1 || mbids[0] != "a74b1b7f-71a5-4011-9441-d0b5e4122711" {
		t.Errorf("ArtistMBIDs() = %v", mbids)
	}
}

func TestEmbyPayload_LibraryChanged(t *testing.T) {
	raw := `{"Event":"library.changed"}`
	var p EmbyPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Event != EmbyEventLibraryChanged {
		t.Errorf("Event = %q", p.Event)
	}
}
