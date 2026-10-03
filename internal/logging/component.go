package logging

import (
	"context"
	"log/slog"
)

// componentKey is the structured-log attribute that names the emitting
// subsystem. The log viewer filters on it, so it must appear at most once per
// record.
const componentKey = "component"

// PreviousComponentKey is the attribute key on the Error record WithComponent
// writes when it re-tags an already-tagged logger. Tests assert on this key
// rather than on the message text, so rewording the message cannot silently
// turn them vacuous.
const PreviousComponentKey = "previous_component"

// componentHandler stamps exactly one component attribute on every record.
//
// slog's Logger.With appends attributes and never deduplicates a key, so
// tagging a logger that already carries a component (a service that tags its
// logger and then hands it to a sub-component constructor that tags it again)
// emits "component" twice on the same line (#2787). Instead of appending at
// With time, this handler holds the name and adds it once at Handle time; a
// second WithComponent replaces the name rather than stacking it.
//
// The attribute is added to the record, so it is emitted at the top level
// unless a group was opened on the logger beforehand; nothing in the tree
// groups a component-tagged logger.
type componentHandler struct {
	inner     slog.Handler
	component string
}

func (h *componentHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *componentHandler) Handle(ctx context.Context, r slog.Record) error {
	r = r.Clone()
	r.AddAttrs(slog.String(componentKey, h.component))
	return h.inner.Handle(ctx, r)
}

func (h *componentHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &componentHandler{inner: h.inner.WithAttrs(attrs), component: h.component}
}

func (h *componentHandler) WithGroup(name string) slog.Handler {
	return &componentHandler{inner: h.inner.WithGroup(name), component: h.component}
}

// WithComponent returns l tagged with the given component name. A second
// WithComponent on the result replaces the name rather than stacking it. It
// does not defend against a raw .With("component", ...) on either side of it,
// or a component attribute passed on an individual log call: those still
// produce a second key.
//
// Re-tagging an already-tagged logger is a wiring bug: the sub-component should
// receive the untagged base logger. It is not fatal, so the new name replaces
// the old one (no duplicate key is ever emitted) and an Error record naming
// both is written so the mistake is visible and greppable.
func WithComponent(l *slog.Logger, name string) *slog.Logger {
	if l == nil {
		l = slog.Default()
	}
	if prev, ok := l.Handler().(*componentHandler); ok {
		next := slog.New(&componentHandler{inner: prev.inner, component: name})
		if prev.component != name {
			next.Error("logger re-tagged with a different component; pass the untagged base logger to sub-components",
				slog.String(PreviousComponentKey, prev.component),
				slog.String("new_component", name))
		}
		return next
	}
	return slog.New(&componentHandler{inner: l.Handler(), component: name})
}
