package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/config"
	"github.com/sydlexius/stillwater/internal/maintenance"
)

// lockDamageRepairKey guards the one-shot locked-field damage repair (#3038 /
// #3075). Its VALUE is the completion timestamp; only its presence is
// consulted. Written only AFTER a pass with no row-level failures, so a crash
// or partial pass retries next boot rather than being permanently skipped.
const lockDamageRepairKey = "lock_damage_repair.completed_at"

// runLockDamageDryRun is the -lock-damage-dry-run entry point: select and
// report, write nothing, exit. It never falls through into the normal startup
// path, never starts a listener, and never stamps lockDamageRepairKey.
//
// Built for the production-clone validation in
// docs/architecture/lock-damage-repair.md: point SW_DB_PATH (or the config
// file) at a COPY of the database and inspect what the predicate selects
// before any write pass runs.
func runLockDamageDryRun() error {
	configPath := os.Getenv("SW_CONFIG_PATH")
	if configPath == "" {
		configPath = "/config/config.toml"
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	db, err := openMigratedRuntimeDB(cfg.Database.Path)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck // Close error not actionable on cleanup

	return lockDamageDryRunDB(context.Background(), db, os.Stdout)
}

// lockDamageDryRunDB performs the dry-run pass against an already-open
// database and prints the candidate report. Accessible from tests in the same
// package, mirroring resetPasswordDB.
//
// The report carries artist IDs, names, fields, rule IDs, and timestamps --
// NEVER old or new field values. stdout is a local surface, but a value here
// would end up copy-pasted into issues and reviews, which is exactly the leak
// the logging constraint exists to prevent.
func lockDamageDryRunDB(ctx context.Context, db *sql.DB, out io.Writer) error {
	artistSvc := artist.NewService(db)
	hist := artist.NewHistoryService(db)
	artistSvc.SetHistoryService(hist)
	maint := maintenance.NewService(db, "", "",
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	maint.SetLockDamageDeps(hist.Repo(), artistSvc)

	res, err := maint.RepairLockDamage(ctx, maintenance.LockDamageOpts{DryRun: true})
	if err != nil {
		return fmt.Errorf("locked-field damage dry run: %w", err)
	}

	printLockDamageReport(out, res)
	return nil
}

// printLockDamageReport renders the dry-run report. Split from the database
// plumbing so a test can drive every section -- the Failed loop needs a
// result only an injected repository failure produces, which the dry-run
// entry point's self-built services cannot be given.
func printLockDamageReport(out io.Writer, res *maintenance.LockDamageResult) {
	_, _ = fmt.Fprintf(out, "locked-field damage repair DRY RUN (no writes performed)\n")
	_, _ = fmt.Fprintf(out, "would restore: %d\n", len(res.Restored))
	for _, r := range res.Restored {
		_, _ = fmt.Fprintf(out, "  artist=%s (%s) field=%s rule=%s damaged_at=%s\n",
			r.ArtistID, r.ArtistName, r.Field, r.RuleID,
			r.DamagedAt.UTC().Format(time.RFC3339))
	}
	_, _ = fmt.Fprintf(out, "unrecoverable: %d (unattributable_all=%d)\n",
		len(res.Unrecoverable), res.UnattributableAll)
	for _, u := range res.Unrecoverable {
		_, _ = fmt.Fprintf(out, "  artist=%s field=%s rule=%q reason=%s\n",
			u.ArtistID, u.Field, u.RuleID, u.Reason)
	}
	_, _ = fmt.Fprintf(out, "failed permanently: %d\n", len(res.FailedPermanent))
	for _, f := range res.FailedPermanent {
		_, _ = fmt.Fprintf(out, "  artist=%s field=%s rule=%q reason=%s\n",
			f.ArtistID, f.Field, f.RuleID, f.Reason)
	}
	_, _ = fmt.Fprintf(out, "failed: %d\n", len(res.Failed))
	for _, f := range res.Failed {
		_, _ = fmt.Fprintf(out, "  artist=%s field=%s rule=%q reason=%s\n",
			f.ArtistID, f.Field, f.RuleID, f.Reason)
	}
}

// startLockDamageRepair launches the one-shot locked-field damage repair
// (#3038 / #3075) unless a previous pass already completed.
//
// The settings key is written only AFTER a pass with no row-level failures, so
// a crash mid-run retries next boot rather than being permanently skipped. The
// key is an optimization only: a restore writes a source="revert" row that
// drops the pair from the damage query, so a second pass selects nothing on
// its own merits even with the key absent.
func (a *Application) startLockDamageRepair(ctx context.Context, db *sql.DB, logger *slog.Logger) {
	if getDBStringSetting(ctx, db, lockDamageRepairKey, "") != "" {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				// Log the TYPE, never the recovered value. A panic from the
				// restore path can carry a field value in its message, and an
				// old biography is user library content that must not reach
				// the log.
				logger.Error("locked-field damage repair panicked",
					"panic_type", fmt.Sprintf("%T", r))
			}
		}()
		runLockDamageRepairPass(ctx, db, logger, a.maintenanceService)
	}()
}

// runLockDamageRepairPass runs one repair pass and decides whether to record
// completion. Synchronous so the completion gate is testable; the goroutine
// wrapper above owns the panic handler.
func runLockDamageRepairPass(ctx context.Context, db *sql.DB, logger *slog.Logger, maint *maintenance.Service) {
	res, err := maint.RepairLockDamage(ctx, maintenance.LockDamageOpts{})
	if err != nil {
		logger.Error("locked-field damage repair failed; will retry next start",
			"error", err)
		return
	}
	// COMPLETION IS RECORDED ONLY ON A PASS WITH NO TRANSIENT ROW-LEVEL
	// FAILURES. A transiently failed row was neither restored nor proven
	// unrecoverable, so stamping the key here would retire the one-shot with
	// work outstanding and nothing would ever retry it. Unrecoverable rows do
	// NOT block completion: they are a decided outcome, and they can never
	// become recoverable on a later boot. Neither do PERMANENT failures: a
	// restore refused for a deterministic reason (validation, a name
	// collision) is refused identically on every retry, so blocking on it
	// would re-run the full pass on every start forever with no way to retire
	// it. They are reported in their own count so the refusal is visible, not
	// silent.
	//
	// THE FAILURE GATE COMES BEFORE ANY "complete" LINE. Logging that the
	// pass finished and then warning that it did not is a false success
	// signal in the startup log, which is the one place an operator looks to
	// see whether the repair ran (#3074 review).
	if len(res.Failed) > 0 {
		logger.Warn("locked-field damage repair finished with row-level "+
			"failures; not recording completion, the next start retries them",
			"restored", len(res.Restored),
			"unrecoverable", len(res.Unrecoverable),
			"unattributable_all", res.UnattributableAll,
			"failed_permanent", len(res.FailedPermanent),
			"failed", len(res.Failed))
		return
	}
	logger.Info("locked-field damage repair complete",
		"restored", len(res.Restored),
		"unrecoverable", len(res.Unrecoverable),
		"unattributable_all", res.UnattributableAll,
		"failed_permanent", len(res.FailedPermanent),
		"failed", len(res.Failed))
	if _, err := db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		lockDamageRepairKey, time.Now().UTC().Format(time.RFC3339)); err != nil {
		logger.Error("recording locked-field damage repair completion", "error", err)
	}
}
