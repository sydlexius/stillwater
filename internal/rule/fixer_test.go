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

	engine := NewEngine(ruleSvc, db, nil, testLogger())
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

	engine := NewEngine(ruleSvc, db, nil, testLogger())
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

	engine := NewEngine(ruleSvc, db, nil, testLogger())
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

	engine := NewEngine(ruleSvc, db, nil, testLogger())
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

	engine := NewEngine(ruleSvc, db, nil, testLogger())
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

	f := NewImageFixer(mock, nil, testLogger())
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

	f := NewImageFixer(mock, nil, testLogger())
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

	f := NewImageFixer(mock, nil, testLogger())
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

	f := NewImageFixer(mock, nil, testLogger())
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

	got := existingImageFileNames(context.Background(), dir, "thumb", nil)
	if len(got) != 1 || got[0] != "folder.jpg" {
		t.Errorf("existingImageFileNames = %v; want [folder.jpg]", got)
	}
}

func TestExistingImageFileNames_FallsBackToPrimary(t *testing.T) {
	dir := t.TempDir() // empty -- no existing files

	got := existingImageFileNames(context.Background(), dir, "thumb", nil)
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

func TestExtraneousImagesFixer_CanFix(t *testing.T) {
	f := NewExtraneousImagesFixer(nil, testLogger())
	if !f.CanFix(&Violation{RuleID: RuleExtraneousImages}) {
		t.Error("should handle extraneous_images")
	}
	if f.CanFix(&Violation{RuleID: RuleNFOExists}) {
		t.Error("should not handle nfo_exists")
	}
}

func TestExtraneousImagesFixer_Fix_DeletesExtraneous(t *testing.T) {
	dir := t.TempDir()
	// Create canonical files
	if err := os.WriteFile(filepath.Join(dir, "folder.jpg"), []byte("thumb"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fanart.jpg"), []byte("fanart"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Create extraneous files
	if err := os.WriteFile(filepath.Join(dir, "random.jpg"), []byte("extra1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "old-poster.png"), []byte("extra2"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &artist.Artist{Name: "Fixer Test", Path: dir}
	f := NewExtraneousImagesFixer(nil, testLogger())

	result, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleExtraneousImages})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !result.Fixed {
		t.Error("Fixed = false, want true")
	}
	if !strings.Contains(result.Message, "random.jpg") {
		t.Errorf("Message should mention random.jpg: %s", result.Message)
	}

	// Canonical files should remain
	if _, err := os.Stat(filepath.Join(dir, "folder.jpg")); err != nil {
		t.Error("folder.jpg should not have been deleted")
	}
	if _, err := os.Stat(filepath.Join(dir, "fanart.jpg")); err != nil {
		t.Error("fanart.jpg should not have been deleted")
	}
	// Extraneous files should be gone
	if _, err := os.Stat(filepath.Join(dir, "random.jpg")); !os.IsNotExist(err) {
		t.Error("random.jpg should have been deleted")
	}
	if _, err := os.Stat(filepath.Join(dir, "old-poster.png")); !os.IsNotExist(err) {
		t.Error("old-poster.png should have been deleted")
	}
}

func TestExtraneousImagesFixer_Fix_NoExtraneous(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "folder.jpg"), []byte("thumb"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &artist.Artist{Name: "Clean Artist", Path: dir}
	f := NewExtraneousImagesFixer(nil, testLogger())

	result, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleExtraneousImages})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if result.Fixed {
		t.Error("Fixed = true, want false when no extraneous files exist")
	}
}

func TestExtraneousImagesFixer_Fix_EmptyPath(t *testing.T) {
	a := &artist.Artist{Name: "No Path"}
	f := NewExtraneousImagesFixer(nil, testLogger())

	result, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleExtraneousImages})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if result.Fixed {
		t.Error("Fixed = true, want false for empty path")
	}
	if result.Message != "artist has no path" {
		t.Errorf("Message = %q, want 'artist has no path'", result.Message)
	}
}

