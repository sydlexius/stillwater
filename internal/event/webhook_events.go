package event

// WebhookEventTypes is the canonical, ordered set of event types a webhook may
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
// to match.
var WebhookEventTypes = []Type{
	ArtistNew, MetadataFixed, ReviewNeeded,
	RuleViolation, BulkCompleted, ScanCompleted,
	LidarrArtistAdd, LidarrDownload,
	EmbyArtistUpdate, EmbyLibraryScan,
	JellyfinArtistUpdate, JellyfinLibraryScan,
	FSDirCreated, FSDirRemoved, FSUnexpectedWrite,
}

// IsWebhookEvent reports whether s is a subscribable webhook event type.
func IsWebhookEvent(s string) bool {
	for _, t := range WebhookEventTypes {
		if string(t) == s {
			return true
		}
	}
	return false
}

// WebhookEventStrings returns WebhookEventTypes as a string slice, in order.
func WebhookEventStrings() []string {
	out := make([]string, len(WebhookEventTypes))
	for i, t := range WebhookEventTypes {
		out[i] = string(t)
	}
	return out
}
