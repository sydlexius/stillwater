//go:build integration

package publish

import (
	"bytes"
	"context"
	"fmt"
	stdimage "image"
	"image/color"
	"image/jpeg"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	xdraw "golang.org/x/image/draw"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/encryption"
	img "github.com/sydlexius/stillwater/internal/image"
)

// #3138 S0: the live before/after measurement of the PERCEPTUAL platform prune.
//
// The issue's open acceptance criterion: "Verified against a real Emby AND a
// real Jellyfin, with backdrop counts measured before and after. A fake client
// that records 'delete was called' passes against both broken and correct
// code." and "Re-running the prune over its own output deletes nothing."
//
// Every count below is read back FROM THE PEER, and every slot is identified by
// the sha256 of the bytes the peer serves, never by what the test uploaded.
//
// Entry point under test: PrunePlatformBackdropsForArtist, the one the "No
// duplicate images" rule calls. Nothing here calls the client's delete except
// the clear before and after the run.

// s0TestTimeout is a whole-test budget: one scenario is roughly sixty round
// trips (seed, three full read-backs, detection, per-delete re-verifies).
const s0TestTimeout = 3 * time.Minute

// s0Picture renders a smooth, structured 640x360 picture from seed: three
// plane waves per channel. Unlike a noise field it survives re-encoding and
// resampling with its perceptual hash nearly intact, which is what a
// near-duplicate fixture needs; distinct seeds give distinct hashes (asserted
// against the peer's bytes in s0AssertFixture, not assumed here).
func s0Picture(seed int) *stdimage.RGBA {
	const w, h = 640, 360
	state := uint32(seed)*2654435761 + 12345
	next := func() float64 {
		state = state*1664525 + 1013904223
		return float64(state>>8) / float64(1<<24)
	}
	type wave struct{ fx, fy, ph, amp float64 }
	var waves [3][3]wave
	for c := range waves {
		for k := range waves[c] {
			waves[c][k] = wave{fx: 0.5 + 3*next(), fy: 0.5 + 2*next(), ph: 2 * math.Pi * next(), amp: 25 + 20*next()}
		}
	}
	out := stdimage.NewRGBA(stdimage.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var px [3]uint8
			for c := range waves {
				v := 128.0
				for _, wv := range waves[c] {
					v += wv.amp * math.Sin(2*math.Pi*(wv.fx*float64(x)/w+wv.fy*float64(y)/h)+wv.ph)
				}
				px[c] = uint8(math.Max(0, math.Min(255, v)))
			}
			out.Set(x, y, color.RGBA{R: px[0], G: px[1], B: px[2], A: 255})
		}
	}
	return out
}

func s0JPEG(t *testing.T, src stdimage.Image, quality int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, src, &jpeg.Options{Quality: quality}); err != nil {
		t.Fatalf("encoding fixture jpeg: %v", err)
	}
	return buf.Bytes()
}

// s0Downscale resamples src to 480x270: the "same picture at a lower
// resolution" near-duplicate.
func s0Downscale(src stdimage.Image) *stdimage.RGBA {
	dst := stdimage.NewRGBA(stdimage.Rect(0, 0, 480, 270))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), xdraw.Src, nil)
	return dst
}

// s0Slot is one fixture image with the name it is reported under.
type s0Slot struct {
	name string
	data []byte
}

// s0Fixture is the known backdrop set.
type s0Fixture struct {
	p1, p1q60, p1small, p2, p3 s0Slot
}

func newS0Fixture(t *testing.T) s0Fixture {
	t.Helper()
	pic1 := s0Picture(101)
	return s0Fixture{
		p1:      s0Slot{"P1", s0JPEG(t, pic1, 92)},
		p1q60:   s0Slot{"P1-q60", s0JPEG(t, pic1, 60)},
		p1small: s0Slot{"P1-480x270", s0JPEG(t, s0Downscale(pic1), 85)},
		p2:      s0Slot{"P2", s0JPEG(t, s0Picture(202), 92)},
		p3:      s0Slot{"P3", s0JPEG(t, s0Picture(303), 92)},
	}
}

func (f s0Fixture) all() []s0Slot { return []s0Slot{f.p1, f.p1q60, f.p1small, f.p2, f.p3} }

// s0Peer is what the measurement needs from a peer besides the prune itself.
type s0Peer interface {
	backdropClearer
	GetArtistBackdrop(ctx context.Context, platformArtistID string, index int) ([]byte, string, error)
}