func TestLogoTrimFixer_CanFix(t *testing.T) {
	f := NewLogoTrimFixer(nil, testLogger())
	if !f.CanFix(&Violation{RuleID: RuleLogoTrimmable}) {
		t.Error("should handle logo_trimmable")
	}
	if f.CanFix(&Violation{RuleID: RuleNFOExists}) {
		t.Error("should not handle nfo_exists")
	}
}

func TestLogoTrimFixer_Fix_TrimsPadding(t *testing.T) {
	dir := t.TempDir()
	// Create a padded PNG: 200x100 with 20px horizontal and 15px vertical padding.
	createTestPNGWithPadding(t, filepath.Join(dir, "logo.png"), 200, 100, 20, 20, 15, 15)

	origData, err := os.ReadFile(filepath.Join(dir, "logo.png"))
	if err != nil {
		t.Fatalf("reading original logo: %v", err)
	}

	a := &artist.Artist{Name: "Trim Test", Path: dir, LogoExists: true}
	f := NewLogoTrimFixer(nil, testLogger())

	fr, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleLogoTrimmable})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !fr.Fixed {
		t.Errorf("Fixed = false, want true; message: %s", fr.Message)
	}
	if fr.RuleID != RuleLogoTrimmable {
		t.Errorf("RuleID = %q, want %q", fr.RuleID, RuleLogoTrimmable)
	}

	// Verify the file on disk changed (trimmed should be smaller).
	trimmedData, err := os.ReadFile(filepath.Join(dir, "logo.png"))
	if err != nil {
		t.Fatalf("reading trimmed logo: %v", err)
	}
	if bytes.Equal(origData, trimmedData) {
		t.Error("logo file was not modified after trimming")
	}
}

func TestLogoTrimFixer_Fix_EmptyPath(t *testing.T) {
	a := &artist.Artist{Name: "No Path"}
	f := NewLogoTrimFixer(nil, testLogger())

	fr, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleLogoTrimmable})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if fr.Fixed {
		t.Error("Fixed = true, want false for empty path")
	}
	if fr.Message != "artist has no path" {
		t.Errorf("Message = %q, want 'artist has no path'", fr.Message)
	}
}

func TestLogoTrimFixer_Fix_NoLogoOnDisk(t *testing.T) {
	dir := t.TempDir()
	a := &artist.Artist{Name: "No Logo", Path: dir, LogoExists: true}
	f := NewLogoTrimFixer(nil, testLogger())

	fr, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleLogoTrimmable})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if fr.Fixed {
		t.Error("Fixed = true, want false when no logo on disk")
	}
	if fr.Message != "no logo file found on disk" {
		t.Errorf("Message = %q, want 'no logo file found on disk'", fr.Message)
	}
}

func TestLogoTrimFixer_Fix_CaseInsensitiveLookup(t *testing.T) {
	dir := t.TempDir()
	// Create file as Logo.PNG (mixed case) with padding.
	createTestPNGWithPadding(t, filepath.Join(dir, "Logo.PNG"), 200, 100, 20, 20, 15, 15)

	a := &artist.Artist{Name: "Case Test", Path: dir, LogoExists: true}
	f := NewLogoTrimFixer(nil, testLogger())

	fr, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleLogoTrimmable})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !fr.Fixed {
		t.Errorf("Fixed = false, want true; fixer should find Logo.PNG via case-insensitive lookup; message: %s", fr.Message)
	}
}

