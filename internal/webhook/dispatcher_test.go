package webhook

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/event"
)

func setupDispatcherTest(t *testing.T) (*Service, *slog.Logger) {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return NewService(db), logger
}

func TestDispatcher_GenericWebhook(t *testing.T) {
	svc, logger := setupDispatcherTest(t)

	var mu sync.Mutex
	var received map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		json.NewDecoder(r.Body).Decode(&received) //nolint:errcheck
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	w := &Webhook{
		Name:    "test",
		URL:     srv.URL,
		Type:    TypeGeneric,
		Events:  []string{"scan.completed"},
		Enabled: true,
	}
	if err := svc.Create(context.Background(), w); err != nil {
		t.Fatal(err)
	}

	dispatcher := NewDispatcherWithHTTPClient(svc, srv.Client(), logger)
	dispatcher.HandleEvent(event.Event{
		Type:      event.ScanCompleted,
		Timestamp: time.Now().UTC(),
		Data:      map[string]any{"artists": float64(42)},
	})

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if received == nil {
		t.Fatal("expected to receive webhook payload")
	}
	if received["event"] != "scan.completed" {
		t.Errorf("event = %v, want scan.completed", received["event"])
	}
}

func TestDispatcher_DiscordFormat(t *testing.T) {
	svc, logger := setupDispatcherTest(t)

	var mu sync.Mutex
	var received map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		json.NewDecoder(r.Body).Decode(&received) //nolint:errcheck
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	w := &Webhook{
		Name:    "discord",
		URL:     srv.URL,
		Type:    TypeDiscord,
		Events:  []string{"scan.completed"},
		Enabled: true,
	}
	if err := svc.Create(context.Background(), w); err != nil {
		t.Fatal(err)
	}

	dispatcher := NewDispatcherWithHTTPClient(svc, srv.Client(), logger)
	dispatcher.HandleEvent(event.Event{
		Type:      event.ScanCompleted,
		Timestamp: time.Now().UTC(),
		Data:      map[string]any{"message": "Scan finished"},
	})

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if received == nil {
		t.Fatal("expected to receive webhook payload")
	}
	embeds, ok := received["embeds"].([]any)
	if !ok || len(embeds) == 0 {
		t.Fatal("expected discord embeds array")
	}
	embed := embeds[0].(map[string]any)
	if embed["description"] != "Scan finished" {
		t.Errorf("description = %v, want 'Scan finished'", embed["description"])
	}
}

func TestDispatcher_RetryOn500(t *testing.T) {
	svc, logger := setupDispatcherTest(t)

	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	w := &Webhook{
		Name:    "retry-test",
		URL:     srv.URL,
		Type:    TypeGeneric,
		Events:  []string{"scan.completed"},
		Enabled: true,
	}
	if err := svc.Create(context.Background(), w); err != nil {
		t.Fatal(err)
	}

	dispatcher := NewDispatcherWithHTTPClient(svc, srv.Client(), logger)
	dispatcher.HandleEvent(event.Event{
		Type:      event.ScanCompleted,
		Timestamp: time.Now().UTC(),
	})

	// Wait for retries (1s + 2s backoff)
	time.Sleep(5 * time.Second)

	got := int(attempts.Load())
	if got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

func TestDispatcher_MaxRetries(t *testing.T) {
	svc, logger := setupDispatcherTest(t)

	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	w := &Webhook{
		Name:    "maxretry-test",
		URL:     srv.URL,
		Type:    TypeGeneric,
		Events:  []string{"bulk.completed"},
		Enabled: true,
	}
	if err := svc.Create(context.Background(), w); err != nil {
		t.Fatal(err)
	}

	dispatcher := NewDispatcherWithHTTPClient(svc, srv.Client(), logger)
	dispatcher.HandleEvent(event.Event{
		Type:      event.BulkCompleted,
		Timestamp: time.Now().UTC(),
	})

	// Wait for all retries (1s + 2s + attempt 3)
	time.Sleep(6 * time.Second)

	got := int(attempts.Load())
	if got != 3 {
		t.Errorf("attempts = %d, want 3 (max retries)", got)
	}
}

func TestDispatcher_NoMatchingWebhooks(t *testing.T) {
	svc, logger := setupDispatcherTest(t)

	w := &Webhook{
		Name:    "other",
		URL:     "http://localhost:9999",
		Type:    TypeGeneric,
		Events:  []string{"bulk.completed"},
		Enabled: true,
	}
	if err := svc.Create(context.Background(), w); err != nil {
		t.Fatal(err)
	}

	dispatcher := NewDispatcher(svc, logger)
	// Should not panic or hang
	dispatcher.HandleEvent(event.Event{
		Type:      event.ScanCompleted,
		Timestamp: time.Now().UTC(),
	})
	time.Sleep(50 * time.Millisecond)
}
