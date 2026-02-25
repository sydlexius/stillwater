package rule

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/provider"
)

// makeTestJPEG encodes a solid-color JPEG of the given dimensions.
func makeTestJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: 128, G: 64, B: 32, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatalf("encoding test jpeg: %v", err)
	}
	return buf.Bytes()
}

// mockImageProvider records FetchImages calls for testing.
type mockImageProvider struct {
	result *provider.FetchResult
	err    error
	calls  int
}

func (m *mockImageProvider) FetchImages(_ context.Context, _ string, _ map[provider.ProviderName]string) (*provider.FetchResult, error) {
	m.calls++
	return m.result, m.err
}

func TestNFOFixer_CanFix(t *testing.T) {
	f := &NFOFixer{}

	if !f.CanFix(&Violation{RuleID: RuleNFOExists}) {
		t.Error("NFOFixer should handle nfo_exists")
	}
	if f.CanFix(&Violation{RuleID: RuleThumbExists}) {
		t.Error("NFOFixer should not handle thumb_exists")
	}
}

func TestNFOFixer_Fix(t *testing.T) {
	dir := t.TempDir()
	a := &artist.Artist{
		Name:          "Test Artist",
		SortName:      "Test Artist",
		Path:          dir,
		MusicBrainzID: "abc-123",
	}

	f := &NFOFixer{}
	fr, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleNFOExists})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !fr.Fixed {
		t.Errorf("Fixed = false, want true")
	}
	if fr.RuleID != RuleNFOExists {
		t.Errorf("RuleID = %q, want %q", fr.RuleID, RuleNFOExists)
	}

	// Verify the NFO was created
	nfoPath := filepath.Join(dir, "artist.nfo")
	data, err := os.ReadFile(nfoPath)
	if err != nil {
		t.Fatalf("reading nfo: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("nfo file is empty")
	}

	// Verify the NFO content is valid
	parsed, err := nfo.Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parsing created nfo: %v", err)
	}
	if parsed.Name != "Test Artist" {
		t.Errorf("nfo Name = %q, want %q", parsed.Name, "Test Artist")
	}
	if parsed.MusicBrainzArtistID != "abc-123" {
		t.Errorf("nfo MBID = %q, want %q", parsed.MusicBrainzArtistID, "abc-123")
	}

	// Verify artist flag was updated
	if !a.NFOExists {
		t.Error("artist.NFOExists should be true after fix")
	}
}

func TestMetadataFixer_CanFix(t *testing.T) {
	f := &MetadataFixer{}

	if !f.CanFix(&Violation{RuleID: RuleNFOHasMBID}) {
		t.Error("MetadataFixer should handle nfo_has_mbid")
	}
	if !f.CanFix(&Violation{RuleID: RuleBioExists}) {
		t.Error("MetadataFixer should handle bio_exists")
	}
	if f.CanFix(&Violation{RuleID: RuleThumbExists}) {
		t.Error("MetadataFixer should not handle thumb_exists")
	}
}

func TestImageFixer_CanFix(t *testing.T) {
	f := &ImageFixer{}

	for _, ruleID := range []string{RuleThumbExists, RuleFanartExists, RuleLogoExists, RuleThumbSquare, RuleThumbMinRes} {
		if !f.CanFix(&Violation{RuleID: ruleID}) {
			t.Errorf("ImageFixer should handle %s", ruleID)
		}
	}

	if f.CanFix(&Violation{RuleID: RuleNFOExists}) {
		t.Error("ImageFixer should not handle nfo_exists")
	}
}

func TestImageFixer_Fix_NoMBID(t *testing.T) {
	f := &ImageFixer{}
	a := &artist.Artist{Name: "Test", MusicBrainzID: ""}

	fr, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleThumbExists})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if fr.Fixed {
		t.Error("should not fix when MBID is empty")
	}
}

func TestRuleToImageType(t *testing.T) {
	tests := []struct {
		ruleID string
		want   string
	}{
		{RuleThumbExists, "thumb"},
		{RuleThumbSquare, "thumb"},
		{RuleThumbMinRes, "thumb"},
		{RuleFanartExists, "fanart"},
		{RuleLogoExists, "logo"},
		{RuleNFOExists, ""},
		{RuleBioExists, ""},
	}

	for _, tt := range tests {
		if got := ruleToImageType(tt.ruleID); got != tt.want {
			t.Errorf("ruleToImageType(%q) = %q, want %q", tt.ruleID, got, tt.want)
		}
	}
}