func TestPipeline_ManualMode_DiscoversCandidates(t *testing.T) {
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	ctx := context.Background()

	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	// Set nfo_exists rule to manual mode.
	r, err := ruleSvc.GetByID(ctx, RuleNFOExists)
	if err != nil {
		t.Fatalf("getting rule: %v", err)
	}
	r.AutomationMode = AutomationModeManual
	if err := ruleSvc.Update(ctx, r); err != nil {
		t.Fatalf("updating rule: %v", err)
	}

	a := &artist.Artist{
		Name:     "Manual Mode Test",
		SortName: "Manual Mode Test",
		Path:     t.TempDir(),
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Use a candidate-aware fixer (implements CandidateDiscoverer) so the
	// pipeline invokes it for candidate discovery in manual mode.
	fixer := &mockCandidateFixer{canFix: true}

	engine := NewEngine(ruleSvc, db, nil, testLogger())
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{fixer}, testLogger())

	result, err := pipeline.RunRule(ctx, RuleNFOExists)
	if err != nil {
		t.Fatalf("RunRule: %v", err)
	}

	if result.FixesSucceeded != 0 {
		t.Errorf("FixesSucceeded = %d, want 0 (manual mode should never auto-resolve)", result.FixesSucceeded)
	}
	if result.FixesAttempted != 1 {
		t.Errorf("FixesAttempted = %d, want 1 (should still attempt to discover candidates)", result.FixesAttempted)
	}
	if fixer.calls != 1 {
		t.Errorf("fixer.calls = %d, want 1", fixer.calls)
	}

	violations, err := ruleSvc.ListViolations(ctx, ViolationStatusPendingChoice)
	if err != nil {
		t.Fatalf("ListViolations: %v", err)
	}
	found := false
	for _, v := range violations {
		if v.ArtistID == a.ID && v.RuleID == RuleNFOExists {
			found = true
			if v.Status != ViolationStatusPendingChoice {
				t.Errorf("status = %q, want %q", v.Status, ViolationStatusPendingChoice)
			}
			if len(v.Candidates) != 1 {
				t.Errorf("Candidates len = %d, want 1", len(v.Candidates))
			}
		}
	}
	if !found {
		t.Error("expected pending_choice violation for manual-mode rule, none found")
	}

	// Verify no resolved violations exist for this rule.
	resolved, err := ruleSvc.ListViolations(ctx, ViolationStatusResolved)
	if err != nil {
		t.Fatalf("ListViolations(resolved): %v", err)
	}
	for _, v := range resolved {
		if v.ArtistID == a.ID && v.RuleID == RuleNFOExists {
			t.Error("manual-mode rule should never produce resolved violations")
		}
	}
}

func TestPipeline_RunAll_RespectsManualMode(t *testing.T) {
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	ctx := context.Background()

	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	// Set nfo_exists to manual mode.
	r, err := ruleSvc.GetByID(ctx, RuleNFOExists)
	if err != nil {
		t.Fatalf("getting rule: %v", err)
	}
	r.AutomationMode = AutomationModeManual
	if err := ruleSvc.Update(ctx, r); err != nil {
		t.Fatalf("updating rule: %v", err)
	}

	a := &artist.Artist{
		Name:     "RunAll Manual Test",
		SortName: "RunAll Manual Test",
		Path:     t.TempDir(),
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Fixer that would auto-resolve if mode were "auto", but RunAll must not
	// auto-apply it under manual mode (and it returns no candidates).
	fixer := &mockFixer{
		canFix: true,
		result: &FixResult{
			RuleID:     RuleNFOExists,
			Fixed:      true,
			Message:    "mock fixed",
			Candidates: nil,
		},
	}

	engine := NewEngine(ruleSvc, db, nil, testLogger())
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{fixer}, testLogger())

	_, err = pipeline.RunAll(ctx)
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}

	// The fixer reports Fixed=true, but RunAll must not count the manual-mode
	// rule (nfo_exists) as succeeded. Other auto-mode rules may still succeed.
	resolved, err := ruleSvc.ListViolations(ctx, ViolationStatusResolved)
	if err != nil {
		t.Fatalf("ListViolations(resolved): %v", err)
	}
	for _, v := range resolved {
		if v.ArtistID == a.ID && v.RuleID == RuleNFOExists {
			t.Error("RunAll should not auto-resolve manual-mode violations")
		}
	}

	// The manual-mode nfo_exists violation should be open (fixer returns no
	// candidates, so status is open rather than pending_choice).
	openViolations, err := ruleSvc.ListViolations(ctx, ViolationStatusOpen)
	if err != nil {
		t.Fatalf("ListViolations(open): %v", err)
	}
	found := false
	for _, v := range openViolations {
		if v.ArtistID == a.ID && v.RuleID == RuleNFOExists {
			found = true
		}
	}
	if !found {
		t.Error("expected open violation for manual-mode nfo_exists rule")
	}
}

