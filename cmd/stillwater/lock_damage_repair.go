package main

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
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

	printLockDamageReport(out, res, opts.PreGuard)
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
//
// THE DIGEST IS REQUIRED, NOT OPTIONAL (#3079 review, HIGH-1). Typing the
// flag proves the operator INTENDED a write; it says nothing about WHICH rows
// they intended, and the two invocations re-select independently. An empty
// approvedDigest is refused here rather than defaulted to "whatever the
// predicate finds", because a gate that can be skipped by omitting an
// argument is not a gate.
//
// IT OPENS THE DATABASE READ-WRITE AND MIGRATES IT, unlike the dry run. Two
// consequences worth stating where an operator will read them: it must not be
// pointed at a database a server is currently using (SQLite will refuse or
// contend, and migrations must not run under a live reader), and it will
// upgrade a behind-on-migrations database in place. Back up first.
func runLockDamagePreGuardRepair(logger *slog.Logger, approvedDigest string) error {
	if strings.TrimSpace(approvedDigest) == "" {
		return fmt.Errorf("-lock-damage-pre-guard-repair requires -lock-damage-pre-guard-approve=<digest>. " +
			"Run -lock-damage-pre-guard-dry-run first, review the candidate list it prints, " +
			"and pass the approval digest from the end of that report. " +
			"The digest is what makes the preview binding: without it the repair would " +
			"restore whatever the predicate matches now, not the set you approved")
	}
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

	return guardPreGuardPanic(logger, func() error {
		return runLockDamageRepairPass(ctx, db, logger, maint,
			maintenance.LockDamageOpts{PreGuard: true, ApprovedDigest: approvedDigest},
			lockDamagePreGuardKey)
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
func guardPreGuardPanic(logger *slog.Logger, pass func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("pre-guard locked-field damage repair panicked",
				"panic_type", fmt.Sprintf("%T", r))
			err = fmt.Errorf("pre-guard locked-field damage repair panicked (%T); "+
				"completion was not recorded, so it can be re-run", r)
		}
	}()
	// The pass's own error is RETURNED, not swallowed (#3079 review,
	// MEDIUM-1). Before this the whole entry point returned nil regardless,
	// so a pass that failed outright still exited 0 and an operator scripting
	// `stillwater -lock-damage-pre-guard-repair && echo done` was told the
	// repair succeeded.
	return pass()
}

// previewDirectionRank orders the preview's restore list by how AMBIGUOUS the
// row is, least first: an emptied field is an unarguable loss, a shortened one
// nearly so, and a longer or same-length value is the shape genuine operator
// curation takes.
//
// ORDERING ONLY. It decides nothing: every candidate is printed whatever its
// rank, and no predicate reads Direction (see the header of
// internal/maintenance/lock_damage_repair.go for why "it grew, so it was
// curation" is false -- a provider can return a longer WRONG value). Sorting
// the unambiguous rows to the top is what makes the preview usable as the
// approval mechanism it is: on a real library the list runs to hundreds of
// rows, and a chronological dump buries the ones that need no thought among
// the ones that need the most.
//
// An unrecognized direction sorts LAST rather than first, so a future
// vocabulary addition lands among the rows that get scrutiny rather than
// among the ones a reader may wave through.
func previewDirectionRank(direction string) int {
	switch direction {
	case "emptied":
		return 0
	case "shorter":
		return 1
	case "same-length":
		return 2
	case "longer":
		return 3
	default:
		return 4
	}
}

// orderedForPreview returns a COPY of the restore list sorted for the preview.
// A copy, not an in-place sort: the caller's result is shared with the write
// pass and with the tests, and a reporting concern must not reorder it.
//
// The sort is STABLE and fully tie-broken (direction, then field, then artist
// id), so two runs over the same database print byte-identical lists and a
// diff between two previews shows only real changes.
func orderedForPreview(restored []maintenance.LockDamageRestore) []maintenance.LockDamageRestore {
	out := slices.Clone(restored)
	slices.SortStableFunc(out, func(a, b maintenance.LockDamageRestore) int {
		if c := cmp.Compare(previewDirectionRank(a.Direction), previewDirectionRank(b.Direction)); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Field, b.Field); c != 0 {
			return c
		}
		return cmp.Compare(a.ArtistID, b.ArtistID)
	})
	return out
}