func TestSetImageFlag(t *testing.T) {
	a := &artist.Artist{}

	setImageFlag(a, "thumb")
	if !a.ThumbExists {
		t.Error("ThumbExists should be true")
	}

	setImageFlag(a, "fanart")
	if !a.FanartExists {
		t.Error("FanartExists should be true")
	}

	setImageFlag(a, "logo")
	if !a.LogoExists {
		t.Error("LogoExists should be true")
	}

	setImageFlag(a, "banner")
	if !a.BannerExists {
		t.Error("BannerExists should be true")
	}
}

func TestImageFixer_CanFix_NewRules(t *testing.T) {
	f := &ImageFixer{}

	newRules := []string{
		RuleFanartMinRes, RuleFanartAspect, RuleLogoMinRes, RuleBannerExists, RuleBannerMinRes,
	}
	for _, ruleID := range newRules {
		if !f.CanFix(&Violation{RuleID: ruleID}) {
			t.Errorf("ImageFixer should handle %s", ruleID)
		}
	}
}

func TestRuleToImageType_NewRules(t *testing.T) {
	tests := []struct {
		ruleID string
		want   string
	}{
		{RuleFanartMinRes, "fanart"},
		{RuleFanartAspect, "fanart"},
		{RuleLogoMinRes, "logo"},
		{RuleBannerExists, "banner"},
		{RuleBannerMinRes, "banner"},
	}
	for _, tt := range tests {
		if got := ruleToImageType(tt.ruleID); got != tt.want {
			t.Errorf("ruleToImageType(%q) = %q, want %q", tt.ruleID, got, tt.want)
		}
	}
}

func TestPipeline_PendingChoiceViolation(t *testing.T) {
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	ctx := context.Background()

	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	a := &artist.Artist{
		Name:     "Candidate Test",
		SortName: "Candidate Test",
		Path:     t.TempDir(),
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	candidates := []ImageCandidate{
		{URL: "http://example.com/img1.jpg", Width: 1920, Height: 1080, Source: "prov", ImageType: "fanart"},
		{URL: "http://example.com/img2.jpg", Width: 3840, Height: 2160, Source: "prov", ImageType: "fanart"},
	}
	// Fixer returns multiple candidates (pending_choice)
	fixer := &mockFixer{
		canFix: true,
		result: &FixResult{
			RuleID:     RuleNFOExists,
			Fixed:      false,
			Message:    "multiple candidates",
			Candidates: candidates,
		},
	}

	engine := NewEngine(ruleSvc, db, testLogger())
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{fixer}, testLogger())

	result, err := pipeline.RunAll(ctx)
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if result.FixesSucceeded != 0 {
		t.Errorf("FixesSucceeded = %d, want 0 (pending_choice)", result.FixesSucceeded)
	}

	// Verify violation was persisted as pending_choice with candidates
	violations, err := ruleSvc.ListViolations(ctx, ViolationStatusPendingChoice)
	if err != nil {
		t.Fatalf("ListViolations: %v", err)
	}
	found := false
	for _, v := range violations {
		if v.ArtistID == a.ID {
			found = true
			if v.Status != ViolationStatusPendingChoice {
				t.Errorf("status = %q, want %q", v.Status, ViolationStatusPendingChoice)
			}
			if len(v.Candidates) != 2 {
				t.Errorf("Candidates len = %d, want 2", len(v.Candidates))
			}
		}
	}
	if !found {
		t.Error("expected pending_choice violation for artist, none found")
	}
}

// mockFixer is a test helper that records calls.
type mockFixer struct {
	canFix bool
	result *FixResult
	err    error
	calls  int
}

func (m *mockFixer) CanFix(_ *Violation) bool {
	return m.canFix
}

func (m *mockFixer) Fix(_ context.Context, _ *artist.Artist, v *Violation) (*FixResult, error) {
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	if m.result != nil {
		return m.result, nil
	}
	return &FixResult{RuleID: v.RuleID, Fixed: true, Message: "mock fixed"}, nil
}

