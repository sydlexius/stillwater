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

// receiptNeedsAttention reports whether a run's receipt takes the amber accent:
// everything except the three clean outcomes, so a status this code does not
// know is never shown as a quiet success.
func (v ExtraFanartMigrationView) receiptNeedsAttention() bool {
	if v.Aborted {
		return true
	}
	switch v.Status {
	case "migrated", "nothing_to_do", "nothing_checked":
		return false
	}
	return true
}

// receiptTitle is the receipt heading: the run's status label, or the stopped
// title when the run ended early (its status alone would read as a finished run).
func (v ExtraFanartMigrationView) receiptTitle(ctx context.Context) string {
	if v.Aborted {
		return t(ctx, "extrafanart_migration.receipt_stopped_title")
	}
	return extraFanartLabel(ctx, "status", v.Status)
}

// receiptBody says what the outcome means and what to do next.
func (v ExtraFanartMigrationView) receiptBody(ctx context.Context) string {
	switch {
	case v.Aborted:
		return t(ctx, "extrafanart_migration.receipt_stopped_body")
	case v.Status == "migrated":
		return t(ctx, "extrafanart_migration.receipt_migrated_body")
	case v.Status == "nothing_to_do":
		return t(ctx, "extrafanart_migration.receipt_nothing_to_do_body")
	case v.Status == "nothing_checked":
		return t(ctx, "extrafanart_migration.receipt_nothing_checked_body")
	case v.Status == "failed":
		return t(ctx, "extrafanart_migration.receipt_failed_body")
	}
	return t(ctx, "extrafanart_migration.receipt_partial_body")
}
