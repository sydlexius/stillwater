package mediabrowser

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// scanTasksTransport serves a canned /ScheduledTasks payload.
func scanTasksTransport(tasks []scheduledTask) *rawTransport {
	return &rawTransport{getResults: map[string]any{scheduledTasksPath: tasks}}
}

// TestLibraryScanIdleRaw_IdleOnlyOnAPositiveIdleReading is the happy path, and
// it is the ONLY input shape that may yield idle=true. Every other test in this
// file asserts the complement.
func TestLibraryScanIdleRaw_IdleOnlyOnAPositiveIdleReading(t *testing.T) {
	tr := scanTasksTransport([]scheduledTask{
		{Key: "ChapterImageExtraction", State: "Running"},
		{Key: "RefreshLibrary", State: "Idle"},
	})

	idle, err := LibraryScanIdleRaw(context.Background(), tr, noopClassifier)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !idle {
		t.Error("idle = false for a RefreshLibrary task the peer reports as Idle")
	}
}

// TestLibraryScanIdleRaw_RunningScanIsNotIdle is the case the whole primitive
// exists for: a scan in progress must be reported as such, because a caller
// reading an index mid-rebuild sees absences that are not real.
func TestLibraryScanIdleRaw_RunningScanIsNotIdle(t *testing.T) {
	for _, state := range []string{taskStateRunning, taskStateCancelling} {
		t.Run(state, func(t *testing.T) {
			tr := scanTasksTransport([]scheduledTask{{Key: "RefreshLibrary", State: state}})

			idle, err := LibraryScanIdleRaw(context.Background(), tr, noopClassifier)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if idle {
				t.Errorf("idle = true while the scan task reports %q", state)
			}
		})
	}
}

// TestLibraryScanIdleRaw_UnknownStateIsNotIdle covers a State value this code
// has never seen -- a future server release, or a value a fork invents. The
// safe reading of an unrecognized state is NOT idle: only the exact, known
// idle value may unlock a destructive caller.
//
// A negated check ("state != Running") would call this idle and fail open,
// which is the specific inversion the mutation table proves against.
func TestLibraryScanIdleRaw_UnknownStateIsNotIdle(t *testing.T) {
	tr := scanTasksTransport([]scheduledTask{{Key: "RefreshLibrary", State: "Paused"}})

	idle, err := LibraryScanIdleRaw(context.Background(), tr, noopClassifier)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if idle {
		t.Error("idle = true for an unrecognized state; an unknown state must never read as idle")
	}
}

// TestLibraryScanIdleRaw_IdleIsCaseInsensitive guards the casing
// inconsistency both servers have shown across versions (the same reason
// TypeMusicArtist is matched case-insensitively). This one asserts the
// PERMISSIVE direction is safe to have: it only ever admits the known idle
// word, never an unknown one.
func TestLibraryScanIdleRaw_IdleIsCaseInsensitive(t *testing.T) {
	for _, state := range []string{"idle", "IDLE", " Idle "} {
		t.Run(state, func(t *testing.T) {
			tr := scanTasksTransport([]scheduledTask{{Key: "RefreshLibrary", State: state}})

			idle, err := LibraryScanIdleRaw(context.Background(), tr, noopClassifier)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !idle {
				t.Errorf("idle = false for state %q; casing/whitespace must not change the verdict", state)
			}
		})
	}
}

// TestLibraryScanIdleRaw_UnreachablePeerIsNotIdle is the headline fail-closed
// case. An unreachable peer answers NOTHING, and "no answer" must never render
// as "no scan running" -- that is the "a zero renders as clean" trap from
// internal/dupimages/cache.go, which is what makes a caller confidently
// destroy state on the strength of a failed request.
func TestLibraryScanIdleRaw_UnreachablePeerIsNotIdle(t *testing.T) {
	tr := &rawTransport{getErr: errors.New("dial tcp: connection refused")}

	idle, err := LibraryScanIdleRaw(context.Background(), tr, noopClassifier)
	if err == nil {
		t.Fatal("expected an error from an unreachable peer")
	}
	if idle {
		t.Error("idle = true from an unreachable peer; a failed request is not evidence of an idle scan")
	}
}

