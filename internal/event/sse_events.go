package event

// SSEForwardedTypes is the canonical set of event types that are forwarded to
// connected SSE clients -- the "forwarded" half of the internal-vs-forwarded
// split (#2009 #12). Every other event.Type is internal-only (consumed by
// in-process subscribers such as the FSCache invalidator, health/dirty
// subscribers, or the webhook dispatcher) and is deliberately NOT streamed to
// browsers.
//
// Most of these are forwarded by the SSE hub (SSEHub.SubscribeToEventBus in
// internal/api/handlers_sse.go); LogsLine and LogsThrottled are forwarded on
// the dedicated logs stream (#1338) instead. This set is the source of truth
// for the SSE event catalog doc
// (docs/site/src/contributing/architecture/sse-events.md), kept in lockstep by
// TestSSECatalogMatchesForwardedSet, and for the hub coverage check
// (TestSSEHubForwardsCanonicalSet).
var SSEForwardedTypes = []Type{
	ScanCompleted,
	RuleViolation,
	BulkCompleted,
	ArtistNew,
	ArtistUpdated,
	MetadataFixed,
	ConflictChanged,
	OperationProgress,
	ConnectionPushFailed,
	BackdropCollision,
	ActivityRecent,
	SettingsChanged,
	DashboardActionResolved,
	LogsLine,
	LogsThrottled,
}
