package api

import (
	"context"
	"log/slog"
	"sync"
	"time"

	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/provider"
)

// Web-search cull bounds (#1833). The probes hit the third-party IMAGE hosts,
// never the search engine, so they cannot count against the search provider's
// rate limits. They are vars (not consts) only so tests can shrink them.
var (
	// cullMaxConcurrent caps simultaneous probes. Results usually span many
	// hosts, so this is a bit above probeImageDimensions' 5 without
	// hammering any single host.
	cullMaxConcurrent = 8
	// cullProbeTimeout bounds one probe. A host slower than this is treated
	// as broken: a result that takes >4s to even return its header would
	// also make the panel's <img> tile look dead.
	cullProbeTimeout = 4 * time.Second
	// cullPassBudget bounds the whole pass so a slow host cannot stall the
	// panel. Results still unverified when it expires are KEPT (fail-open):
	// running out of time says nothing about the image being broken.
	cullPassBudget = 8 * time.Second
)

// cullBrokenImageResults drops web-search results whose URL is unreachable,
// not an image, or undecodable, using a header-only probe through the
// SSRF-safe client (r.ssrfClient). Contracts:
//
//   - Each distinct URL is probed at most once per pass.
//   - Provider dimensions are kept when present (so the caller's sort order is
//     unchanged, #2280 owns sorting); zero dimensions are filled from the
//     probe. Every result is probed regardless, because a search index's
//     dimensions say nothing about whether the host still serves the file.
//   - If ctx is canceled (client went away) the input is returned untouched:
//     an abandoned request is not evidence that every image is broken.
//   - If the pass budget expires, unverified results are kept.
//   - Relative order of survivors is preserved. May return an empty slice;
//     the caller treats that as a working provider with no usable results,
//     never as an outage.
func (r *Router) cullBrokenImageResults(ctx context.Context, images []provider.ImageResult) []provider.ImageResult {
	return r.cullBrokenImageResultsWith(ctx, images, cullConfig{
		maxConcurrent: cullMaxConcurrent,
		probeTimeout:  cullProbeTimeout,
		passBudget:    cullPassBudget,
	})
}

// cullConfig carries the bounds so tests can shrink them without mutating
// package state (which would race with parallel tests).
type cullConfig struct {
	maxConcurrent int
	probeTimeout  time.Duration
	passBudget    time.Duration
}

func (r *Router) cullBrokenImageResultsWith(ctx context.Context, images []provider.ImageResult, cfg cullConfig) []provider.ImageResult {
	if len(images) == 0 {
		return images
	}

	passCtx, cancel := context.WithTimeout(ctx, cfg.passBudget)
	defer cancel()

	type verdict struct {
		broken bool
		w, h   int
	}
	var (
		mu       sync.Mutex
		verdicts = make(map[string]verdict, len(images))
		seen     = make(map[string]bool, len(images))
		wg       sync.WaitGroup
		sem      = make(chan struct{}, cfg.maxConcurrent)
	)

	for _, im := range images {
		if seen[im.URL] {
			continue
		}
		seen[im.URL] = true
		sem <- struct{}{}
		if passCtx.Err() != nil {
			<-sem
			break // budget spent or client gone: leave the rest unverified
		}
		wg.Add(1)
		go func(url string) {
			defer wg.Done()
			defer func() { <-sem }()
			probeCtx, probeCancel := context.WithTimeout(passCtx, cfg.probeTimeout)
			defer probeCancel()
			info, err := img.ProbeRemoteImageHeaderWithClient(probeCtx, url, r.ssrfClient)
			if err != nil {
				if passCtx.Err() != nil {
					return // aborted by budget/cancel, not a verdict
				}
				r.logger.Debug("culling web search result",
					slog.String("url", url), slog.String("error", err.Error()))
				mu.Lock()
				verdicts[url] = verdict{broken: true}
				mu.Unlock()
				return
			}
			mu.Lock()
			verdicts[url] = verdict{w: info.Width, h: info.Height}
			mu.Unlock()
		}(im.URL)
	}
	wg.Wait()

	if ctx.Err() != nil {
		return images
	}

	kept := make([]provider.ImageResult, 0, len(images))
	for _, im := range images {
		v, probed := verdicts[im.URL]
		if probed && v.broken {
			continue
		}
		if probed && im.Width == 0 && im.Height == 0 {
			im.Width, im.Height = v.w, v.h
		}
		kept = append(kept, im)
	}
	return kept
}