func TestPipeline_RunAll(t *testing.T) {
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	ctx := context.Background()

	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	// Create a test artist with some violations
	a := &artist.Artist{
		Name:     "Pipeline Test",
		SortName: "Pipeline Test",
		Path:     t.TempDir(),
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	engine := NewEngine(ruleSvc, db, testLogger())
	fixer := &mockFixer{canFix: true}
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{fixer}, testLogger())

	result, err := pipeline.RunAll(ctx)
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}

	if result.ArtistsProcessed != 1 {
		t.Errorf("ArtistsProcessed = %d, want 1", result.ArtistsProcessed)
	}
	if result.ViolationsFound == 0 {
		t.Error("expected at least one violation for empty artist")
	}
	if fixer.calls == 0 {
		t.Error("expected fixer to be called at least once")
	}
}

func TestPipeline_RunRule(t *testing.T) {
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	ctx := context.Background()

	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	a := &artist.Artist{
		Name:     "Rule Test",
		SortName: "Rule Test",
		Path:     t.TempDir(),
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	engine := NewEngine(ruleSvc, db, testLogger())
	fixer := &mockFixer{canFix: true}
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{fixer}, testLogger())

	result, err := pipeline.RunRule(ctx, RuleNFOExists)
	if err != nil {
		t.Fatalf("RunRule: %v", err)
	}

	if result.ArtistsProcessed != 1 {
		t.Errorf("ArtistsProcessed = %d, want 1", result.ArtistsProcessed)
	}
	if result.ViolationsFound != 1 {
		t.Errorf("ViolationsFound = %d, want 1 (nfo_exists)", result.ViolationsFound)
	}
	if result.FixesAttempted != 1 {
		t.Errorf("FixesAttempted = %d, want 1", result.FixesAttempted)
	}
}

func TestPipeline_SkipsExcludedArtists(t *testing.T) {
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	ctx := context.Background()

	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	a := &artist.Artist{
		Name:            "Various Artists",
		SortName:        "Various Artists",
		Path:            t.TempDir(),
		IsExcluded:      true,
		ExclusionReason: "default exclusion list",
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	engine := NewEngine(ruleSvc, db, testLogger())
	fixer := &mockFixer{canFix: true}
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{fixer}, testLogger())

	result, err := pipeline.RunAll(ctx)
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}

	if result.ArtistsProcessed != 0 {
		t.Errorf("ArtistsProcessed = %d, want 0 (excluded)", result.ArtistsProcessed)
	}
}

func TestPipeline_NoFixerAvailable(t *testing.T) {
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	ctx := context.Background()

	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	a := &artist.Artist{
		Name:     "No Fixer",
		SortName: "No Fixer",
		Path:     t.TempDir(),
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	engine := NewEngine(ruleSvc, db, testLogger())
	// Register a fixer that can't fix anything
	fixer := &mockFixer{canFix: false}
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{fixer}, testLogger())

	result, err := pipeline.RunAll(ctx)
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}

	for _, fr := range result.Results {
		if fr.Fixed {
			t.Errorf("no fix should succeed when fixer.CanFix is false")
		}
		if fr.Message != "no fixer available" {
			t.Errorf("Message = %q, want 'no fixer available'", fr.Message)
		}
	}
}

func TestFilterCandidatesByResolution(t *testing.T) {
	logger := testLogger()

	candidates := []provider.ImageResult{
		{URL: "a", Width: 200, Height: 200},   // below minimum
		{URL: "b", Width: 1000, Height: 1000}, // passes
		{URL: "c", Width: 0, Height: 0},       // unknown dims, always passes
		{URL: "d", Width: 800, Height: 800},   // below existing (900x900)
	}

	got := filterCandidatesByResolution(candidates, 500, 500, 900, 900, logger)

	if len(got) != 2 {
		t.Fatalf("want 2 candidates, got %d: %v", len(got), got)
	}
	if got[0].URL != "b" {
		t.Errorf("first candidate URL = %q, want %q", got[0].URL, "b")
	}
	if got[1].URL != "c" {
		t.Errorf("second candidate URL = %q, want %q", got[1].URL, "c")
	}
}

func TestFilterCandidatesByResolution_NoConstraints(t *testing.T) {
	logger := testLogger()
	candidates := []provider.ImageResult{
		{URL: "a", Width: 100, Height: 100},
		{URL: "b", Width: 50, Height: 50},
	}
	got := filterCandidatesByResolution(candidates, 0, 0, 0, 0, logger)
	if len(got) != 2 {
		t.Errorf("want 2 candidates with no constraints, got %d", len(got))
	}
}