// s0Target is one peer + scratch item, and how to append a backdrop to it.
type s0Target struct {
	connType, url, apiKey, userID, itemID string
	client                                s0Peer
	// appendBackdrop adds one backdrop at the END of the item's list. position
	// is the slot it is expected to land in.
	appendBackdrop func(ctx context.Context, position int, data []byte) error
}

// s0Read reads the item's backdrop list back from the peer and names every
// slot by content. A slot whose bytes match no fixture image fails the test:
// it means the peer re-encoded an upload, and then nothing below can say which
// picture a slot holds.
func s0Read(ctx context.Context, t *testing.T, tg s0Target, f s0Fixture, step string, wantCount int) (names []string, raw [][]byte) {
	t.Helper()
	st := pollBackdropDetail(ctx, t, tg.client, tg.itemID, wantCount)
	known := map[string]string{}
	for _, s := range f.all() {
		known[hashOf(s.data)] = s.name
	}
	for i := 0; i < st.BackdropCount; i++ {
		data, _, err := tg.client.GetArtistBackdrop(ctx, tg.itemID, i)
		if err != nil {
			t.Fatalf("%s: reading backdrop %d: %v", step, i, err)
		}
		name, ok := known[hashOf(data)]
		if !ok {
			t.Fatalf("%s: backdrop %d holds %d bytes that match NO uploaded image; the peer did not store the upload byte-for-byte, so slots cannot be identified", step, i, len(data))
		}
		names = append(names, name)
		raw = append(raw, data)
	}
	t.Logf("COUNT %-22s = %d   slots=%v", step, st.BackdropCount, names)
	return names, raw
}

func s0Names(slots []s0Slot) []string {
	out := make([]string, len(slots))
	for i, s := range slots {
		out[i] = s.name
	}
	return out
}

func s0Phash(t *testing.T, data []byte) uint64 {
	t.Helper()
	h, err := img.PerceptualHash(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("perceptual hash: %v", err)
	}
	return h
}

// s0AssertFixture asserts the fixture's DEFINING properties on the bytes the
// PEER serves: which slots are near-duplicates (same picture, different bytes,
// similarity at or above the default tolerance) and which are distinct
// (below it). Without this a passing run could mean the fixture held nothing
// perceptual to prune.
func s0AssertFixture(t *testing.T, names []string, raw [][]byte) {
	t.Helper()
	picture := func(name string) string { return strings.SplitN(name, "-", 2)[0] }
	tol := img.DefaultDuplicateTolerance
	for i := range raw {
		for j := i + 1; j < len(raw); j++ {
			sim := img.Similarity(s0Phash(t, raw[i]), s0Phash(t, raw[j]))
			same := picture(names[i]) == picture(names[j])
			identical := bytes.Equal(raw[i], raw[j])
			t.Logf("fixture: slot %d %-10s vs slot %d %-10s similarity=%.4f byte-identical=%v", i, names[i], j, names[j], sim, identical)
			switch {
			case same && names[i] != names[j] && identical:
				t.Fatalf("fixture: %s and %s are byte-identical on the peer; the near-duplicate is not a near-duplicate", names[i], names[j])
			case same && sim < tol:
				t.Fatalf("fixture: %s and %s are the same picture but similarity %.4f is below tolerance %.2f; nothing perceptual to prune", names[i], names[j], sim, tol)
			case !same && sim >= tol:
				t.Fatalf("fixture: %s and %s are meant to be DISTINCT but similarity %.4f is at or above tolerance %.2f", names[i], names[j], sim, tol)
			}
		}
	}
}

