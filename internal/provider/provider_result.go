package provider

import (
	"context"
	"errors"
	"log/slog"
)

// ProviderResult caches a single provider's API response for one artist lookup.
// Fields are unexported; use the accessor methods from external packages.
type ProviderResult struct {
	meta            *ArtistMetadata
	images          []ImageResult
	err             error
	imageErr        error // non-nil when GetImages returned a transient error (not ErrNotFound)
	imagesAttempted bool  // true whenever GetImages was actually invoked, regardless of outcome
}

// Meta returns the artist metadata retrieved from this provider, or nil on error or not-found.
func (pr *ProviderResult) Meta() *ArtistMetadata { return pr.meta }

// Images returns the image results retrieved from this provider.
func (pr *ProviderResult) Images() []ImageResult { return pr.images }

// Err returns any transient error that prevented metadata retrieval.
// A nil Err with a nil Meta means the provider definitively reported no data (ErrNotFound).
func (pr *ProviderResult) Err() error { return pr.err }

// ImageErr returns any transient error that prevented image retrieval.
// A nil ImageErr with ImagesAttempted==true means ErrNotFound (no images exist).
func (pr *ProviderResult) ImageErr() error { return pr.imageErr }

// ImagesAttempted reports whether GetImages was invoked for this provider.
// When false, image fields must not be cleared (no ID was available to query).
func (pr *ProviderResult) ImagesAttempted() bool { return pr.imagesAttempted }

// FetchProviderResult executes the per-provider lookup-precedence ladder
// (provider-specific ID > MBID > artist name), retries GetArtist with the
// artist name when the provider reports MBID not-found and implements
// NameLookupProvider, and enforces the ErrNotFound-vs-transient distinction
// for both GetArtist and GetImages so callers can clear stale data on a
// definitive miss while preserving it on a transient failure.
//
// p must be non-nil. aimd may be nil; when nil, AIMD signals are skipped.
// Cache management and registry lookup are the caller's responsibility.
func FetchProviderResult(
	ctx context.Context,
	p Provider,
	name ProviderName,
	mbid, artistName string,
	providerIDs map[ProviderName]string,
	logger *slog.Logger,
	aimd *AIMDController,
) *ProviderResult {
	pr := &ProviderResult{}

	var aimdRateLimitErr error
	aimdGotResult := false

	usedProviderID := false
	id := mbid
	if pid, ok := providerIDs[name]; ok && pid != "" {
		id = pid
		usedProviderID = true
	} else if id == "" {
		id = artistName
	}

	if id != "" {
		meta, queryID, err := fetchArtist(ctx, p, name, id, mbid, artistName, usedProviderID, logger)
		if err != nil {
			var notFound *ErrNotFound
			if errors.As(err, &notFound) {
				logger.Debug("provider has no data for artist",
					slog.String("provider", string(name)),
					slog.String("id", queryID))
			} else {
				logger.Debug("provider GetArtist failed",
					slog.String("provider", string(name)),
					slog.String("error", ScrubError(err)),
					retryAfterAttr(err))
				pr.err = err
				if IsRateLimitError(err) && aimdRateLimitErr == nil {
					aimdRateLimitErr = err
				}
			}
		} else {
			pr.meta = meta
			aimdGotResult = true
		}
	}

	imgID := mbid
	if pid, ok := providerIDs[name]; ok && pid != "" {
		imgID = pid
	}
	if imgID != "" {
		images, err := p.GetImages(ctx, imgID)
		pr.imagesAttempted = true
		if err != nil {
			var notFound *ErrNotFound
			if errors.As(err, &notFound) {
				logger.Debug("provider has no images for artist",
					slog.String("provider", string(name)),
					slog.String("id", imgID))
			} else {
				logger.Warn("provider GetImages failed, preserving existing image data",
					slog.String("provider", string(name)),
					slog.String("error", ScrubError(err)),
					retryAfterAttr(err))
				pr.imageErr = err
				if IsRateLimitError(err) && aimdRateLimitErr == nil {
					aimdRateLimitErr = err
				}
			}
		} else {
			pr.images = images
			aimdGotResult = true
		}
	}

	emitAIMDSignal(aimd, name, aimdRateLimitErr, aimdGotResult)
	return pr
}

// fetchArtist calls p.GetArtist(id) and, when the provider returns ErrNotFound
// for an MBID-based lookup and supports name lookups, retries with artistName.
// It returns the metadata, the query ID that produced the final result (used
// only for logging), and the final error.
func fetchArtist(
	ctx context.Context,
	p Provider,
	name ProviderName,
	id, mbid, artistName string,
	usedProviderID bool,
	logger *slog.Logger,
) (meta *ArtistMetadata, queryID string, err error) {
	queryID = id
	meta, err = p.GetArtist(ctx, id)
	if err != nil && !usedProviderID && mbid != "" && artistName != "" {
		var notFound *ErrNotFound
		if errors.As(err, &notFound) {
			if nlp, ok := p.(NameLookupProvider); ok && nlp.SupportsNameLookup() {
				logger.Debug("retrying with artist name after MBID not-found",
					slog.String("provider", string(name)),
					slog.String("name", artistName))
				queryID = artistName
				meta, err = p.GetArtist(ctx, artistName)
			}
		}
	}
	return meta, queryID, err
}

// emitAIMDSignal fires exactly one AIMD signal per provider call:
// RecordFailure when a rate-limit error was seen, RecordSuccess when at least
// one sub-call returned a useful result and no rate-limit error was recorded.
// Ordinary errors (ErrNotFound, auth, context cancel) produce no signal.
func emitAIMDSignal(aimd *AIMDController, name ProviderName, rateLimitErr error, gotResult bool) {
	if aimd == nil {
		return
	}
	if rateLimitErr != nil {
		aimd.RecordFailure(name, RetryAfterDuration(rateLimitErr))
	} else if gotResult {
		aimd.RecordSuccess(name)
	}
}