func TestImageFixer_Fix_ResolutionGate(t *testing.T) {
	mock := &mockImageProvider{
		result: &provider.FetchResult{
			Images: []provider.ImageResult{
				{URL: "http://example.com/low.jpg", Type: "thumb", Width: 300, Height: 300},
			},
		},
	}

	f := NewImageFixer(mock, testLogger())
	a := &artist.Artist{
		Name:          "Gate Test",
		MusicBrainzID: "mbid-gate",
		Path:          t.TempDir(),
	}
	v := &Violation{
		RuleID: RuleThumbMinRes,
		Config: RuleConfig{MinWidth: 1000, MinHeight: 1000},
	}

	fr, err := f.Fix(context.Background(), a, v)
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if fr.Fixed {
		t.Error("Fixed = true; want false when all candidates are below minimum")
	}
	if !strings.Contains(fr.Message, "no thumb candidates meet minimum resolution requirements") {
		t.Errorf("Message = %q; want 'no thumb candidates meet minimum resolution requirements'", fr.Message)
	}
}

func TestImageFixer_FetchImages_Cached(t *testing.T) {
	mock := &mockImageProvider{
		result: &provider.FetchResult{
			Images: []provider.ImageResult{
				{URL: "http://example.com/img.jpg", Type: "thumb", Width: 1920, Height: 1080},
			},
		},
	}

	f := NewImageFixer(mock, testLogger())
	a := &artist.Artist{
		Name:          "Cache Test",
		MusicBrainzID: "mbid-cache",
		Path:          t.TempDir(),
	}

	// First call: thumb_min_res -- one candidate passes, SelectBestCandidate=false
	// so it returns pending_choice (multiple not needed here -- just one candidate
	// means it would try to download; use SelectBestCandidate to avoid HTTP).
	// Use a resolution constraint to get a predictable no-download path instead.
	v1 := &Violation{
		RuleID: RuleThumbMinRes,
		Config: RuleConfig{MinWidth: 5000, MinHeight: 5000}, // forces "no candidates" path
	}
	if _, err := f.Fix(context.Background(), a, v1); err != nil {
		t.Fatalf("Fix v1: %v", err)
	}

	// Second call: fanart_min_res -- different rule, same MBID
	v2 := &Violation{
		RuleID: RuleFanartMinRes,
		Config: RuleConfig{MinWidth: 5000, MinHeight: 5000},
	}
	if _, err := f.Fix(context.Background(), a, v2); err != nil {
		t.Fatalf("Fix v2: %v", err)
	}

	if mock.calls != 1 {
		t.Errorf("FetchImages called %d times; want 1 (cache hit on second call)", mock.calls)
	}
}

// TestImageFixer_Fix_PostDownloadDimensionGate verifies that a candidate with
// unknown provider dimensions (Width=0, Height=0 -- as returned by FanartTV and
// Deezer) is still rejected when its actual downloaded pixels fall below the
// existing image's resolution. This is the real-world "Adie thumbnail clobber"
// scenario.
func TestImageFixer_Fix_PostDownloadDimensionGate(t *testing.T) {
	// Serve a 200x200 JPEG from an HTTP test server (simulates a low-res FanartTV candidate).
	smallJPEG := makeTestJPEG(t, 200, 200)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(smallJPEG)
	}))
	defer srv.Close()

	// The mock provider returns a candidate with NO dimension info (0x0) -- exactly
	// what FanartTV and Deezer produce.
	mock := &mockImageProvider{
		result: &provider.FetchResult{
			Images: []provider.ImageResult{
				{URL: srv.URL + "/low.jpg", Type: "thumb", Width: 0, Height: 0, Source: "fanarttv"},
			},
		},
	}

	dir := t.TempDir()

	// Write a high-res existing thumbnail (1500x1500) so the gate can compare.
	highResJPEG := makeTestJPEG(t, 1500, 1500)
	if err := os.WriteFile(filepath.Join(dir, "folder.jpg"), highResJPEG, 0o644); err != nil {
		t.Fatalf("writing existing thumb: %v", err)
	}

	f := NewImageFixer(mock, testLogger())
	a := &artist.Artist{
		Name:          "Adie",
		MusicBrainzID: "mbid-adie",
		Path:          dir,
		ThumbExists:   true,
	}
	v := &Violation{
		RuleID: RuleThumbMinRes,
		Config: RuleConfig{MinWidth: 500, MinHeight: 500, SelectBestCandidate: true},
	}

	fr, err := f.Fix(context.Background(), a, v)
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if fr.Fixed {
		t.Error("Fixed = true; low-res candidate should not overwrite higher-res existing image")
	}

	// Confirm the original high-res file is still in place and unchanged.
	remaining, err := os.ReadFile(filepath.Join(dir, "folder.jpg"))
	if err != nil {
		t.Fatalf("reading existing thumb after fix: %v", err)
	}
	if !bytes.Equal(remaining, highResJPEG) {
		t.Error("existing thumbnail was modified; it should have been left untouched")
	}
}