// mockSideEffectFixer is a fixer that does NOT implement CandidateDiscoverer,
// simulating side-effect fixers like LogoTrimFixer or NFOFixer.
type mockSideEffectFixer struct {
	canFix bool
	calls  int
}

func (m *mockSideEffectFixer) CanFix(_ *Violation) bool { return m.canFix }

func (m *mockSideEffectFixer) Fix(_ context.Context, _ *artist.Artist, v *Violation) (*FixResult, error) {
	m.calls++
	return &FixResult{RuleID: v.RuleID, Fixed: true, Message: "side-effect applied"}, nil
}

// mockCandidateFixer implements CandidateDiscoverer and returns candidates.
type mockCandidateFixer struct {
	canFix bool
	calls  int
}

func (m *mockCandidateFixer) CanFix(_ *Violation) bool         { return m.canFix }
func (m *mockCandidateFixer) SupportsCandidateDiscovery() bool { return true }

func (m *mockCandidateFixer) Fix(_ context.Context, _ *artist.Artist, v *Violation) (*FixResult, error) {
	m.calls++
	return &FixResult{
		RuleID:  v.RuleID,
		Fixed:   false,
		Message: "candidates found",
		Candidates: []ImageCandidate{
			{URL: "http://example.com/c1.jpg", Width: 500, Height: 500, Source: "test", ImageType: "thumb"},
		},
	}, nil
}

func TestPipeline_ManualMode_SkipsSideEffectFixer(t *testing.T) {
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	ctx := context.Background()

	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	// Enable logo_trimmable and set it to manual mode.
	r, err := ruleSvc.GetByID(ctx, RuleLogoTrimmable)
	if err != nil {
		t.Fatalf("getting rule: %v", err)
	}
	r.Enabled = true
	r.AutomationMode = AutomationModeManual
	if err := ruleSvc.Update(ctx, r); err != nil {
		t.Fatalf("updating rule: %v", err)
	}

	dir := t.TempDir()
	// Create a padded logo so the checker flags it.
	createTestPNGWithPadding(t, filepath.Join(dir, "logo.png"), 200, 100, 20, 20, 15, 15)

	a := &artist.Artist{
		Name:       "Side Effect Test",
		SortName:   "Side Effect Test",
		Path:       dir,
		LogoExists: true,
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Register a side-effect fixer (no CandidateDiscoverer) that handles
	// logo_trimmable. It must NOT be called in manual mode.
	seFixer := &mockSideEffectFixer{canFix: true}

	engine := NewEngine(ruleSvc, db, nil, testLogger())
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{seFixer}, testLogger())

	result, err := pipeline.RunRule(ctx, RuleLogoTrimmable)
	if err != nil {
		t.Fatalf("RunRule: %v", err)
	}

	if seFixer.calls != 0 {
		t.Errorf("side-effect fixer was called %d times in manual mode; want 0", seFixer.calls)
	}

	if result.FixesAttempted != 0 {
		t.Errorf("FixesAttempted = %d; want 0 (side-effect fixer should be skipped)", result.FixesAttempted)
	}

	// The violation should be persisted as open (not pending_choice).
	openViolations, err := ruleSvc.ListViolations(ctx, ViolationStatusOpen)
	if err != nil {
		t.Fatalf("ListViolations(open): %v", err)
	}
	found := false
	for _, v := range openViolations {
		if v.ArtistID == a.ID && v.RuleID == RuleLogoTrimmable {
			found = true
			if !v.Fixable {
				t.Error("violation Fixable should be true (fixer exists, just skipped for safety)")
			}
		}
	}
	if !found {
		t.Error("expected open violation for manual-mode logo_trimmable rule")
	}
}

