package rule

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/database"
)

// baseTestTime is the starting timestamp for FakeClock instances. Initialized
// in TestMain after the template DB is migrated, so the value is guaranteed to
// be strictly greater than any datetime('now') the migrations wrote during
// setup (notably the seeded extraneous_images rule's updated_at). Computing it
// at package-init time worked in practice but left a race: anything that ran
// slower than the test-clock-vs-migration tolerance could see the wrong
// ordering.
var baseTestTime time.Time

// FakeClock is a Clock implementation for tests. Each call to Now advances the
// internal counter by one second, so callers that need strictly ordered
// timestamps never need time.Sleep.
type FakeClock struct {
	mu  sync.Mutex
	cur time.Time
}

// NewFakeClock returns a FakeClock starting at the given base time.
func NewFakeClock(base time.Time) *FakeClock {
	return &FakeClock{cur: base}
}

// Now returns the current fake time and advances the clock by one second.
func (f *FakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.cur
	f.cur = f.cur.Add(time.Second)
	return t
}

var templateDBPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "rule-test-template-*")
	if err != nil {
		panic("creating temp dir: " + err.Error())
	}

	templateDBPath = filepath.Join(dir, "template.db")
	db, err := database.Open(templateDBPath)
	if err != nil {
		panic("opening template db: " + err.Error())
	}
	if err := database.Migrate(db); err != nil {
		panic("migrating template db: " + err.Error())
	}
	if _, err := db.ExecContext(context.Background(), "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		panic("checkpointing template db: " + err.Error())
	}
	_ = db.Close()

	// Initialize baseTestTime here, after every datetime('now') the migrations
	// just wrote, so FakeClock ticks always start strictly after seeded rows.
	baseTestTime = time.Now().UTC().Add(time.Minute)

	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
