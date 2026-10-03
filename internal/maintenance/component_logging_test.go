package maintenance

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/logging"
	"github.com/sydlexius/stillwater/internal/logging/logtest"
)

type emptyLister struct{}

func (emptyLister) List(context.Context, artist.ListParams) ([]artist.Artist, int, error) {
	return nil, 0, nil
}

// TestServiceSubComponentsDoNotStackComponentKey drives the two real paths
// behind #2787: the service tags its logger "maintenance", and both the
// foreign-file scanner and the image-registry repair then tagged it again.
func TestServiceSubComponentsDoNotStackComponentKey(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	logger, buf := logtest.NewJSONLogger()
	svc := NewService(db, dbPath, "", logger)

	dir := t.TempDir()
	writeImage(t, filepath.Join(dir, "backdrop.jpg"), 640, 360)
	seedArtist(t, db, "aaaa1111-0000-0000-0000-000000000001", dir)
	if _, err := svc.RepairImageRegistry(context.Background(), ImageRepairOpts{Commit: true}); err != nil {
		t.Fatalf("repair: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		svc.StartForeignFileScanner(ctx, emptyLister{}, time.Hour, time.Hour)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "foreign-file scanner started") {
		if time.Now().After(deadline) {
			t.Fatal("scanner never logged its start line")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	out := buf.String()
	if d := logtest.DuplicateKeys(out); len(d) != 0 {
		t.Fatalf("duplicate keys on the real maintenance paths: %v", d)
	}
	for _, want := range []string{`"component":"image-repair"`, `"component":"foreign-scanner"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in %s", want, out)
		}
	}
	if strings.Contains(out, `"`+logging.PreviousComponentKey+`"`) {
		t.Fatalf("service handed its tagged logger to a sub-component: %s", out)
	}
}

// A nil logger is normalized once, so the foreign scanner (which does not
// nil-check) gets the same default logger the service itself uses. Before the
// normalization baseLogger stayed nil and NewScanner panicked on it.
func TestNewServiceNilLoggerReachesForeignScanner(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := NewService(db, dbPath, "", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // StartScheduler logs its start line and returns at once
	svc.StartForeignFileScanner(ctx, emptyLister{}, time.Hour, time.Hour)
}
