package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Installer downloads and atomically replaces the running binary.
type Installer struct {
	client *http.Client
	logger *slog.Logger
}

// NewInstaller creates an Installer. client may be nil; a default client with a
// 5-minute timeout is used for large binary downloads.
func NewInstaller(client *http.Client, logger *slog.Logger) *Installer {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Installer{client: client, logger: logger}
}

// Install downloads asset, verifies its SHA256 checksum if asset.ChecksumURL is
// non-empty, then atomically replaces the running binary.
//
// Replacement steps:
//  1. Download to <execPath>.tmp
//  2. Verify SHA256 against the .sha256 file (if ChecksumURL is set)
//  3. Rename <execPath> to <execPath>.bak
//  4. Rename <execPath>.tmp to <execPath>
//  5. Remove <execPath>.bak
//
// If step 4 fails the backup is restored from <execPath>.bak before returning.
func (inst *Installer) Install(ctx context.Context, asset AssetInfo) error {
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding executable path: %w", err)
	}
	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		return fmt.Errorf("resolving executable symlinks: %w", err)
	}

	tmpPath := execPath + ".tmp"
	bakPath := execPath + ".bak"

	// Step 1: Download to temp file.
	if err := inst.download(ctx, asset.DownloadURL, tmpPath, execPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("downloading update: %w", err)
	}

	// Step 2: Verify checksum.
	if asset.ChecksumURL == "" {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("asset %q has no checksum URL; refusing to install unverified binary", asset.Name)
	}
	if err := inst.verifyChecksum(ctx, tmpPath, asset.ChecksumURL); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("checksum verification failed: %w", err)
	}

	// Step 3: Backup current binary.
	_ = os.Remove(bakPath) // ignore error if no previous backup
	if err := os.Rename(execPath, bakPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("backing up current binary: %w", err)
	}

	// Step 4: Atomic install.
	if err := os.Rename(tmpPath, execPath); err != nil {
		// Rollback: restore from backup.
		if rbErr := os.Rename(bakPath, execPath); rbErr != nil {
			inst.logger.Error("rollback failed after install error",
				"backup", bakPath, "exec", execPath, "error", rbErr)
		}
		return fmt.Errorf("installing new binary: %w", err)
	}

	// Step 5: Remove backup.
	_ = os.Remove(bakPath)
	inst.logger.Info("update installed successfully", "path", execPath, "asset", asset.Name)
	return nil
}

// download fetches url to destPath, preserving mode bits from refPath.
func (inst *Installer) download(ctx context.Context, url, destPath, refPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}

	resp, err := inst.client.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned %d", resp.StatusCode)
	}

	// Preserve mode bits from the existing binary when possible.
	perm := os.FileMode(0o755)
	if info, err := os.Stat(refPath); err == nil {
		perm = info.Mode().Perm()
	}

	f, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm) //nolint:gosec // G304: destPath is a controlled temp path in the binary's own directory, not user input
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("closing temp file: %w", cerr)
		}
	}()

	if _, err = io.Copy(f, resp.Body); err != nil {
		return fmt.Errorf("writing temp file: %w", err)
	}
	return nil
}

// verifyChecksum fetches checksumURL (a plain-text file containing
// "<hex>  <filename>" or just "<hex>"), computes SHA256 of filePath, and
// returns an error if they do not match.
func (inst *Installer) verifyChecksum(ctx context.Context, filePath, checksumURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checksumURL, nil)
	if err != nil {
		return fmt.Errorf("building checksum request: %w", err)
	}
	resp, err := inst.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetching checksum: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("checksum server returned %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading checksum: %w", err)
	}

	// Accept "<hex>  <filename>" or bare "<hex>".
	line := string(raw)
	expected := line
	if idx := len(line); idx > 64 {
		expected = line[:64]
	}
	expected = trimSpaces(expected)

	// Compute local SHA256.
	f, err := os.Open(filePath) //nolint:gosec // G304: filePath is our own temp download, not user input
	if err != nil {
		return fmt.Errorf("opening file for checksum: %w", err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hashing file: %w", err)
	}
	actual := hex.EncodeToString(h.Sum(nil))

	if actual != expected {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expected, actual)
	}
	return nil
}

// trimSpaces removes all whitespace characters from s.
func trimSpaces(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != ' ' && c != '\t' && c != '\r' && c != '\n' {
			b = append(b, c)
		}
	}
	return string(b)
}
