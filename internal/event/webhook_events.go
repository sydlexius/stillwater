package event

import "slices"

// webhookEventTypes is the canonical, ordered set of event types a webhook may
// subscribe to. It is the single source of truth for three places that
// previously held independent copies and could silently drift (#2009 #6):
//
//   - cmd/stillwater/main.go subscribes the webhook dispatcher to exactly these
//     types (an event not in this list can never reach a webhook);
//   - internal/api/handlers_webhook.go validates subscription requests against
//     it (IsWebhookEvent), rejecting unknown event types instead of silently
//     accepting a typo that would never fire;
//   - internal/api/openapi.yaml documents these as the `events` enum, kept in
//     lockstep by a drift test.
//
// Keep the order meaningful (grouped by domain); openapi enum order is asserted
// to match. It is unexported so the registry cannot be mutated (reordered or
// appended) from another package, which would silently change webhook
// validation and the openapi enum ordering; callers read it via the accessors
// below, each of which returns a fresh copy.
var webhookEventTypes = []Type{
	ArtistNew, MetadataFixed, ReviewNeeded,
	RuleViolation, BulkCompleted, ScanCompleted,
	LidarrArtistAdd, LidarrDownload,
	EmbyArtistUpdate, EmbyLibraryScan,
	JellyfinArtistUpdate, JellyfinLibraryScan,
	FSDirCreated, FSDirRemoved, FSUnexpectedWrite,
}

// WebhookEventTypes returns the canonical, ordered set of subscribable webhook
// event types. It returns a copy, so callers cannot mutate the registry.
func WebhookEventTypes() []Type {
	return slices.Clone(webhookEventTypes)
}

// IsWebhookEvent reports whether s is a subscribable webhook event type.
func IsWebhookEvent(s string) bool {
	for _, t := range webhookEventTypes {
		if string(t) == s {
			return true
		}
	}
	return false
}

// WebhookEventStrings returns the registry as a string slice, in order.
func WebhookEventStrings() []string {
	out := make([]string, len(webhookEventTypes))
	for i, t := range webhookEventTypes {
		out[i] = string(t)
	}
	return out
}
