package api

import (
	"os"
	"testing"

	"github.com/sydlexius/stillwater/internal/event"
	"gopkg.in/yaml.v3"
)

// TestWebhookEventEnumMatchesRegistry guards #2009 #6: the openapi.yaml Webhook
// schema `events` enum must match event.WebhookEventTypes exactly (same values,
// same order). The registry is the single source of truth (main.go subscribes
// the dispatcher and handlers_webhook.go validates against it); this keeps the
// published contract from drifting away from what the server actually accepts
// and dispatches.
func TestWebhookEventEnumMatchesRegistry(t *testing.T) {
	data, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatalf("reading openapi.yaml: %v", err)
	}
	var doc struct {
		Components struct {
			Schemas struct {
				Webhook struct {
					Properties struct {
						Events struct {
							Items struct {
								Enum []string `yaml:"enum"`
							} `yaml:"items"`
						} `yaml:"events"`
					} `yaml:"properties"`
				} `yaml:"Webhook"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parsing openapi.yaml: %v", err)
	}

	got := doc.Components.Schemas.Webhook.Properties.Events.Items.Enum
	want := event.WebhookEventStrings()
	if len(got) == 0 {
		t.Fatal("openapi Webhook.events.items.enum is empty -- the enum is missing or the schema path changed")
	}
	if len(got) != len(want) {
		t.Fatalf("openapi Webhook events enum has %d values, registry has %d:\n  openapi: %v\n  registry: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("openapi Webhook events enum[%d] = %q, registry = %q (regenerate the enum from event.WebhookEventTypes)", i, got[i], want[i])
		}
	}
}

// TestFirstInvalidWebhookEvent exercises the API-boundary validation predicate
// used by the create/update webhook handlers.
func TestFirstInvalidWebhookEvent(t *testing.T) {
	t.Run("all valid", func(t *testing.T) {
		if bad, ok := firstInvalidWebhookEvent(event.WebhookEventStrings()); !ok {
			t.Errorf("firstInvalidWebhookEvent(all valid) = (%q, false); want ('', true)", bad)
		}
	})
	t.Run("empty list", func(t *testing.T) {
		if _, ok := firstInvalidWebhookEvent(nil); !ok {
			t.Error("firstInvalidWebhookEvent(nil) = (_, false); want (_, true) -- empty subscription is allowed")
		}
	})
	t.Run("one invalid", func(t *testing.T) {
		bad, ok := firstInvalidWebhookEvent([]string{"artist.new", "totally.bogus", "metadata.fixed"})
		if ok || bad != "totally.bogus" {
			t.Errorf("firstInvalidWebhookEvent = (%q, %v); want ('totally.bogus', false)", bad, ok)
		}
	})
}
