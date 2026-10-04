package provider

import (
	"context"
	"log/slog"
)

// Cause classes: WHAT KIND of thing started a piece of work. The outermost
// trigger owns the class; see EnrichCause.
const (
	CauseClassRule      = "rule"
	CauseClassScheduled = "scheduled"
	CauseClassUser      = "user"
	CauseClassBulk      = "bulk"

	// CauseClassScan: a library scan (set where a scan is started).
	CauseClassScan = "scan"
	// CauseClassWatcher: the filesystem watcher reacting to a change on disk.
	CauseClassWatcher = "watcher"
	// The webhook classes name the sender. The source is part of the class
	// (not the detail) because EnrichCause replaces the detail, so a source
	// carried there would be lost once a rule or scan enriches the context.
	// Set in the matching webhook handler.
	CauseClassWebhookLidarr   = "webhook.lidarr"
	CauseClassWebhookEmby     = "webhook.emby"
	CauseClassWebhookJellyfin = "webhook.jellyfin"
	// CauseClassHealth: a background health check (set where the check runs).
	CauseClassHealth = "health"
	// CauseClassSweep: a background sweep job (set where the sweep starts).
	CauseClassSweep = "sweep"
)

// causeLogKey is the log attribute the cause is emitted under, and
// causeUnattributed is the value written when the context carries no cause.
// The marker is explicit on purpose: a blank or missing attribute would read
// as if attribution had succeeded and simply had nothing to say (#2784).
const (
	causeLogKey       = "cause"
	causeUnattributed = "unattributed"
)

// ctxKeyCause is the context key for the operation cause.
type ctxKeyCause struct{}

// Cause says why a piece of work is happening: the Class of trigger and the
// specific instance in Detail (which rule, which route, which job type). The
// zero value means "no cause known".
type Cause struct {
	Class  string
	Detail string
}

// String renders the cause as "class:detail", or just the non-empty half when
// only one is set. The zero value renders as the empty string.
func (c Cause) String() string {
	if c.Class == "" || c.Detail == "" {
		return c.Class + c.Detail
	}
	return c.Class + ":" + c.Detail
}

// WithCause returns a child context carrying c, replacing any cause already
// present. Use it at an outermost trigger (a scheduler tick, a request); an
// inner layer that only knows a more specific instance uses EnrichCause.
func WithCause(ctx context.Context, c Cause) context.Context {
	return context.WithValue(ctx, ctxKeyCause{}, c)
}

// CauseFromContext retrieves the cause from the context. It returns the zero
// Cause when none has been set and never panics.
func CauseFromContext(ctx context.Context) Cause {
	c, _ := ctx.Value(ctxKeyCause{}).(Cause)
	return c
}

// CarryCause returns dst carrying src's cause when src has one, and dst
// unchanged otherwise. Go contexts only inherit values from their parent, so
// work handed to a context that is not derived from the caller (the scanner
// runs on a shutdown-scoped context) loses the cause unless it is copied by
// hand. Only the cause crosses over: not src's cancellation, its deadline, or
// any of its other values. Both contexts must be non-nil, as with the standard
// library's context functions (a nil src panics).
func CarryCause(dst, src context.Context) context.Context {
	c := CauseFromContext(src)
	if c == (Cause{}) {
		return dst
	}
	return WithCause(dst, c)
}

// EnrichCause is for an inner layer that knows a specific instance (the rule
// being fixed, the bulk job type) but not who started the run. It keeps the
// Class an outer trigger already set and writes the instance into Detail as
// "class:detail", so a scheduled tick fixing rule X reads
// "scheduled:rule:X". With no outer cause the instance becomes the cause
// itself ("rule:X"). An existing Detail is replaced, not appended to.
// When the existing class is the class being added and the new detail is
// non-empty, the detail is replaced rather than nested, so rule A enriched with
// rule B reads "rule:B", not "rule:rule:B". When the class matches and the new
// detail is empty, the context is returned unchanged, because that adds no
// information and must not erase the detail already there.
func EnrichCause(ctx context.Context, class, detail string) context.Context {
	inner := Cause{Class: class, Detail: detail}
	outer := CauseFromContext(ctx)
	if outer.Class == "" {
		return WithCause(ctx, inner)
	}
	if outer.Class == class {
		if detail == "" {
			return ctx
		}
		return WithCause(ctx, inner)
	}
	return WithCause(ctx, Cause{Class: outer.Class, Detail: inner.String()})
}

// causeAttr is the package-internal spelling of CauseAttr, kept so the shared
// fetch points need no change.
func causeAttr(ctx context.Context) slog.Attr { return CauseAttr(ctx) }

// CauseAttr renders the context's cause as a log attribute. It is the single
// owner of the unattributed marker. Each shared fetch point calls it once and
// reuses the result on the lines it already emits; adapters never call it.
//
// Limits, so nobody reads more into it: those lines are emitted only on a
// not-found, an error or a skip, so a fetch that succeeds logs no cause; and a
// call that bypasses the shared fetch points (GetReleaseGroups, the #2476
// path) logs no cause at all.
func CauseAttr(ctx context.Context) slog.Attr {
	if s := CauseFromContext(ctx).String(); s != "" {
		return slog.String(causeLogKey, s)
	}
	return slog.String(causeLogKey, causeUnattributed)
}
