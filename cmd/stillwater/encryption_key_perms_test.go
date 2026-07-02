package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestKeyFilePermsTooOpen(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		perm os.FileMode
		want bool
	}{
		{"owner-only 0600", 0o600, false},
		{"owner-only 0400", 0o400, false},
		{"group-readable 0640", 0o640, true},
		{"world-readable 0644", 0o644, true},
		{"group-writable 0620", 0o620, true},
		{"world-everything 0666", 0o666, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := keyFilePermsTooOpen(tc.perm); got != tc.want {
				t.Errorf("keyFilePermsTooOpen(%04o) = %v, want %v", tc.perm, got, tc.want)
			}
		})
	}
}

func TestWarnIfKeyFileTooOpen(t *testing.T) {
	t.Parallel()
	// A warning must fire for group/other-accessible modes and stay silent
	// for owner-only modes. os.Chmod is applied explicitly because the
	// process umask can alter modes passed to os.WriteFile.
	cases := []struct {
		name     string
		perm     os.FileMode
		wantWarn bool
	}{
		{"0600 stays silent", 0o600, false},
		{"0640 warns", 0o640, true},
		{"0644 warns", 0o644, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			keyPath := filepath.Join(t.TempDir(), "encryption.key")
			if err := os.WriteFile(keyPath, []byte("test-key\n"), 0o600); err != nil {
				t.Fatalf("writing key file: %v", err)
			}
			if err := os.Chmod(keyPath, tc.perm); err != nil {
				t.Fatalf("chmod key file: %v", err)
			}

			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

			warnIfKeyFileTooOpen(keyPath, logger)

			logged := buf.String()
			if tc.wantWarn {
				if !bytes.Contains(buf.Bytes(), []byte("chmod 600")) {
					t.Errorf("mode %04o: expected an insecure-permissions warning, got log output: %q", tc.perm, logged)
				}
				// The warning must never include the key contents.
				if bytes.Contains(buf.Bytes(), []byte("test-key")) {
					t.Errorf("mode %04o: warning leaked key contents: %q", tc.perm, logged)
				}
			} else if logged != "" {
				t.Errorf("mode %04o: expected no warning, got: %q", tc.perm, logged)
			}
		})
	}
}

func TestWarnIfKeyFileTooOpen_MissingFileIsSilent(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	warnIfKeyFileTooOpen(filepath.Join(t.TempDir(), "does-not-exist.key"), logger)

	if buf.Len() != 0 {
		t.Errorf("expected no log output for a missing file, got: %q", buf.String())
	}
}