// TestImageFixer_Fix_ThumbSquare_ResolutionGate verifies that a thumb_square
// violation (not a min-res rule) still protects a high-res existing image from
// being overwritten by a lower-res candidate.
func TestImageFixer_Fix_ThumbSquare_ResolutionGate(t *testing.T) {
	smallJPEG := makeTestJPEG(t, 300, 300)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(smallJPEG)
	}))
	defer srv.Close()

	mock := &mockImageProvider{
		result: &provider.FetchResult{
			Images: []provider.ImageResult{
				{URL: srv.URL + "/square.jpg", Type: "thumb", Width: 0, Height: 0, Source: "fanarttv"},
			},
		},
	}

	dir := t.TempDir()
	highResJPEG := makeTestJPEG(t, 1200, 800) // non-square, high-res
	if err := os.WriteFile(filepath.Join(dir, "folder.jpg"), highResJPEG, 0o644); err != nil {
		t.Fatalf("writing existing thumb: %v", err)
	}

	f := NewImageFixer(mock, testLogger())
	a := &artist.Artist{
		Name:          "Adie",
		MusicBrainzID: "mbid-adie-sq",
		Path:          dir,
		ThumbExists:   true,
	}
	v := &Violation{
		RuleID: RuleThumbSquare,
		// No MinWidth/MinHeight; AspectRatio/Tolerance only
		Config: RuleConfig{AspectRatio: 1.0, Tolerance: 0.1, SelectBestCandidate: true},
	}

	fr, err := f.Fix(context.Background(), a, v)
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if fr.Fixed {
		t.Error("Fixed = true; 300x300 candidate should not overwrite 1200x800 (960000 px) existing image")
	}

	remaining, err := os.ReadFile(filepath.Join(dir, "folder.jpg"))
	if err != nil {
		t.Fatalf("reading existing thumb: %v", err)
	}
	if !bytes.Equal(remaining, highResJPEG) {
		t.Error("existing high-res thumbnail was modified; it should have been left untouched")
	}
}

func TestExistingImageFileNames_OnlyWritesExistingFiles(t *testing.T) {
	dir := t.TempDir()

	// Only folder.jpg exists; artist.jpg and poster.jpg do not
	if err := os.WriteFile(filepath.Join(dir, "folder.jpg"), []byte("x"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	got := existingImageFileNames(dir, "thumb")
	if len(got) != 1 || got[0] != "folder.jpg" {
		t.Errorf("existingImageFileNames = %v; want [folder.jpg]", got)
	}
}

func TestExistingImageFileNames_FallsBackToPrimary(t *testing.T) {
	dir := t.TempDir() // empty -- no existing files

	got := existingImageFileNames(dir, "thumb")
	if len(got) != 1 {
		t.Fatalf("want 1 (primary fallback), got %d: %v", len(got), got)
	}
	// primary for thumb is folder.jpg
	if got[0] != "folder.jpg" {
		t.Errorf("primary fallback = %q; want folder.jpg", got[0])
	}
}

func TestWriteArtistNFO(t *testing.T) {
	dir := t.TempDir()
	a := &artist.Artist{
		Name:          "WriteTest",
		SortName:      "WriteTest",
		Path:          dir,
		MusicBrainzID: "test-mbid",
	}

	writeArtistNFO(a, nil)

	data, err := os.ReadFile(filepath.Join(dir, "artist.nfo"))
	if err != nil {
		t.Fatalf("reading nfo: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("nfo file is empty")
	}

	parsed, err := nfo.Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parsing nfo: %v", err)
	}
	if parsed.MusicBrainzArtistID != "test-mbid" {
		t.Errorf("MBID = %q, want 'test-mbid'", parsed.MusicBrainzArtistID)
	}
}