// s0Publisher wires a Publisher the way cmd/stillwater does for the pieces the
// perceptual prune reads: real SQLite artist and connection services, the
// artist-image reader (durabilityPublisher leaves it nil, which turns the
// perceptual tier off), one image-write connection, one artist whose folder
// holds local.
func s0Publisher(t *testing.T, tg s0Target, local []s0Slot) (*Publisher, *artist.Artist) {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(filepath.Join(t.TempDir(), "sw.db"))
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := database.Migrate(db); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("encryptor: %v", err)
	}
	artistSvc := artist.NewService(db)
	connSvc := connection.NewService(db, enc)

	conn := &connection.Connection{Name: "s0-peer", Type: tg.connType, URL: tg.url, APIKey: tg.apiKey, Enabled: true, Status: "ok"}
	if tg.connType == connection.TypeEmby {
		conn.Emby = &connection.EmbyConfig{PlatformUserID: tg.userID, FeatureImageWrite: true}
	} else {
		conn.Jellyfin = &connection.JellyfinConfig{PlatformUserID: tg.userID, FeatureImageWrite: true}
	}
	if err := connSvc.Create(ctx, conn); err != nil {
		t.Fatalf("creating connection: %v", err)
	}

	dir := t.TempDir()
	for i, s := range local {
		name := "fanart.jpg"
		if i > 0 {
			name = fmt.Sprintf("fanart%d.jpg", i+1)
		}
		if err := os.WriteFile(filepath.Join(dir, name), s.data, 0o600); err != nil {
			t.Fatalf("writing local fanart: %v", err)
		}
	}
	a := &artist.Artist{Name: "S0 Scratch Artist", SortName: "S0 Scratch Artist", Path: dir}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if err := artistSvc.SetPlatformID(ctx, a.ID, conn.ID, tg.itemID); err != nil {
		t.Fatalf("mapping artist: %v", err)
	}
	p := New(Deps{
		Logger:            silentLogger(),
		ArtistService:     artistSvc,
		ArtistLister:      artistSvc,
		ArtistGetter:      artistSvc,
		ArtistImages:      artistSvc,
		ConnectionService: connSvc,
	})
	return p, a
}

// s0PlanRow is a plan entry reduced to what the measurement compares.
type s0PlanRow struct {
	Index, Survivor int
	Tier, Outcome   string
}

func s0Plan(res PlatformBackdropPruneResult) []s0PlanRow {
	out := make([]s0PlanRow, 0, len(res.Plan))
	for _, e := range res.Plan {
		out = append(out, s0PlanRow{e.Index, e.Survivor, e.Tier, e.Outcome})
	}
	return out
}

func s0SamePlan(a, b []s0PlanRow) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func s0SameNames(a, b []string) bool { return strings.Join(a, "|") == strings.Join(b, "|") }

// s0Seed clears the item, appends seed in order, and asserts the peer holds
// exactly that list.
func s0Seed(ctx context.Context, t *testing.T, tg s0Target, f s0Fixture, seed []s0Slot) {
	t.Helper()
	clearAllBackdrops(ctx, t, tg.itemID, tg.client)
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), liveBackdropCleanupTimeout)
		defer cancel()
		clearAllBackdrops(cctx, t, tg.itemID, tg.client)
	})
	for i, s := range seed {
		if err := tg.appendBackdrop(ctx, i, s.data); err != nil {
			t.Fatalf("seeding backdrop %d (%s): %v", i, s.name, err)
		}
	}
	t.Logf("UPLOADED %d backdrops: %v", len(seed), s0Names(seed))
	names, raw := s0Read(ctx, t, tg, f, "before (seeded)", len(seed))
	if !s0SameNames(names, s0Names(seed)) {
		t.Fatalf("precondition: peer holds %d backdrops %v after seeding, want the %d uploaded %v", len(names), names, len(seed), s0Names(seed))
	}
	s0AssertFixture(t, names, raw)
}

func s0Run(ctx context.Context, t *testing.T, p *Publisher, a *artist.Artist, dry bool) PlatformBackdropPruneResult {
	t.Helper()
	res, err := p.PrunePlatformBackdropsForArtist(ctx, a, ArtistBackdropPruneOptions{
		Perceptual: true, DryRun: dry, Tolerance: img.DefaultDuplicateTolerance,
	})
	if err != nil {
		t.Fatalf("prune (dry=%v): %v", dry, err)
	}
	t.Logf("RUN dry=%v tolerance=%.2f: removed=%d skippedChanged=%d failures=%d skipped=%v plan=%+v",
		dry, img.DefaultDuplicateTolerance, res.BackdropsRemoved, res.SkippedChanged, len(res.Failures), res.Skipped, s0Plan(res))
	if len(res.Failures) != 0 {
		t.Fatalf("prune (dry=%v) reported failures: %+v", dry, res.Failures)
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("prune (dry=%v): perceptual tier was skipped by policy, so this run measured the exact tier only: %+v", dry, res.Skipped)
	}
	if res.DryRun != dry {
		t.Fatalf("result.DryRun = %v, want %v", res.DryRun, dry)
	}
	return res
}

