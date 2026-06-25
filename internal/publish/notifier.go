package publish

import (
	"github.com/sydlexius/stillwater/internal/event"
)

// busNotifier adapts an *event.Bus to the publish.Notifier interface so
// per-connection goroutine failures from the publisher become
// event.ConnectionPushFailed events. The SSE hub then broadcasts those
// events to connected clients as toasts.
type busNotifier struct {
	bus *event.Bus
}

// NewBusNotifier constructs a Notifier backed by the given event bus.
// A nil bus is accepted and produces a no-op notifier so wiring at
// startup does not have to special-case the test/headless paths.
func NewBusNotifier(bus *event.Bus) Notifier {
	return &busNotifier{bus: bus}
}

// NotifyConnectionPushFailed implements Notifier. The raw err.Error() is
// deliberately NOT placed onto the published event: Go's *url.Error
// includes the full method+URL ("Post http://emby.internal.lan:8096/..."),
// and 4xx/5xx response bodies can carry tokens or internal hostnames; the
// SSE hub broadcasts Data to every connected client, so a verbatim error
// would leak that surface to anyone with DevTools open. The detailed
// error stays in the server-side slog.Error call the caller emits before
// invoking the notifier; the toast only sees connection + error class +
// artist context.
//
// connectionID is the raw connection UUID threaded through from the publisher
// so the frontend can construct a deep-link to the connection edit panel
// (e.g. /settings?tab=connections&edit=<id>&focus=api_key). It is omitted
// from the payload when empty (connection lookup failures fall back to a
// short-label name only).
//
// artistID / artistName / operation are optional context: PushLocks fans
// out one goroutine per platform mapping, so a single artist failing
// across N platforms otherwise produces N anonymous toasts. operation is
// a short slug ("lock_toggle") so the UI can disambiguate the originating
// action when more push surfaces gain notifier coverage (PR follow-up).
func (n *busNotifier) NotifyConnectionPushFailed(connectionID, connectionName, errorClass, artistID, artistName, operation string, err error) {
	if n == nil || n.bus == nil {
		return
	}
	// err is accepted for API symmetry with the publisher's logging path
	// but is not placed on the event Data; see the function-level comment
	// for why.
	_ = err
	data := map[string]any{
		"connection":  connectionName,
		"error_class": errorClass,
	}
	if connectionID != "" {
		data["connection_id"] = connectionID
	}
	if artistID != "" {
		data["artist_id"] = artistID
	}
	if artistName != "" {
		data["artist_name"] = artistName
	}
	if operation != "" {
		data["operation"] = operation
	}
	n.bus.Publish(event.Event{
		Type: event.ConnectionPushFailed,
		Data: data,
	})
}
