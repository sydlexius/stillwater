package event

import "testing"

func TestIsWebhookEvent(t *testing.T) {
	// Every canonical type is accepted.
	for _, ty := range WebhookEventTypes() {
		if !IsWebhookEvent(string(ty)) {
			t.Errorf("IsWebhookEvent(%q) = false; want true (it is in WebhookEventTypes())", ty)
		}
	}
	// Non-subscribable / unknown types are rejected.
	for _, bad := range []string{
		"", "bogus.event", "test",
		string(SettingsChanged), // emitted internally but NOT webhook-subscribable
		string(LogsLine),        // SSE-only
		"ARTIST.NEW",            // case-sensitive
	} {
		if IsWebhookEvent(bad) {
			t.Errorf("IsWebhookEvent(%q) = true; want false", bad)
		}
	}
}

func TestWebhookEventStrings(t *testing.T) {
	got := WebhookEventStrings()
	want := WebhookEventTypes()
	if len(got) != len(want) {
		t.Fatalf("WebhookEventStrings() len = %d, want %d", len(got), len(want))
	}
	for i, ty := range want {
		if got[i] != string(ty) {
			t.Errorf("WebhookEventStrings()[%d] = %q, want %q (order must match WebhookEventTypes())", i, got[i], ty)
		}
	}
}