// s0Scenario is one fixture for measurePerceptualPrune: what is seeded on the
// peer, what the artist's local folder holds, and the plan and final list a
// correct prune produces.
type s0Scenario struct {
	seed, local []s0Slot
	wantPlan    []s0PlanRow
	wantAfter   []string
}

// s0DuplicatesScenario puts the survivor (slot 4, the copy byte-identical to
// the local file) ABOVE both of its near-duplicates (slots 0 and 2), so the
// live run only completes if the peer renumbers after a delete the way
// shiftAfterDelete relies on. This is the measurement its comment cites: it
// passed on Emby 4.10.1.0, Emby 4.11.0.5 (beta) and Jellyfin 10.11.10
// (2026-10-03), for backdrops uploaded through the image API. Every library
// measured had saveLocalMetadata off and no metadata savers; the
// saveLocalMetadata-on case and library-bound backdrops are NOT measured.
func s0DuplicatesScenario(f s0Fixture) s0Scenario {
	return s0Scenario{
		seed:  []s0Slot{f.p1small, f.p2, f.p1q60, f.p2, f.p1, f.p3},
		local: []s0Slot{f.p1, f.p2, f.p3},
		wantPlan: []s0PlanRow{
			{Index: 3, Survivor: 1, Tier: PruneTierExact},
			{Index: 2, Survivor: 4, Tier: PruneTierPerceptual},
			{Index: 0, Survivor: 4, Tier: PruneTierPerceptual},
		},
		wantAfter: []string{"P2", "P1", "P3"},
	}
}

// s0TwinIsSmallerScenario isolates the FIRST survivor rule. In the scenario
// above the local twin is also the largest copy, so "keep the local twin" and
// "keep the biggest" name the same slot. Here they disagree: the local file is
// the 480x270 downscale, and the full-size P1 exists only on the peer, in the
// LOWER slot. The local twin must survive and the larger copy must go. P3 is
// distinct and platform-only, and must be left alone.
func s0TwinIsSmallerScenario(f s0Fixture) s0Scenario {
	return s0Scenario{
		seed:      []s0Slot{f.p1, f.p1small, f.p3},
		local:     []s0Slot{f.p1small},
		wantPlan:  []s0PlanRow{{Index: 0, Survivor: 1, Tier: PruneTierPerceptual}},
		wantAfter: []string{"P1-480x270", "P3"},
	}
}

// measurePerceptualPrune is steps 1-4: fixture, dry run, live run, re-run.
func measurePerceptualPrune(t *testing.T, tg s0Target, scenario func(s0Fixture) s0Scenario) {
	ctx, cancel := context.WithTimeout(context.Background(), s0TestTimeout)
	defer cancel()
	f := newS0Fixture(t)
	sc := scenario(f)
	seed, local, wantPlan, wantAfter := sc.seed, sc.local, sc.wantPlan, sc.wantAfter
	withOutcome := func(o string) []s0PlanRow {
		out := append([]s0PlanRow(nil), wantPlan...)
		for i := range out {
			out[i].Outcome = o
		}
		return out
	}

	// 1. Fixture, read back and asserted before anything else.
	s0Seed(ctx, t, tg, f, seed)
	p, a := s0Publisher(t, tg, local)

	// 2. Dry run: the plan, and an untouched peer.
	dry := s0Run(ctx, t, p, a, true)
	if dry.BackdropsRemoved != 0 {
		t.Fatalf("dry run reports %d removed", dry.BackdropsRemoved)
	}
	if got := s0Plan(dry); !s0SamePlan(got, withOutcome(PrunePlanPlanned)) {
		t.Fatalf("dry-run plan = %+v, want %+v", got, withOutcome(PrunePlanPlanned))
	}
	afterDry, _ := s0Read(ctx, t, tg, f, "after dry run", len(seed))
	if !s0SameNames(afterDry, s0Names(seed)) {
		t.Fatalf("DRY RUN CHANGED THE PEER: %v, want the seeded %v", afterDry, s0Names(seed))
	}

	// 3. Live run, same entry point, default tolerance.
	live := s0Run(ctx, t, p, a, false)
	if got := s0Plan(live); !s0SamePlan(got, withOutcome(PrunePlanDeleted)) {
		t.Errorf("live plan = %+v, want the dry run's plan with every entry deleted: %+v", got, withOutcome(PrunePlanDeleted))
	}
	if live.BackdropsRemoved != len(wantPlan) || live.SkippedChanged != 0 {
		t.Errorf("live run: removed=%d skippedChanged=%d, want %d and 0", live.BackdropsRemoved, live.SkippedChanged, len(wantPlan))
	}
	afterLive, _ := s0Read(ctx, t, tg, f, "after live run", len(wantAfter))
	// What the DRY RUN promised, derived from its plan rather than restated:
	// the seeded list minus the planned indices.
	doomed := map[int]bool{}
	for _, e := range dry.Plan {
		doomed[e.Index] = true
	}
	var promised []string
	for i, s := range seed {
		if !doomed[i] {
			promised = append(promised, s.name)
		}
	}
	if !s0SameNames(afterLive, promised) {
		t.Errorf("after the live run the peer holds %v; the dry run's plan promised %v", afterLive, promised)
	}
	if !s0SameNames(afterLive, wantAfter) {
		t.Fatalf("after the live run the peer holds %v, want exactly one copy of each picture, %v", afterLive, wantAfter)
	}

	// 4. Idempotence: the prune over its own output.
	again := s0Run(ctx, t, p, a, false)
	if again.BackdropsRemoved != 0 || len(again.Plan) != 0 {
		t.Errorf("re-run: removed=%d plan=%+v, want nothing", again.BackdropsRemoved, s0Plan(again))
	}
	afterAgain, _ := s0Read(ctx, t, tg, f, "after re-run", len(wantAfter))
	if !s0SameNames(afterAgain, wantAfter) {
		t.Fatalf("re-run changed the peer: %v, want %v", afterAgain, wantAfter)
	}
	t.Logf("SUMMARY %s item %s: uploaded=%d before=%d afterDryRun=%d afterLiveRun=%d afterReRun=%d",
		tg.connType, tg.itemID, len(seed), len(seed), len(afterDry), len(afterLive), len(afterAgain))
}