// TestLibraryScanIdleRaw_MissingScanTaskIsAnErrorNotIdle is the subtle one.
// A scheduled-task list with no RefreshLibrary entry means the peer did not
// answer the question -- an older server, a permission-filtered list, a
// renamed key. Reporting idle there would be a green light derived from zero
// evidence, so it must be an error.
//
// This is the assertion most likely to be "simplified" away by someone who
// reads the absence as benign, which is exactly why it is pinned.
func TestLibraryScanIdleRaw_MissingScanTaskIsAnErrorNotIdle(t *testing.T) {
	tr := scanTasksTransport([]scheduledTask{
		{Key: "ChapterImageExtraction", State: "Idle"},
		{Key: "CleanCache", State: "Idle"},
	})

	idle, err := LibraryScanIdleRaw(context.Background(), tr, noopClassifier)
	if err == nil {
		t.Fatal("expected an error when no library-scan task is present")
	}
	if idle {
		t.Error("idle = true with no scan task in the list; silence is not a verified answer")
	}
}

// TestLibraryScanIdleRaw_EmptyTaskListIsAnErrorNotIdle is the degenerate form
// of the previous case: a peer that returns an empty list (or a permission
// filter that empties it) tells us nothing, and nothing is not idle.
func TestLibraryScanIdleRaw_EmptyTaskListIsAnErrorNotIdle(t *testing.T) {
	tr := scanTasksTransport(nil)

	idle, err := LibraryScanIdleRaw(context.Background(), tr, noopClassifier)
	if err == nil {
		t.Fatal("expected an error for an empty scheduled-task list")
	}
	if idle {
		t.Error("idle = true for an empty task list")
	}
}

// TestLibraryScanIdleRaw_MatchesOnKeyNotName pins the Key-not-Name rule. A
// non-English server localizes Name; matching on it would report idle for a
// running scan on every such server -- a fail-open answer that would be
// invisible in an English test suite.
//
// The fixture gives the scan task a localized Name and a DIFFERENT task the
// English name, so an implementation that matched on Name would read the wrong
// row's state and return the wrong verdict.
func TestLibraryScanIdleRaw_MatchesOnKeyNotName(t *testing.T) {
	// The fixture only expresses the Key-vs-Name distinction if the Names
	// actually DISAGREE with the Keys. Previously both entries omitted Name
	// entirely, so a Name-matching implementation would have failed to match
	// anything and the test would still have passed -- for the wrong reason.
	tr := scanTasksTransport([]scheduledTask{
		// The REAL scan task, running, as a non-English server presents it:
		// the stable Key, with a localized display name.
		{Key: "RefreshLibrary", Name: "Bibliothek aktualisieren", State: "Running"},
		// A decoy carrying the ENGLISH name a Name-matcher would look for,
		// and idle. Match on Name and you read this row: idle = true, while a
		// library scan is actually running.
		{Key: "SomeOtherTask", Name: "Scan Media Library", State: "Idle"},
	})

	idle, err := LibraryScanIdleRaw(context.Background(), tr, noopClassifier)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if idle {
		t.Error("idle = true: the running RefreshLibrary task was not matched by Key")
	}
}

// TestLibraryScanIdleRaw_ErrorNamesTheMissingKey keeps the not-found error
// actionable: an operator reading it should learn WHICH task was looked for,
// not just that something was absent.
func TestLibraryScanIdleRaw_ErrorNamesTheMissingKey(t *testing.T) {
	tr := scanTasksTransport([]scheduledTask{{Key: "CleanCache", State: "Idle"}})

	_, err := LibraryScanIdleRaw(context.Background(), tr, noopClassifier)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := err.Error(); !strings.Contains(got, "RefreshLibrary") {
		t.Errorf("error %q does not name the task key it looked for", got)
	}
}
