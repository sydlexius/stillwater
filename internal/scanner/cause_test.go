package scanner

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/provider"
)

// These tests drive the real Service.Run and read the cause off the context
// the post-scan hook receives (#2784). The hook is the real seam that gets the
// scan context. The scanner makes no provider calls itself, so recording the
// cause there is what a downstream provider call would read; a real
// orchestrator is not wired in because it would add a database, encryption and
// a fake provider to prove the same context value.

// runWithCause runs one scan on svc under ctx and returns the cause the hook
// saw. It fails if the hook never ran, so a test cannot pass vacuously.
func runWithCause(t *testing.T, svc *Service, ctx context.Context) provider.Cause {
	t.Helper()
	var mu sync.Mutex
	var got provider.Cause
	ran := false
	svc.SetPostScanHook(func(hctx context.Context) {
		mu.Lock()
		defer mu.Unlock()
		got = provider.CauseFromContext(hctx)
		ran = true
	})
	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The hook runs before the scan is marked finished, so a settled status
	// proves it already ran.
	waitForScan(t, svc, 5*time.Second)
	mu.Lock()
	defer mu.Unlock()
	if !ran {
		t.Fatal("post-scan hook did not run")
	}
	return got
}

func TestCause_ScanCarriesCallerCause(t *testing.T) {
	svc, _ := setupScanner(t, t.TempDir())
	ctx := provider.WithCause(context.Background(), provider.Cause{Class: provider.CauseClassUser, Detail: "POST /scans"})
	if got := runWithCause(t, svc, ctx).String(); got != "user:scan" {
		t.Errorf("cause = %q, want %q", got, "user:scan")
	}
}

func TestCause_ScanWithNoCallerCause(t *testing.T) {
	svc, _ := setupScanner(t, t.TempDir())
	if got := runWithCause(t, svc, context.Background()).String(); got != "scan" {
		t.Errorf("cause = %q, want %q", got, "scan")
	}
}

// The caller (an HTTP request) is finished as soon as Run returns, so the scan
// must not be tied to the caller's context.
func TestCause_ScanIsNotCancelledByCaller(t *testing.T) {
	svc, _ := setupScanner(t, t.TempDir())
	var mu sync.Mutex
	var hookErr error
	ran := false
	release := make(chan struct{})
	svc.SetPostScanHook(func(hctx context.Context) {
		<-release // park until the caller has been canceled
		mu.Lock()
		defer mu.Unlock()
		hookErr = hctx.Err()
		ran = true
	})
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	cancel()
	close(release)
	waitForScan(t, svc, 5*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if !ran {
		t.Fatal("post-scan hook did not run")
	}
	if hookErr != nil {
		t.Errorf("hook context error = %v, want nil: canceling the caller must not cancel the scan", hookErr)
	}
	if st := svc.Status(); st == nil || st.Status != "completed" {
		t.Errorf("final scan status = %+v, want completed", st)
	}
}

func TestCause_SequentialScansDoNotInheritEachOther(t *testing.T) {
	svc, _ := setupScanner(t, t.TempDir())
	a := provider.WithCause(context.Background(), provider.Cause{Class: provider.CauseClassWebhookEmby})
	if got := runWithCause(t, svc, a).String(); got != "webhook.emby:scan" {
		t.Fatalf("first scan cause = %q, want %q", got, "webhook.emby:scan")
	}
	if got := runWithCause(t, svc, context.Background()).String(); got != "scan" {
		t.Errorf("second scan cause = %q, want %q (it inherited the first scan's cause)", got, "scan")
	}
}

// Two scans in flight at once, each parked in its hook until both have
// started, must each still see their own cause.
func TestCause_ConcurrentScansDoNotLeak(t *testing.T) {
	svcA, _ := setupScanner(t, t.TempDir())
	svcB, _ := setupScanner(t, t.TempDir())

	var entered sync.WaitGroup
	entered.Add(2)
	gate := make(chan struct{})
	go func() { entered.Wait(); close(gate) }()

	var mu sync.Mutex
	seen := map[string]string{}
	hook := func(name string) PostScanHook {
		return func(hctx context.Context) {
			entered.Done()
			<-gate
			mu.Lock()
			seen[name] = provider.CauseFromContext(hctx).String()
			mu.Unlock()
		}
	}
	svcA.SetPostScanHook(hook("a"))
	svcB.SetPostScanHook(hook("b"))

	ctxA := provider.WithCause(context.Background(), provider.Cause{Class: provider.CauseClassUser})
	ctxB := provider.WithCause(context.Background(), provider.Cause{Class: provider.CauseClassWebhookLidarr})
	if _, err := svcA.Run(ctxA); err != nil {
		t.Fatalf("Run A: %v", err)
	}
	if _, err := svcB.Run(ctxB); err != nil {
		t.Fatalf("Run B: %v", err)
	}
	waitForScan(t, svcA, 5*time.Second)
	waitForScan(t, svcB, 5*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("hooks that ran = %v, want both", seen)
	}
	if seen["a"] != "user:scan" || seen["b"] != "webhook.lidarr:scan" {
		t.Errorf("causes = %v, want a=user:scan b=webhook.lidarr:scan", seen)
	}
}

// Carrying the cause by hand must not detach the scan from application
// shutdown: Shutdown cancels the shutdown context and then waits for the scan,
// so the scan's context has to be canceled by it.
func TestCause_ShutdownStillCancelsTheScan(t *testing.T) {
	svc, _ := setupScanner(t, t.TempDir())
	entered := make(chan struct{})
	var mu sync.Mutex
	var hookErr error
	svc.SetPostScanHook(func(hctx context.Context) {
		close(entered)
		// Park until shutdown cancels us. The timeout only bounds a broken
		// build (scan detached from shutdown) so the test fails, not hangs.
		select {
		case <-hctx.Done():
		case <-time.After(2 * time.Second):
		}
		mu.Lock()
		hookErr = hctx.Err()
		mu.Unlock()
	})
	ctx := provider.WithCause(context.Background(), provider.Cause{Class: provider.CauseClassUser})
	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	select {
	case <-entered: // precondition: the hook is running before Shutdown is called
	case <-time.After(5 * time.Second):
		t.Fatal("post-scan hook never entered")
	}

	done := make(chan struct{})
	go func() { svc.Shutdown(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return")
	}

	mu.Lock()
	defer mu.Unlock()
	if !errors.Is(hookErr, context.Canceled) {
		t.Errorf("hook context error = %v, want context.Canceled: Shutdown must still reach the scan", hookErr)
	}
}
