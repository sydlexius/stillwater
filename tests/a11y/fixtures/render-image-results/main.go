// Command render-image-results prints the REAL server-rendered provider-search
// result fragment for a fixed set of images, so the artwork-modal a11y spec can
// feed genuine result-card markup to the page without a live provider (#3475).
//
// WHY A RENDER TOOL AND NOT HAND-WRITTEN HTML: `make test-a11y` runs offline, so
// a provider search can never return cards, and the cards are where the
// image-card meta text (#2209) lives. Hand-copied markup would be a second
// implementation of the component under test and would keep passing after the
// template changed. Rendering the component itself means the scan sees the
// classes the template ships today.
//
// Usage: go run ./tests/a11y/fixtures/render-image-results <artist-id> <image-url> [images|fanart]
//
// The URL is used for every card and must load (an unloadable image swaps in
// the "Image unavailable" placeholder, a different surface). The optional third
// argument picks the fragment: "images" (default) is the Primary/Logo/Banner
// result panel, "fanart" is the Backdrops kind's result grid.
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/sydlexius/stillwater/internal/i18n"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/web/templates"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run renders the fragment named by args to stdout and returns the process exit
// code: 0 on success, 2 for a usage error, 1 for a load or render failure. It is
// split from main so the tests can drive every branch, including a write
// failure, without spawning a process.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 || len(args) > 3 {
		_, _ = fmt.Fprintln(stderr, "usage: render-image-results <artist-id> <image-url> [images|fanart]")
		return 2
	}
	artistID, url := args[0], args[1]
	kind := "images"
	if len(args) == 3 {
		kind = args[2]
	}
	if kind != "images" && kind != "fanart" {
		_, _ = fmt.Fprintf(stderr, "unknown fragment %q (want images or fanart)\n", kind)
		return 2
	}

	bundle, err := i18n.LoadEmbedded()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "loading locales:", err)
		return 1
	}
	ctx := i18n.WithTranslator(context.Background(), bundle.Translator("en"))

	// One skipped and one errored provider, so the status banner above the cards
	// is rendered and painted. The real handler passes these whenever a provider
	// has no ID for the artist or fails.
	statuses := []provider.ProviderImageStatus{
		{Provider: provider.ProviderName("discogs"), Outcome: provider.ImageOutcomeSkipped, Reason: provider.SkipReasonNoProviderID},
		{Provider: provider.ProviderName("fanarttv"), Outcome: provider.ImageOutcomeErrored, Reason: "request timed out"},
	}

	if kind == "fanart" {
		fanart := []provider.ImageResult{
			{URL: url, Type: provider.ImageFanart, Source: "fanarttv", Width: 1920, Height: 1080, Likes: 42},
			{URL: url, Type: provider.ImageFanart, Source: "audiodb", Width: 1280, Height: 720},
		}
		if err := templates.FanartSearchResults(artistID, fanart, "likes", statuses).Render(ctx, stdout); err != nil {
			_, _ = fmt.Fprintln(stderr, "rendering:", err)
			return 1
		}
		return 0
	}

	// One card per meta shape the template branches on: sized + liked,
	// sized only, unknown size, a low-resolution one, and a logo (which adds the
	// transparency note for duckduckgo).
	images := []provider.ImageResult{
		{URL: url, Type: provider.ImageThumb, Source: "fanarttv", Width: 1000, Height: 1000, Likes: 42},
		{URL: url, Type: provider.ImageThumb, Source: "audiodb", Width: 800, Height: 800},
		{URL: url, Type: provider.ImageThumb, Source: "discogs"},
		{URL: url, Type: provider.ImageThumb, Source: "musicbrainz", Width: 120, Height: 120, Likes: 3},
		{URL: url, Type: provider.ImageLogo, Source: "duckduckgo", Width: 400, Height: 155},
	}
	if err := templates.ImageSearchResults(artistID, images, "likes", false, statuses).Render(ctx, stdout); err != nil {
		_, _ = fmt.Fprintln(stderr, "rendering:", err)
		return 1
	}
	return 0
}
