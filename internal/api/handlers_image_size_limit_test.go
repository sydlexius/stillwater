package api

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	img "github.com/sydlexius/stillwater/internal/image"
)

// writePaddedLogo creates a logo file of EXACTLY size bytes whose leading
// bytes are a valid, trimmable PNG.
//
// The shape is what makes the boundary tests meaningful. Padding a real PNG
// (rather than writing a file of zeroes) means the file stays DECODABLE when
// truncated: Go's PNG decoder stops at IEND and ignores trailing bytes. So an
// implementation that drops the load-bearing +1 reads exactly MaxDecodeBytes,
// gets a clean EOF, successfully decodes the prefix, and returns 200 -- which
// is precisely the silent-truncation regression these tests must catch. A
// zero-filled fixture could not catch it: the truncated prefix would fail to
// decode and the handler would return 500 either way, so the assertion would
// pass for the wrong reason.
//
// Truncate extends the file with a sparse hole, so a 25 MB fixture costs no
// real disk and no test-side allocation. That is what lets these tests
// exercise the REAL MaxDecodeBytes bound rather than a lowered test-only
// constant, leaving no production/test skew to reason about.
func writePaddedLogo(t *testing.T, path string, size int64) {
	t.Helper()

	m := image.NewRGBA(image.Rect(0, 0, 200, 100))
	for y := 20; y < 80; y++ {
		for x := 20; x < 180; x++ {
			m.Set(x, y, color.RGBA{R: 10, G: 20, B: 30, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, m); err != nil {
		t.Fatalf("encoding logo fixture: %v", err)
	}
	if int64(buf.Len()) > size {
		t.Fatalf("fixture PNG is %d bytes, larger than the requested size %d", buf.Len(), size)
	}

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("creating logo fixture: %v", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(buf.Bytes()); err != nil {
		t.Fatalf("writing logo fixture: %v", err)
	}
	// Extend to the exact target size with a sparse hole.
	if err := f.Truncate(size); err != nil {
		t.Fatalf("padding logo fixture to %d: %v", size, err)
	}

	// Assert the fixture is the size the test thinks it is. Without this the
	// boundary tests could pass vacuously against a fixture that was never
	// actually built at the bound.
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat logo fixture: %v", err)
	}
	if st.Size() != size {
		t.Fatalf("logo fixture is %d bytes, want exactly %d", st.Size(), size)
	}
}

// seedTrimArtist creates an artist with an image dir and returns both.
func seedTrimArtist(t *testing.T, name string) (*Router, *artist.Artist, string) {
	t.Helper()
	r, artistSvc := testRouterWithPlatform(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: name, SortName: name, Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	return r, a, dir
}

func postLogoTrim(t *testing.T, r *Router, a *artist.Artist) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/logo/trim", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleLogoTrim(w, req)
	return w
}

// A logo ONE BYTE past the bound must be rejected on the SIZE path with 413,
// and the canonical file must be left exactly as it was.
//
// This is the test that catches a truncating implementation. Because the
// fixture's prefix is a valid PNG (see writePaddedLogo), an implementation
// that reads only MaxDecodeBytes decodes that prefix happily and returns 200,
// having trimmed and SAVED a truncated logo over the operator's original.
// Asserting 413 -- not merely "not 200" -- pins the rejection to the size
// guard rather than to an incidental decode failure.
func TestHandleLogoTrim_OverLimit_Rejected413(t *testing.T) {
	t.Parallel()
	r, a, dir := seedTrimArtist(t, "Oversized Logo")
	logoPath := filepath.Join(dir, "logo.png")
	writePaddedLogo(t, logoPath, img.MaxDecodeBytes+1)
	before, err := os.ReadFile(logoPath)
	if err != nil {
		t.Fatalf("reading fixture back: %v", err)
	}

	w := postLogoTrim(t, r, a)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body: %s", w.Code, w.Body.String())
	}

	// Assert the OUTCOME on disk, not just the status code: the guard must
	// fail closed, leaving the original logo untouched.
	after, err := os.ReadFile(logoPath)
	if err != nil {
		t.Fatalf("reading logo after rejected trim: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("logo bytes changed on a rejected trim: %d bytes before, %d after", len(before), len(after))
	}

	// A rejected trim must not have reached the backup step either; a backup
	// directory here would mean the handler got past the guard.
	backup := filepath.Join(dir, img.BackupDirName, "logo", "logo.png")
	if _, err := os.Stat(backup); err == nil {
		t.Error("a rejected oversized trim created a backup; the guard ran too late")
	}
}

// A logo of EXACTLY MaxDecodeBytes is within budget and must still trim
// successfully. This pins the comparison as > rather than >=, which an
// off-by-one in the guard would flip, and proves the guard does not reject
// the legal maximum.
func TestHandleLogoTrim_AtLimit_Succeeds(t *testing.T) {
	t.Parallel()
	r, a, dir := seedTrimArtist(t, "At Limit Logo")
	logoPath := filepath.Join(dir, "logo.png")
	writePaddedLogo(t, logoPath, img.MaxDecodeBytes)
	before, err := os.ReadFile(logoPath)
	if err != nil {
		t.Fatalf("reading fixture back: %v", err)
	}

	w := postLogoTrim(t, r, a)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 at exactly the bound; body: %s", w.Code, w.Body.String())
	}

	// Assert the trim actually HAPPENED rather than trusting the status code:
	// the canonical file must have been rewritten, and the pre-trim original
	// must be recoverable from the backup.
	after, err := os.ReadFile(logoPath)
	if err != nil {
		t.Fatalf("reading logo after trim: %v", err)
	}
	if bytes.Equal(before, after) {
		t.Error("logo bytes unchanged after a successful trim at the bound; the trim did nothing")
	}
	backup := filepath.Join(dir, img.BackupDirName, "logo", "logo.png")
	gotBackup, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("expected a pre-trim backup at %s: %v", backup, err)
	}
	if !bytes.Equal(gotBackup, before) {
		t.Error("backup bytes do not match the pre-trim original")
	}
}
