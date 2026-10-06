package templates

import (
	"context"
	"log/slog"
	"sync"

	"github.com/sydlexius/stillwater/internal/i18n"
)

// extraFanartUnknownSeen records the codes already logged, so a code the
// templates do not know is reported once per process rather than once per row.
var extraFanartUnknownSeen sync.Map

// extraFanartLabel returns the display text for an outcome, reason or status
// code of the extrafanart migration (kind is "outcome", "reason" or "status").
// The keys are built from the code, so drift_test.go cannot see them; a code the
// server adds without copy would otherwise render as a bare key such as
// "extrafanart_migration.reason_new_code". An unknown code logs a Warn once and
// renders the generic label for its kind instead. A Go test in internal/api
// pins that every code the handler can emit has its key.
func extraFanartLabel(ctx context.Context, kind, code string) string {
	tr := i18n.TFromCtx(ctx)
	key := "extrafanart_migration." + kind + "_" + code
	if s := tr.T(key); s != key {
		return s
	}
	if _, seen := extraFanartUnknownSeen.LoadOrStore(key, struct{}{}); !seen {
		slog.Warn("extrafanart migration: no label for code", slog.String("kind", kind), slog.String("code", code))
	}
	return tr.T("extrafanart_migration." + kind + "_unknown")
}
