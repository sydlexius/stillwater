package watcher

import (
	"testing"
	"time"
)

func TestProbeFSNotify_LocalDir(t *testing.T) {
	dir := t.TempDir()
	supported := ProbeFSNotify(dir, 2*time.Second)
	if !supported {
		t.Error("expected fsnotify to be supported on local temp dir")
	}
}

func TestProbeFSNotify_NonexistentDir(t *testing.T) {
	supported := ProbeFSNotify("/nonexistent/path/that/does/not/exist", 500*time.Millisecond)
	if supported {
		t.Error("expected fsnotify to report unsupported for nonexistent dir")
	}
}

func TestProbeFSNotify_Timeout(t *testing.T) {
	// A very short timeout should cause the probe to fail even on a local dir
	// in some cases, but on fast systems it may still succeed.
	// This test verifies the function does not hang.
	dir := t.TempDir()
	_ = ProbeFSNotify(dir, 1*time.Nanosecond)
	// No assertion on result; just verify it returns without hanging.
}

func TestProbeCache_GetSet(t *testing.T) {
	pc := NewProbeCache()

	// Get on empty cache.
	_, ok := pc.Get("/some/path")
	if ok {
		t.Error("expected ok=false for unprobed path")
	}

	// Set and get.
	pc.Set("/some/path", true)
	supported, ok := pc.Get("/some/path")
	if !ok || !supported {
		t.Errorf("expected supported=true, ok=true; got supported=%v, ok=%v", supported, ok)
	}

	pc.Set("/other/path", false)
	supported, ok = pc.Get("/other/path")
	if !ok || supported {
		t.Errorf("expected supported=false, ok=true; got supported=%v, ok=%v", supported, ok)
	}
}
