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
	t.Cleanup(cancel) // ensure shutdown even if the test fails early

	logger := slog.Default()
	client := &http.Client{Timeout: 5 * time.Second}

	// registerPprof binds the port synchronously, so the listener is ready
	// immediately after this call returns -- no sleep needed.
	registerPprof(ctx, logger)

	// If the port was already in use, registerPprof returns without starting
	// the server. Verify the listener is reachable; skip if not.
	resp, err := client.Get("http://127.0.0.1:6060/debug/pprof/")
	if err != nil {
		t.Skipf("pprof listener not reachable (port 6060 may be in use): %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from pprof index, got %d", resp.StatusCode)
	}

	// Cancel the context to trigger shutdown.
	cancel()

	// Poll until the listener stops accepting connections. Any network error
	// (connection refused, EOF, reset) indicates the server has stopped.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, pollErr := client.Get("http://127.0.0.1:6060/debug/pprof/")
		if pollErr != nil {
			return // server shut down -- success
		}
		resp.Body.Close()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("pprof listener did not shut down within 3 seconds after context cancellation")
}
