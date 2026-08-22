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
	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/maintenance"
)

// lockDamageRepairKey guards the one-shot locked-field damage repair (#3038 /
// #3075). Its VALUE is the completion timestamp; only its presence is
// consulted. Written only AFTER a pass with no row-level failures, so a crash
// or partial pass retries next boot rather than being permanently skipped.
const lockDamageRepairKey = "lock_damage_repair.completed_at"

// lockDamagePreGuardKey guards the PRE-GUARD one-shot (#3079). Same
// mechanism and same write-only-after-success rule as lockDamageRepairKey --
// a DIFFERENT KEY because the two passes cover disjoint populations. Sharing
// one would silently retire the pre-guard pass on every database that already
// ran the attributed one, i.e. every deployment of v1.6.2 or later: the exact
// population this repair exists for.
const lockDamagePreGuardKey = "lock_damage_repair.pre_guard_completed_at"

// runLockDamageDryRun is the -lock-damage-dry-run entry point: select and
// report, write nothing, exit. It never falls through into the normal startup
// path, never starts a listener, and never stamps lockDamageRepairKey.
//
// Built for the production-clone validation in
// docs/architecture/lock-damage-repair.md: point SW_DB_PATH (or the config
// file) at a COPY of the database and inspect what the predicate selects
// before any write pass runs.
func runLockDamageDryRun(preGuard bool) error {
	configPath := os.Getenv("SW_CONFIG_PATH")
	if configPath == "" {
		configPath = "/config/config.toml"
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// THE DRY RUN NEVER MIGRATES AND CANNOT WRITE. openMigratedRuntimeDB's
	// first act is migrateSchema, and the migrations mutate DATA, not just
	// schema (014 rewrites lock state, 024 retracts rule results and edits
	// artists). A clone of a released deployment is behind on migrations BY
	// CONSTRUCTION, so migrate-then-preview would silently rewrite the clone
	// -- including the lock state that is the predicate's own condition 1 --
	// while printing "no writes performed". Two independent defenses:
	//
	//   - mode=ro makes the HANDLE refuse writes (the idiom the startup
	//     scaffolding probe uses), so "no writes performed" is a property of
	//     the connection, not of a DryRun boolean a future edit in the repair
	//     path could route around. An accidental write fails loudly.
	//   - the version check below REFUSES a behind-on-migrations database,
	//     so the operator's next step (run the real server against a copy
	//     they are willing to migrate) is an informed choice, never a side
	//     effect.
	// The file: prefix is LOAD-BEARING: modernc's driver honors mode=ro only
	// in URI form. Without it the parameter is silently ignored and the
	// handle opens read-write -- verified against the driver, and the reason
	// this DSN does not copy the scaffolding probe's prefix-less form.
	db, err := sql.Open("sqlite", "file:"+cfg.Database.Path+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return fmt.Errorf("opening database read-only: %w", err)
	}
	defer db.Close() //nolint:errcheck // Close error not actionable on cleanup
	// Surface a bad path or DSN at open, not at first query.
	if err := db.PingContext(context.Background()); err != nil {
		return fmt.Errorf("opening database read-only: %w", err)
	}

	applied, err := database.AppliedMigrationVersion(context.Background(), db)
	if err != nil {
		return err
	}
	latest, err := database.LatestMigrationVersion()
	if err != nil {
		return err
	}
	if applied != latest {
		return fmt.Errorf("database at %s is at migration version %d; this build expects %d. "+
			"The dry run refuses to migrate: migrations rewrite data (lock state among it), "+
			"which would alter the very state this preview inspects. "+
			"Start the server once against a copy you are willing to migrate, then re-run the dry run",
			cfg.Database.Path, applied, latest)
	}

	return lockDamageDryRunDB(context.Background(), db, os.Stdout,
		maintenance.LockDamageOpts{DryRun: true, PreGuard: preGuard})
}

// lockDamageDryRunDB performs the dry-run pass against an already-open
// database and prints the candidate report. Accessible from tests in the same
// package, mirroring resetPasswordDB.
//
// The report carries artist IDs, fields, rule IDs, and timestamps -- NEVER
// artist names and NEVER old or new field values. The design doc's
// clone-handling rules class artist NAMES with the private library metadata
// that must not reach an outward surface, and worse, name is itself a
// lockable trackable field: for a field=name row the ArtistName IS the
// damaged value. stdout is a local surface, but this report exists to be
// copy-pasted into issues and reviews, which is exactly the leak the
// constraint prevents.
func lockDamageDryRunDB(ctx context.Context, db *sql.DB, out io.Writer, opts maintenance.LockDamageOpts) error {
	artistSvc := artist.NewService(db)
	hist := artist.NewHistoryService(db)
	artistSvc.SetHistoryService(hist)
	maint := maintenance.NewService(db, "", "",
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	maint.SetLockDamageDeps(hist.Repo(), artistSvc)

	res, err := maint.RepairLockDamage(ctx, opts)
	if err != nil {
		return fmt.Errorf("locked-field damage dry run: %w", err)
	}

	printLockDamageReport(out, res)
	return nil
}

// runLockDamagePreGuardRepair is the -lock-damage-pre-guard-repair entry
// point: the WRITE pass over the pre-guard population (#3079), once per
// database, then exit.
//
// WHY A FLAG AND NOT A STARTUP ONE-SHOT. The attributed pass (#3075) runs
// itself at boot because its predicate proves, per row, that a rule wrote the
// value. This population has no such proof by construction, so what makes it
// safe is a human ruling on the cut BEFORE anything writes -- and a boot-time
// pass has nowhere to put that ruling. Requiring the operator to preview and
// then type this flag makes the approval structural. See #3074.
func runLockDamagePreGuardRepair(logger *slog.Logger) error {
	configPath := os.Getenv("SW_CONFIG_PATH")
	if configPath == "" {
		configPath = "/config/config.toml"
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	// READ-WRITE and MIGRATED, unlike the dry run: this pass writes, so the
	// schema it writes against must be the one this build expects.
	db, err := openMigratedRuntimeDB(cfg.Database.Path)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck // Close error not actionable on cleanup

	ctx := context.Background()
	if getDBStringSetting(ctx, db, lockDamagePreGuardKey, "") != "" {
		logger.Info("pre-guard locked-field damage repair already completed on this database; nothing to do")
		return nil
	}

	artistSvc := artist.NewService(db)
	hist := artist.NewHistoryService(db)
	artistSvc.SetHistoryService(hist)
	maint := maintenance.NewService(db, cfg.Database.Path, "", logger)
	maint.SetLockDamageDeps(hist.Repo(), artistSvc)

	return guardPreGuardPanic(logger, func() {
		runLockDamageRepairPass(ctx, db, logger, maint,
			maintenance.LockDamageOpts{PreGuard: true}, lockDamagePreGuardKey)
	})
}

// guardPreGuardPanic runs pass and converts a panic into an error, logging
// the panic TYPE and never the recovered value.
//
// SAME PRIVACY CONTRACT AS THE STARTUP PANIC HANDLER: a panic from the
// restore path can carry a field value in its message, and an old biography
// is library content. Converted rather than re-panicked so the RUNTIME does
// not print it either -- unlike the startup goroutine this path can return,
// so the value has no reason to reach any surface. A separate function
// because a deferred closure inside the entry point is unreachable from a
// test, and an untested privacy guard is an assumption, not a guarantee.
func guardPreGuardPanic(logger *slog.Logger, pass func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("pre-guard locked-field damage repair panicked",
				"panic_type", fmt.Sprintf("%T", r))
			err = fmt.Errorf("pre-guard locked-field damage repair panicked (%T); "+
				"completion was not recorded, so it can be re-run", r)
		}
	}()
	pass()
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
		// direction is a fixed descriptor, never a value: it lets the
		// operator sort unambiguous overwrites from possible curation
		// without being shown the biographies to do it.
		_, _ = fmt.Fprintf(out, "  artist=%s field=%s rule=%s direction=%s damaged_at=%s\n",
			r.ArtistID, r.Field, r.RuleID, r.Direction,
			r.DamagedAt.UTC().Format(time.RFC3339))
	}
	// The bound's effect is PRINTED, not inferred from an absence: a preview
	// that silently withheld rows would read the same as a clean library.
	_, _ = fmt.Fprintf(out, "excluded, newer than the cutoff (%s): %d\n",
		maintenance.PreGuardCutoff().Format(time.RFC3339), res.PreGuardTooNew)
	_, _ = fmt.Fprintf(out, "excluded, field not locked now: %d\n", res.PreGuardUnlocked)
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
//
// a.lockDamageRepairDone is set here and closed when the goroutine returns
// (#3088), so drainLockDamageRepair in main.go's shutdown sequence has
// something to wait on. It stays nil when the completion key already gated
// the pass off -- there is nothing running, so nothing to drain.
func (a *Application) startLockDamageRepair(ctx context.Context, db *sql.DB, logger *slog.Logger) {
	if getDBStringSetting(ctx, db, lockDamageRepairKey, "") != "" {
		return
	}
	done := make(chan struct{})
	a.lockDamageRepairDone = done
	go func() {
		defer close(done)
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
		runLockDamageRepairPass(ctx, db, logger, a.maintenanceService,
			maintenance.LockDamageOpts{}, lockDamageRepairKey)
	}()
}

// drainLockDamageRepair waits for the locked-field damage repair goroutine to
// finish, or for ctx to expire, whichever comes first (#3088).
//
// WHY THIS EXISTS. Nothing tracked the goroutine before this: run()'s
// deferred db.Close() could fire while the pass was still querying, and worse,
// while RestoreLockedFieldGuarded was between its transaction commit and a
// separate best-effort history insert -- a window that produced a restored
// field with no history row to explain it. #3088 closes THAT split by moving
// the history insert inside the same transaction (see
// internal/artist/lock_restore.go), so this drain no longer needs to protect
// that specific window. It still exists for the same reason
// dupimages.Cache.Drain and the webhook drains exist: db.Close() firing under
// a still-running query is its own hazard (a query against a closed *sql.DB
// returns sql.ErrConnDone, logged as an unexplained failure on the next
// boot's retry), and letting a background query outlive shutdown at all is
// something every other worker in this sequence is drained against.
//
// Called from the same slot the webhook and duplicate-image drains occupy:
// after the listeners have drained (so nothing new can start -- this is a
// one-shot pass anyway, gated by the completion key, so it can never be
// re-triggered) and before db.Close(). The shared ctx is already canceled by
// stop() before this runs, and RepairLockDamage checks ctx.Err() between
// candidates (internal/maintenance/lock_damage_repair.go), so a well-behaved
// pass aborts almost immediately -- the deadline below exists only to bound a
// pass wedged in non-context-aware I/O (a slow disk read inside GetByID, for
// instance) so it cannot hang shutdown forever.
func (a *Application) drainLockDamageRepair(ctx context.Context) error {
	if a.lockDamageRepairDone == nil {
		// Never started (the completion key already gated it off) or startup
		// never reached startLockDamageRepair at all.
		return nil
	}
	select {
	case <-a.lockDamageRepairDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// runLockDamageRepairPass runs one repair pass and decides whether to record
// completion. Synchronous so the completion gate is testable; the goroutine
// wrapper above owns the panic handler.
func runLockDamageRepairPass(ctx context.Context, db *sql.DB, logger *slog.Logger, maint *maintenance.Service, opts maintenance.LockDamageOpts, key string) {
	res, err := maint.RepairLockDamage(ctx, opts)
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
		key, time.Now().UTC().Format(time.RFC3339)); err != nil {
		logger.Error("recording locked-field damage repair completion", "error", err)
	}
}
