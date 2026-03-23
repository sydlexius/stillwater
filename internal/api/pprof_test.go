package api

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestRegisterPprof_ShutdownOnContextCancel(t *testing.T) {
	// Reset the sync.Once so this test can call registerPprof independently.
	// This test must NOT use t.Parallel -- the pprofOnce reset and the
	// hard-coded port 6060 require serial execution.
	pprofOnce = sync.Once{}
	t.Cleanup(func() { pprofOnce = sync.Once{} })

	ctx, cancel := context.WithCancel(context.Background())
	logger := slog.Default()

	// registerPprof binds the port synchronously, so the listener is ready
	// immediately after this call returns -- no sleep needed.
	registerPprof(ctx, logger)

	// Verify the pprof server is responding.
	resp, err := http.Get("http://127.0.0.1:6060/debug/pprof/")
	if err != nil {
		t.Fatalf("pprof listener not reachable: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from pprof index, got %d", resp.StatusCode)
	}

	// Cancel the context to trigger shutdown.
	cancel()

	// Poll until the listener stops accepting connections.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, pollErr := http.Get("http://127.0.0.1:6060/debug/pprof/")
		if pollErr != nil {
			// Connection refused means the server shut down -- success.
			return
		}
		resp.Body.Close()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("pprof listener did not shut down within 3 seconds after context cancellation")
}