// TestImageFixer_Fix_DiscoveryOnly_SingleCandidate verifies that when
// DiscoveryOnly is set, a single candidate is returned as a list without
// being downloaded or saved to disk.
func TestImageFixer_Fix_DiscoveryOnly_SingleCandidate(t *testing.T) {
	mock := &mockImageProvider{
		result: &provider.FetchResult{
			Images: []provider.ImageResult{
				{URL: "http://example.com/thumb.jpg", Type: "thumb", Width: 1000, Height: 1000, Source: "fanarttv"},
			},
		},
	}

	f := NewImageFixer(mock, nil, testLogger())
	a := &artist.Artist{
		Name:          "Discovery Single",
		MusicBrainzID: "mbid-disc-single",
		Path:          t.TempDir(),
	}
	v := &Violation{
		RuleID: RuleThumbExists,
		Config: RuleConfig{DiscoveryOnly: true},
	}

	fr, err := f.Fix(context.Background(), a, v)
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if fr.Fixed {
		t.Error("Fixed = true; DiscoveryOnly should never mark as fixed")
	}
	if len(fr.Candidates) != 1 {
		t.Fatalf("Candidates len = %d, want 1", len(fr.Candidates))
	}
	if fr.Candidates[0].URL != "http://example.com/thumb.jpg" {
		t.Errorf("Candidate URL = %q, want http://example.com/thumb.jpg", fr.Candidates[0].URL)
	}
	if fr.Candidates[0].ImageType != "thumb" {
		t.Errorf("Candidate ImageType = %q, want thumb", fr.Candidates[0].ImageType)
	}
	if !strings.Contains(fr.Message, "candidate(s) for user selection") {
		t.Errorf("Message = %q; want 'candidate(s) for user selection'", fr.Message)
	}
}

// TestImageFixer_Fix_DiscoveryOnly_SelectBestCandidate verifies that
// DiscoveryOnly returns ALL candidates even when SelectBestCandidate is set,
// rather than downloading the best one.
func TestImageFixer_Fix_DiscoveryOnly_SelectBestCandidate(t *testing.T) {
	mock := &mockImageProvider{
		result: &provider.FetchResult{
			Images: []provider.ImageResult{
				{URL: "http://example.com/f1.jpg", Type: "fanart", Width: 1920, Height: 1080, Source: "fanarttv", Likes: 10},
				{URL: "http://example.com/f2.jpg", Type: "fanart", Width: 3840, Height: 2160, Source: "fanarttv", Likes: 5},
			},
		},
	}

	f := NewImageFixer(mock, nil, testLogger())
	a := &artist.Artist{
		Name:          "Discovery Best",
		MusicBrainzID: "mbid-disc-best",
		Path:          t.TempDir(),
	}
	v := &Violation{
		RuleID: RuleFanartExists,
		Config: RuleConfig{DiscoveryOnly: true, SelectBestCandidate: true},
	}

	fr, err := f.Fix(context.Background(), a, v)
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if fr.Fixed {
		t.Error("Fixed = true; DiscoveryOnly should never mark as fixed")
	}
	if len(fr.Candidates) != 2 {
		t.Fatalf("Candidates len = %d, want 2 (all candidates returned despite SelectBestCandidate)", len(fr.Candidates))
	}
}

