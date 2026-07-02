package api

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/event"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/publish"
	"github.com/sydlexius/stillwater/internal/rule"
)

// TestFinalizeImageSave_PreservesPerCallSitePublishOrder is a regression test
// for #1552: finalizeImageSave consolidates four previously-duplicated
// post-save tails, but MUST NOT homogenize their ArtistUpdated
// publish-vs-rule-reevaluation ordering. Three of the four tails publish the
// event before rule reevaluation; handleImageFetch's fanart-append branch
// publishes after. If a future edit collapses opts.publishOrder to a single
// hardcoded order, this test fails.
//
// Ordering is measured with real wall-clock timestamps taken synchronously
// at each call site (event.Bus stamps Event.Timestamp inside Publish() at
// the exact call instant; the rule-pipeline stub records time.Now() inside
// RunForArtist, itself invoked synchronously from runRulesAfterRefresh).
// Both calls happen in the same goroutine as the HTTP handler with real
// work (cache invalidation, DB writes) between them, so the timestamps are
// never close enough to tie.
func TestFinalizeImageSave_PreservesPerCallSitePublishOrder(t *testing.T) {
	t.Run("handleImageUpload primary path publishes before rule reevaluation", func(t *testing.T) {
		t.Parallel()
		ruleTime, publishTime, ready := instrumentedFinalizeOrderRig(t)

		r, artistSvc := testRouterWithStubPipeline(t, ready.stub)
		r.eventBus = ready.bus
		platSvc := platform.NewService(r.db)
		r.platformService = platSvc
		r.publisher = publish.New(publish.Deps{
			ArtistService:      r.artistService,
			ConnectionService:  r.connectionService,
			NFOSnapshotService: r.nfoSnapshotService,
			PlatformService:    platSvc,
			ImageCacheDir:      r.imageCacheDir,
			Logger:             r.logger,
		})

		dir := t.TempDir()
		a := &artist.Artist{Name: "Order Upload Primary", SortName: "Order Upload Primary", Path: dir}
		if err := artistSvc.Create(context.Background(), a); err != nil {
			t.Fatalf("creating artist: %v", err)
		}

		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		if err := mw.WriteField("type", "thumb"); err != nil {
			t.Fatalf("writing field: %v", err)
		}
		partHeader := make(map[string][]string)
		partHeader["Content-Disposition"] = []string{`form-data; name="file"; filename="thumb.jpg"`}
		partHeader["Content-Type"] = []string{"image/jpeg"}
		fw, err := mw.CreatePart(partHeader)
		if err != nil {
			t.Fatalf("CreatePart: %v", err)
		}
		if err := jpeg.Encode(fw, image.NewRGBA(image.Rect(0, 0, 500, 500)), nil); err != nil {
			t.Fatalf("encoding JPEG: %v", err)
		}
		if err := mw.Close(); err != nil {
			t.Fatalf("closing multipart writer: %v", err)
		}

		req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/upload?skip_crop=true", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.SetPathValue("id", a.ID)
		w := httptest.NewRecorder()

		r.handleImageUpload(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
		}

		rt := waitForFinalizeOrderTime(t, ruleTime, "rule reevaluation")
		pt := waitForFinalizeOrderTime(t, publishTime, "ArtistUpdated publish")

		if !pt.Before(rt) {
			t.Errorf("expected ArtistUpdated to publish BEFORE rule reevaluation (publishBeforeRuleEval), got publish=%s rules=%s",
				pt.Format(time.RFC3339Nano), rt.Format(time.RFC3339Nano))
		}
	})

	t.Run("handleImageFetch fanart-append branch publishes after rule reevaluation", func(t *testing.T) {
		t.Parallel()
		ruleTime, publishTime, ready := instrumentedFinalizeOrderRig(t)

		r, artistSvc := testRouterWithStubPipeline(t, ready.stub)
		r.eventBus = ready.bus
		platSvc := platform.NewService(r.db)
		r.platformService = platSvc
		r.publisher = publish.New(publish.Deps{
			ArtistService:      r.artistService,
			ConnectionService:  r.connectionService,
			NFOSnapshotService: r.nfoSnapshotService,
			PlatformService:    platSvc,
			ImageCacheDir:      r.imageCacheDir,
			Logger:             r.logger,
		})

		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1920, 1080)), nil); err != nil {
			t.Fatalf("encoding JPEG: %v", err)
		}
		r.ssrfClient = &http.Client{Transport: &stubRoundTripper{body: buf.Bytes()}}

		dir := t.TempDir()
		a := &artist.Artist{Name: "Order Fetch Fanart Append", SortName: "Order Fetch Fanart Append", Path: dir, FanartExists: true}
		if err := artistSvc.Create(context.Background(), a); err != nil {
			t.Fatalf("creating artist: %v", err)
		}
		// Seed a primary fanart so the append branch produces fanart2.jpg.
		writeJPEG(t, filepath.Join(dir, "fanart.jpg"), 1920, 1080)

		// Public IP literal so isPrivateURL's DNS lookup is skipped (see
		// TestHandleImageFetch_RerunsRulesAfterWrite for rationale).
		body := strings.NewReader(`{"url":"https://8.8.8.8/test.jpg","type":"fanart"}`)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fetch?skip_crop=true", body)
		req.Header.Set("Content-Type", "application/json")
		req.SetPathValue("id", a.ID)
		w := httptest.NewRecorder()

		r.handleImageFetch(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
		}

		rt := waitForFinalizeOrderTime(t, ruleTime, "rule reevaluation")
		pt := waitForFinalizeOrderTime(t, publishTime, "ArtistUpdated publish")

		// This is the one call site (of four) that must NOT match the
		// "before" ordering asserted above -- see imagePublishOrder.
		if !rt.Before(pt) {
			t.Errorf("expected ArtistUpdated to publish AFTER rule reevaluation (publishAfterRuleEval) for the fetch fanart-append branch, got rules=%s publish=%s -- if this now publishes BEFORE, finalizeImageSave has silently homogenized the per-call-site publish ordering",
				rt.Format(time.RFC3339Nano), pt.Format(time.RFC3339Nano))
		}
	})
}