// measurePerceptualPruneNegativeControl is step 5: three DISTINCT pictures,
// the same entry point, and nothing may be deleted.
//
// The local folder holds P1 ONLY, so P2 and P3 exist on the peer and nowhere
// else. That is what makes this a control on the similarity judgement: the
// prune never deletes a copy byte-identical to a local file, so with all three
// pictures local it would pass at any tolerance. A platform-only backdrop has
// no such protection; it survives here only because it does not match.
func measurePerceptualPruneNegativeControl(t *testing.T, tg s0Target) {
	ctx, cancel := context.WithTimeout(context.Background(), s0TestTimeout)
	defer cancel()
	f := newS0Fixture(t)
	seed := []s0Slot{f.p1, f.p2, f.p3}
	s0Seed(ctx, t, tg, f, seed)
	p, a := s0Publisher(t, tg, seed[:1])

	dry := s0Run(ctx, t, p, a, true)
	live := s0Run(ctx, t, p, a, false)
	if len(dry.Plan) != 0 || len(live.Plan) != 0 || live.BackdropsRemoved != 0 {
		t.Errorf("distinct-only item: dry plan=%+v live plan=%+v removed=%d, want nothing", s0Plan(dry), s0Plan(live), live.BackdropsRemoved)
	}
	after, _ := s0Read(ctx, t, tg, f, "after live run", len(seed))
	if !s0SameNames(after, s0Names(seed)) {
		t.Fatalf("NEGATIVE CONTROL FAILED: a distinct platform-only picture was pruned; peer holds %v, want %v", after, s0Names(seed))
	}
	t.Logf("SUMMARY %s item %s (negative control): uploaded=%d before=%d afterLiveRun=%d", tg.connType, tg.itemID, len(seed), len(seed), len(after))
}

// s0LogServerVersion records which server build the numbers came from.
func s0LogServerVersion(t *testing.T, url string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, strings.TrimRight(url, "/")+"/System/Info/Public", nil)
	if err != nil {
		t.Logf("SERVER public info: %v", err)
		return
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Logf("SERVER public info: %v", err)
		return
	}
	defer resp.Body.Close() //nolint:errcheck // nothing to do about a close error on a diagnostic read
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	t.Logf("SERVER %s public info: %s", url, body)
}

