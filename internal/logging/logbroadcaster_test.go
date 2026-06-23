package logging

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// newTestBroadcaster returns a broadcaster capturing at debug level with source
// derivation disabled (tests assert on attrs/component/message, not file:line).
func newTestBroadcaster(t *testing.T) *LogBroadcaster {
	t.Helper()
	lvl := &slog.LevelVar{}
	lvl.Set(slog.LevelDebug)
	return NewLogBroadcaster(lvl, false)
}

// emit logs one record through the broadcaster as a slog handler would.
func emit(b *LogBroadcaster, level slog.Level, msg string, attrs ...slog.Attr) {
	r := slog.NewRecord(time.Now(), level, msg, 0)
	r.AddAttrs(attrs...)
	_ = b.Handle(context.Background(), r)
}

func TestLogBroadcaster_SubscribeReceive(t *testing.T) {
	t.Parallel()
	b := newTestBroadcaster(t)

	sub := b.Subscribe(LogFilter{})
	defer sub.Close()

	if got := b.SubscriberCount(); got != 1 {
		t.Fatalf("SubscriberCount = %d, want 1", got)
	}

	emit(b, slog.LevelInfo, "hello world", slog.String("component", "scanner"), slog.Int("count", 3))

	select {
	case e := <-sub.Lines():
		if e.Message != "hello world" {
			t.Errorf("Message = %q, want %q", e.Message, "hello world")
		}
		if e.Level != "info" {
			t.Errorf("Level = %q, want info", e.Level)
		}
		if e.Component != "scanner" {
			t.Errorf("Component = %q, want scanner", e.Component)
		}
		if e.Attrs["count"] != int64(3) {
			t.Errorf("Attrs[count] = %v, want 3", e.Attrs["count"])
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for log line")
	}
}

func TestLogBroadcaster_RedactsSensitiveAttrs(t *testing.T) {
	t.Parallel()
	b := newTestBroadcaster(t)
	sub := b.Subscribe(LogFilter{})
	defer sub.Close()

	emit(b, slog.LevelWarn, "auth attempt", slog.String("api_key", "super-secret-value"))

	select {
	case e := <-sub.Lines():
		if got := e.Attrs["api_key"]; got != "[REDACTED]" {
			t.Errorf("api_key = %v, want [REDACTED]", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for log line")
	}
}

func TestLogBroadcaster_FilterMatching(t *testing.T) {
	t.Parallel()
	b := newTestBroadcaster(t)

	t.Run("level", func(t *testing.T) {
		sub := b.Subscribe(LogFilter{Level: "warn"})
		defer sub.Close()
		emit(b, slog.LevelInfo, "below threshold")
		emit(b, slog.LevelError, "at or above")
		e := mustReceive(t, sub)
		if e.Message != "at or above" {
			t.Errorf("got %q, want the error line (info should be filtered out)", e.Message)
		}
		assertNoMore(t, sub)
	})

	t.Run("component", func(t *testing.T) {
		sub := b.Subscribe(LogFilter{Component: "watcher"})
		defer sub.Close()
		emit(b, slog.LevelInfo, "wrong", slog.String("component", "scanner"))
		emit(b, slog.LevelInfo, "right", slog.String("component", "watcher"))
		e := mustReceive(t, sub)
		if e.Message != "right" {
			t.Errorf("got %q, want the watcher line", e.Message)
		}
		assertNoMore(t, sub)
	})

	t.Run("search", func(t *testing.T) {
		sub := b.Subscribe(LogFilter{Search: "DEADBEEF"})
		defer sub.Close()
		emit(b, slog.LevelInfo, "nothing here")
		emit(b, slog.LevelInfo, "hash is deadbeef now") // case-insensitive
		e := mustReceive(t, sub)
		if e.Message != "hash is deadbeef now" {
			t.Errorf("got %q, want the matching line", e.Message)
		}
		assertNoMore(t, sub)
	})
}

func TestLogBroadcaster_Unsubscribe(t *testing.T) {
	t.Parallel()
	b := newTestBroadcaster(t)
	sub := b.Subscribe(LogFilter{})

	sub.Close()
	if got := b.SubscriberCount(); got != 0 {
		t.Fatalf("SubscriberCount after Close = %d, want 0", got)
	}

	// Lines channel must be closed.
	if _, ok := <-sub.Lines(); ok {
		t.Error("Lines channel should be closed after Close")
	}

	// Close is idempotent and a publish after unsubscribe must not panic.
	sub.Close()
	emit(b, slog.LevelInfo, "after unsubscribe")
}

func TestLogBroadcaster_BufferFullThrottle(t *testing.T) {
	t.Parallel()
	lvl := &slog.LevelVar{}
	lvl.Set(slog.LevelDebug)
	b := newLogBroadcasterWithBuffer(lvl, false, 2)

	sub := b.Subscribe(LogFilter{})
	defer sub.Close()

	// Emit more than the buffer can hold without draining Lines, forcing drops.
	const sent = 10
	for i := 0; i < sent; i++ {
		emit(b, slog.LevelInfo, "flood")
	}

	// A throttle signal must have been raised.
	select {
	case <-sub.Throttle():
	case <-time.After(time.Second):
		t.Fatal("expected a throttle signal when the buffer overflowed")
	}

	dropped := sub.DrainDropped()
	if dropped <= 0 {
		t.Fatalf("DrainDropped = %d, want > 0", dropped)
	}
	// Buffer holds 2, so at least sent-2 lines were dropped.
	if dropped < sent-2 {
		t.Errorf("DrainDropped = %d, want >= %d", dropped, sent-2)
	}
	// Draining resets the counter.
	if again := sub.DrainDropped(); again != 0 {
		t.Errorf("second DrainDropped = %d, want 0", again)
	}
}

func TestLogBroadcaster_NoSubscribersIsCheap(t *testing.T) {
	t.Parallel()
	b := newTestBroadcaster(t)
	// With no subscribers, Handle must be a no-op that never blocks or panics.
	emit(b, slog.LevelError, "into the void", slog.String("api_key", "secret"))
	if got := b.SubscriberCount(); got != 0 {
		t.Fatalf("SubscriberCount = %d, want 0", got)
	}
}

func TestLogBroadcaster_DerivedHandlerSharesSubscribers(t *testing.T) {
	t.Parallel()
	b := newTestBroadcaster(t)
	sub := b.Subscribe(LogFilter{})
	defer sub.Close()

	// A handler derived via WithAttrs/WithGroup must publish to the same
	// subscriber registry and carry its pre-stored attrs onto the entry.
	derived, ok := b.WithAttrs([]slog.Attr{slog.String("component", "derived-pkg")}).(*LogBroadcaster)
	if !ok {
		t.Fatal("WithAttrs did not return *LogBroadcaster")
	}
	emit(derived, slog.LevelInfo, "from derived")

	e := mustReceive(t, sub)
	if e.Message != "from derived" {
		t.Errorf("Message = %q, want from derived", e.Message)
	}
	if e.Component != "derived-pkg" {
		t.Errorf("Component = %q, want derived-pkg (WithAttrs not applied)", e.Component)
	}
}

// TestLogBroadcaster_ConcurrentPublishSubscribe exercises the broadcaster under
// concurrent publishers, subscribers, and unsubscribes. Run with -race it
// guards the lock discipline that keeps publish from sending on a closed
// channel.
func TestLogBroadcaster_ConcurrentPublishSubscribe(t *testing.T) {
	t.Parallel()
	b := newTestBroadcaster(t)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Publishers.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					emit(b, slog.LevelInfo, "concurrent", slog.String("component", "x"))
				}
			}
		}()
	}

	// Subscribers that churn: subscribe, drain a bit, unsubscribe.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				sub := b.Subscribe(LogFilter{})
				deadline := time.After(2 * time.Millisecond)
			drain:
				for {
					select {
					case <-sub.Lines():
					case <-sub.Throttle():
						sub.DrainDropped()
					case <-deadline:
						break drain
					}
				}
				sub.Close()
			}
		}()
	}

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()

	if got := b.SubscriberCount(); got != 0 {
		t.Errorf("SubscriberCount after churn = %d, want 0", got)
	}
}

// mustReceive returns the next line or fails on timeout.
func mustReceive(t *testing.T, sub *Subscription) LogEntry {
	t.Helper()
	select {
	case e, ok := <-sub.Lines():
		if !ok {
			t.Fatal("Lines channel closed unexpectedly")
		}
		return e
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for a log line")
		return LogEntry{}
	}
}

// assertNoMore fails if another line arrives promptly (used to confirm a
// filtered-out entry was not delivered).
func assertNoMore(t *testing.T, sub *Subscription) {
	t.Helper()
	select {
	case e := <-sub.Lines():
		t.Fatalf("unexpected extra line: %q", e.Message)
	case <-time.After(50 * time.Millisecond):
	}
}