// finalizeOrderRig bundles the instrumented rule-pipeline stub and event bus
// used to observe finalizeImageSave's publish-vs-rules call order.
type finalizeOrderRig struct {
	stub *stubPipeline
	bus  *event.Bus
}

// instrumentedFinalizeOrderRig wires a stubPipeline whose RunForArtist
// records its call time and an event.Bus whose ArtistUpdated subscriber
// records the published event's Timestamp (stamped by Bus.Publish at the
// exact call instant). Both channels are buffered so the synchronous
// producer side never blocks; the test drains them after the HTTP handler
// returns.
func instrumentedFinalizeOrderRig(t *testing.T) (ruleTime, publishTime chan time.Time, rig finalizeOrderRig) {
	t.Helper()
	ruleTime = make(chan time.Time, 1)
	publishTime = make(chan time.Time, 1)

	stub := &stubPipeline{
		runForArtistFn: func(_ context.Context, _ *artist.Artist) (*rule.RunResult, error) {
			select {
			case ruleTime <- time.Now():
			default:
			}
			return &rule.RunResult{}, nil
		},
	}

	bus := event.NewBus(slog.New(slog.NewTextHandler(io.Discard, nil)), 16)
	bus.Subscribe(event.ArtistUpdated, func(e event.Event) {
		select {
		case publishTime <- e.Timestamp:
		default:
		}
	})
	go bus.Start()
	t.Cleanup(bus.Stop)

	return ruleTime, publishTime, finalizeOrderRig{stub: stub, bus: bus}
}

func waitForFinalizeOrderTime(t *testing.T, ch chan time.Time, what string) time.Time {
	t.Helper()
	select {
	case ts := <-ch:
		return ts
	case <-time.After(2 * time.Second):
		t.Fatalf("%s was not observed within 2s", what)
		return time.Time{}
	}
}
