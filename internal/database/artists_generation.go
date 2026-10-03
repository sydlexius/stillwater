package database

import (
	"fmt"
	"sync/atomic"

	"modernc.org/sqlite"
)

// Artists write generation (#2395): one process-wide counter that goes up
// every time a row of the artists table is inserted, updated, or deleted.
//
// WHY IT EXISTS. Some in-memory caches hold a number derived from the artists
// table (the sidebar's non-compliant count is "SELECT COUNT(*) FROM artists
// WHERE health_score < 100"). A cache like that goes stale the moment any
// writer changes an artists row. There are dozens of such writers (the artist
// repository, the rule pipeline, the health subscriber, the scanner, the
// library service's raw DELETE statements, the merge transaction, future code),
// and asking each one to remember to invalidate is how the cache came to be
// invalidated by none of them. So the signal is taken from the one place every
// write must pass through, whoever issues it: SQLite itself.
//
// HOW. SQLite calls a "pre-update hook" just before it changes a row. The
// driver lets us install one on every connection as it opens, so the hook
// below sees every artists row write made through any *sql.DB in this process,
// including raw SQL and writes inside a transaction.
//
// HOW A CACHE USES IT. Pull, not push: nothing is called on write. A cache
// reads ArtistsGeneration BEFORE it queries, stores that number next to the
// result, and treats the result as fresh only while the number is unchanged.
// See complianceCountState in internal/api/handlers_report.go.
//
// LIMITS, so nobody assumes more than this gives:
//   - The bump happens when the row is written, not when its transaction
//     commits. That is safe only because the pool has ONE connection
//     (SetMaxOpenConns(1) in open): a reader cannot run its query until the
//     writer's statement or transaction has finished, so it can never pair the
//     new generation with pre-commit data. Raise the cap and that stops being
//     true; see the note there.
//   - A rolled-back write still bumps. That costs one unneeded recount.
//   - Only this process is seen. A second process writing the same file (a CLI
//     repair run against a live server) is not, which is why caches keep a TTL.
//   - Replacing the table wholesale (DROP TABLE / ALTER TABLE RENAME, as a
//     migration does) changes no row and does not bump. Migrations run before
//     anything is cached.
var artistsGeneration atomic.Uint64

// ArtistsGeneration returns the current artists write generation. Only
// equality between two readings is meaningful.
func ArtistsGeneration() uint64 { return artistsGeneration.Load() }

func init() {
	// Runs once per connection the "sqlite" driver opens, for every sql.Open
	// in the process (Open, OpenRuntime, and the read-only probes in cmd/).
	sqlite.RegisterConnectionHook(func(conn sqlite.ExecQuerierContext, _ string) error {
		hooks, ok := conn.(sqlite.HookRegisterer)
		if !ok {
			// Fail the open rather than run without the hook: a driver that
			// stopped offering it would otherwise bring the stale-count bug
			// back with nothing to show for it.
			return fmt.Errorf("sqlite connection %T offers no pre-update hook; artists write generation cannot be tracked", conn)
		}
		hooks.RegisterPreUpdateHook(func(d sqlite.SQLitePreUpdateData) {
			// Called once per row, so a bulk write bumps once per row. That is
			// deliberate and cheap (one atomic add): readers recompute lazily,
			// at most once per read, however many bumps happened in between.
			if d.TableName == "artists" {
				artistsGeneration.Add(1)
			}
		})
		return nil
	})
}