// TestPipeline_ManualMode_SetsDiscoveryOnly verifies that the pipeline sets
// DiscoveryOnly on the violation config before calling attemptFix in manual
// mode, ensuring ImageFixer returns candidates without side effects.
func TestPipeline_ManualMode_SetsDiscoveryOnly(t *testing.T) {
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	ctx := context.Background()

	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	// Set thumb_exists to manual mode.
	r, err := ruleSvc.GetByID(ctx, RuleThumbExists)
	if err != nil {
		t.Fatalf("getting rule: %v", err)
	}
	r.AutomationMode = AutomationModeManual
	if err := ruleSvc.Update(ctx, r); err != nil {
		t.Fatalf("updating rule: %v", err)
	}

	a := &artist.Artist{
		Name:          "DiscoveryOnly Pipeline",
		SortName:      "DiscoveryOnly Pipeline",
		MusicBrainzID: "mbid-disc-pipe",
		Path:          t.TempDir(),
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// discoveryCaptureFixer records whether DiscoveryOnly was set when Fix was called.
	captureFixer := &discoveryCaptureFixer{}

	engine := NewEngine(ruleSvc, db, nil, testLogger())
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{captureFixer}, testLogger())

	_, err = pipeline.RunRule(ctx, RuleThumbExists)
	if err != nil {
		t.Fatalf("RunRule: %v", err)
	}

	if captureFixer.calls == 0 {
		t.Fatal("fixer was never called")
	}
	if !captureFixer.sawDiscoveryOnly {
		t.Error("pipeline did not set DiscoveryOnly before calling Fix in manual mode")
	}
}

// discoveryCaptureFixer records whether DiscoveryOnly was set on the violation.
type discoveryCaptureFixer struct {
	calls            int
	sawDiscoveryOnly bool
}

func (f *discoveryCaptureFixer) CanFix(v *Violation) bool {
	return v.RuleID == RuleThumbExists
}

func (f *discoveryCaptureFixer) SupportsCandidateDiscovery() bool { return true }

func (f *discoveryCaptureFixer) Fix(_ context.Context, _ *artist.Artist, v *Violation) (*FixResult, error) {
	f.calls++
	f.sawDiscoveryOnly = v.Config.DiscoveryOnly
	return &FixResult{
		RuleID:  v.RuleID,
		Fixed:   false,
		Message: "discovery capture",
		Candidates: []ImageCandidate{
			{URL: "http://example.com/c.jpg", Width: 500, Height: 500, Source: "test", ImageType: "thumb"},
		},
	}, nil
}

// TestPipeline_ManualMode_FixableGuard_NoFixer verifies that when no fixer is
// registered for a rule in manual mode, the persisted violation has Fixable=false.
func TestPipeline_ManualMode_FixableGuard_NoFixer(t *testing.T) {
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	ctx := context.Background()

	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	// Set nfo_exists to manual mode.
	r, err := ruleSvc.GetByID(ctx, RuleNFOExists)
	if err != nil {
		t.Fatalf("getting rule: %v", err)
	}
	r.AutomationMode = AutomationModeManual
	if err := ruleSvc.Update(ctx, r); err != nil {
		t.Fatalf("updating rule: %v", err)
	}

	a := &artist.Artist{
		Name:     "No Fixer Manual",
		SortName: "No Fixer Manual",
		Path:     t.TempDir(),
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// No fixers registered at all.
	engine := NewEngine(ruleSvc, db, nil, testLogger())
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, nil, testLogger())

	_, err = pipeline.RunRule(ctx, RuleNFOExists)
	if err != nil {
		t.Fatalf("RunRule: %v", err)
	}

	openViolations, err := ruleSvc.ListViolations(ctx, ViolationStatusOpen)
	if err != nil {
		t.Fatalf("ListViolations: %v", err)
	}
	found := false
	for _, v := range openViolations {
		if v.ArtistID == a.ID && v.RuleID == RuleNFOExists {
			found = true
			if v.Fixable {
				t.Error("Fixable = true; want false when no fixer is registered")
			}
		}
	}
	if !found {
		t.Error("expected open violation for manual-mode nfo_exists, none found")
	}
}