// printLockDamageReport renders the dry-run report. Split from the database
// plumbing so a test can drive every section -- the Failed loop needs a
// result only an injected repository failure produces, which the dry-run
// entry point's self-built services cannot be given.
func printLockDamageReport(out io.Writer, res *maintenance.LockDamageResult, preGuard bool) {
	_, _ = fmt.Fprintf(out, "locked-field damage repair DRY RUN (no writes performed)\n")
	_, _ = fmt.Fprintf(out, "would restore: %d\n", len(res.Restored))
	ordered := orderedForPreview(res.Restored)
	for i := range ordered {
		r := &ordered[i]
		// direction is a fixed descriptor and the lengths are rune counts:
		// magnitude, never content. Together they let the operator separate a
		// near-total wipe from a one-character touch-up, which direction alone
		// cannot do -- both print "shorter".
		//
		// rule= is OMITTED in pre-guard mode (#3079 review, NIT-1). RuleID is
		// deliberately empty there (no rule is named on these rows, which is
		// the whole reason the population exists), and a bare "rule= " on 215
		// consecutive lines reads like a field that failed to populate rather
		// than one that has nothing to say.
		if preGuard {
			_, _ = fmt.Fprintf(out, "  artist=%s field=%s direction=%s %s chain_depth=%d damaged_at=%s\n",
				r.ArtistID, r.Field, r.Direction, formatLengthDelta(r.OldLen, r.NewLen),
				r.ChainDepth, r.DamagedAt.UTC().Format(time.RFC3339))
			continue
		}
		_, _ = fmt.Fprintf(out, "  artist=%s field=%s rule=%s direction=%s %s damaged_at=%s\n",
			r.ArtistID, r.Field, r.RuleID, r.Direction,
			formatLengthDelta(r.OldLen, r.NewLen),
			r.DamagedAt.UTC().Format(time.RFC3339))
	}
	// The bound's effect is PRINTED, not inferred from an absence: a preview
	// that silently withheld rows would read the same as a clean library.
	_, _ = fmt.Fprintf(out, "excluded, newer than the cutoff (%s): %d\n",
		maintenance.PreGuardCutoff().Format(time.RFC3339), res.PreGuardTooNew)
	_, _ = fmt.Fprintf(out, "excluded, field not locked now: %d\n", res.PreGuardUnlocked)
	// Reported for the same reason the other exclusions are: a row dropped
	// because the field moved on since the damage is a DECISION, and a preview
	// that made it silently would be the overstatement this count exists to
	// retire.
	_, _ = fmt.Fprintf(out, "excluded, the field changed since the damage: %d\n", res.PreGuardDiverged)
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

	if !preGuard {
		return
	}
	// THE APPROVAL DIGEST, LAST AND UNMISSABLE. It is the token that makes
	// this preview binding on the write: the repair recomputes it over the set
	// it selects and refuses unless the two agree, so a lock toggled between
	// the two invocations cannot quietly enlarge the write.
	_, _ = fmt.Fprintf(out, "\napproval digest: %s\n", maintenance.LockDamageDigest(res.Restored))
	_, _ = fmt.Fprintf(out, "To restore exactly the %d row(s) listed above, re-run with:\n", len(res.Restored))
	_, _ = fmt.Fprintf(out, "  -lock-damage-pre-guard-repair -lock-damage-pre-guard-approve=%s\n",
		maintenance.LockDamageDigest(res.Restored))
	_, _ = fmt.Fprintf(out, "The repair opens the database READ-WRITE and RUNS MIGRATIONS: "+
		"stop the server and back up first.\n")
}

// formatLengthDelta renders the magnitude of one damage row as "OLD -> NEW
// runes (PCT)". Lengths and a ratio, never content.
//
// The percentage is relative to the ORIGINAL length, so -100% is an emptied
// field and +150% is a value that grew to two and a half times its size. It
// is omitted when the original was empty, since a percentage of zero is not
// a number the operator can act on -- and such a row cannot be a candidate
// anyway (the damage predicate requires old_value != ”).
func formatLengthDelta(oldLen, newLen int) string {
	if oldLen <= 0 {
		return fmt.Sprintf("len=%d->%d", oldLen, newLen)
	}
	pct := float64(newLen-oldLen) * 100 / float64(oldLen)
	return fmt.Sprintf("len=%d->%d (%+.1f%%)", oldLen, newLen, pct)
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
		// The error is DELIBERATELY DISCARDED on the startup path: a boot-time
		// one-shot has no exit code to carry it, the failure is already
		// logged, and the unstamped completion key is what retries it. The
		// CLI path is where the error becomes an exit code.
		_ = runLockDamageRepairPass(ctx, db, logger, a.maintenanceService,
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
func runLockDamageRepairPass(ctx context.Context, db *sql.DB, logger *slog.Logger, maint *maintenance.Service, opts maintenance.LockDamageOpts, key string) error {
	res, err := maint.RepairLockDamage(ctx, opts)
	if err != nil {
		// THE ERROR IS RETURNED AS WELL AS LOGGED (#3079 review, MEDIUM-1).
		// The startup caller ignores it -- a boot-time one-shot has nobody to
		// report an exit code to, and the unstamped key is its retry -- but
		// the CLI caller turns it into a non-zero exit. Before this the error
		// died in a log line, so `stillwater -lock-damage-pre-guard-repair &&
		// echo done` printed "done" for a pass that wrote nothing. The
		// migration-failure path already exited 1, so this was inconsistent
		// as well as wrong.
		//
		// A DIGEST MISMATCH ARRIVES HERE TOO, and takes the same path: the
		// pass returned before writing anything, the key is not stamped, and
		// the operator gets a non-zero exit plus the drift message naming
		// what to do about it.
		logger.Error("locked-field damage repair failed; will retry next start",
			"error", err)
		return err
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
		// A pass with transient row-level failures did not complete. It is
		// not a hard error (the rows retry, and what DID restore is kept), but
		// it must not report success to a script either.
		return fmt.Errorf("locked-field damage repair finished with %d row-level failure(s); "+
			"completion was not recorded and the next run retries them", len(res.Failed))
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
		// The repair itself SUCCEEDED; only the stamp failed. Surfaced as an
		// error anyway, because the operator's next run will redo the whole
		// pass (finding nothing, since the query has converged) and they
		// should know why rather than discovering it as a surprise.
		return fmt.Errorf("the repair completed but recording its completion failed: %w", err)
	}
	return nil
}