func s0LiveEmby(t *testing.T) s0Target {
	env := loadLiveEmbyEnv(t)
	s0LogServerVersion(t, env.url)
	c := emby.New(env.url, env.apiKey, env.userID, silentLogger())
	return s0Target{connection.TypeEmby, env.url, env.apiKey, env.userID, env.itemID, c,
		// Non-indexed upload: appends on both peers (an indexed POST to an
		// occupied slot REPLACES on Emby). The count is asserted regardless.
		func(ctx context.Context, _ int, b []byte) error {
			return c.UploadImage(ctx, env.itemID, "fanart", b, "image/jpeg")
		}}
}

func s0LiveJellyfin(t *testing.T) s0Target {
	// Same dedicated-item convention as liveJellyfinPrune: the internal/api live
	// suite also rewrites SW_LIVE_JELLYFIN_ITEM_ID.
	if id := os.Getenv("SW_LIVE_JELLYFIN_PRUNE_ITEM_ID"); id != "" {
		t.Setenv("SW_LIVE_JELLYFIN_ITEM_ID", id)
	}
	env := loadLiveJellyfinEnv(t)
	s0LogServerVersion(t, env.url)
	c := jellyfin.New(env.url, env.apiKey, env.userID, silentLogger())
	return s0Target{connection.TypeJellyfin, env.url, env.apiKey, env.userID, env.itemID, c,
		func(ctx context.Context, _ int, b []byte) error {
			return c.UploadImage(ctx, env.itemID, "fanart", b, "image/jpeg")
		}}
}

func TestLivePerceptualPrune_Emby(t *testing.T) {
	measurePerceptualPrune(t, s0LiveEmby(t), s0DuplicatesScenario)
}

func TestLivePerceptualPrune_Jellyfin(t *testing.T) {
	measurePerceptualPrune(t, s0LiveJellyfin(t), s0DuplicatesScenario)
}

func TestLivePerceptualPruneKeepsSmallerLocalTwin_Emby(t *testing.T) {
	measurePerceptualPrune(t, s0LiveEmby(t), s0TwinIsSmallerScenario)
}

func TestLivePerceptualPruneKeepsSmallerLocalTwin_Jellyfin(t *testing.T) {
	measurePerceptualPrune(t, s0LiveJellyfin(t), s0TwinIsSmallerScenario)
}

func TestLivePerceptualPruneDistinctOnlyDeletesNothing_Emby(t *testing.T) {
	measurePerceptualPruneNegativeControl(t, s0LiveEmby(t))
}

func TestLivePerceptualPruneDistinctOnlyDeletesNothing_Jellyfin(t *testing.T) {
	measurePerceptualPruneNegativeControl(t, s0LiveJellyfin(t))
}

// TestPerceptualPruneMeasurement_HarnessSelfCheck runs the SAME measurement
// bodies against statefulBackdropPeer, the in-process model of a peer. It is
// NOT the #3138 measurement and proves nothing about a real server: it exists
// so the harness (fixture similarities, expected plan, expected survivors) is
// known to agree with the production code before it is pointed at one, and so
// a red live run can be read as a statement about the peer.
func TestPerceptualPruneMeasurement_HarnessSelfCheck(t *testing.T) {
	for _, connType := range []string{connection.TypeEmby, connection.TypeJellyfin} {
		modelled := func(t *testing.T) s0Target {
			peer := &statefulBackdropPeer{appendAll: connType == connection.TypeJellyfin}
			srv := httptest.NewServer(peer)
			t.Cleanup(srv.Close)
			var c interface {
				s0Peer
				UploadImageAtIndex(ctx context.Context, id, imageType string, index int, data []byte, contentType string) error
			}
			if connType == connection.TypeEmby {
				c = emby.New(srv.URL, "k", "u1", silentLogger())
			} else {
				c = jellyfin.New(srv.URL, "k", "u1", silentLogger())
			}
			return s0Target{connType, srv.URL, "k", "u1", "p1", c,
				// The model serves only the indexed image route.
				func(ctx context.Context, position int, b []byte) error {
					return c.UploadImageAtIndex(ctx, "p1", "fanart", position, b, "image/jpeg")
				}}
		}
		t.Run(connType+"/duplicates", func(t *testing.T) { measurePerceptualPrune(t, modelled(t), s0DuplicatesScenario) })
		t.Run(connType+"/twin-is-smaller", func(t *testing.T) { measurePerceptualPrune(t, modelled(t), s0TwinIsSmallerScenario) })
		t.Run(connType+"/distinct-only", func(t *testing.T) { measurePerceptualPruneNegativeControl(t, modelled(t)) })
	}
}
