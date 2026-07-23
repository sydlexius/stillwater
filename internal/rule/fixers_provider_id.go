package rule

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// ProviderFieldUpdater persists a single provider-ID field on an artist. It is
// satisfied by artist.Service.UpdateProviderField, which re-fetches the artist,
// applies the one field, and writes all provider IDs to the normalized table
// consistently.
type ProviderFieldUpdater interface {
	UpdateProviderField(ctx context.Context, id, field, value string) error
}

// providerIDBackfillField maps an in-scope provider name to the artist
// field-update key UpdateProviderField expects.
var providerIDBackfillField = map[provider.ProviderName]string{
	provider.NameDiscogs: "discogs_id",
	provider.NameDeezer:  "deezer_id",
	provider.NameSpotify: "spotify_id",
}

// setProviderIDForName writes value onto the in-scope flat field of a that
// providerIDForName reads. It is the write-side counterpart to that accessor,
// used so a successful backfill is reflected on the same in-memory Artist the
// pipeline goes on to re-evaluate this pass (see the Fix doc comment for why
// this matters -- issue #2699).
func setProviderIDForName(a *artist.Artist, name provider.ProviderName, value string) {
	switch name {
	case provider.NameDiscogs:
		a.DiscogsID = value
	case provider.NameDeezer:
		a.DeezerID = value
	case provider.NameSpotify:
		a.SpotifyID = value
	default:
		// Out-of-scope provider: nothing to write. Mirrors providerIDForName's
		// default branch above.
	}
}

// ProviderIDBackfillFixer resolves provider_id_missing violations by deriving
// the missing Discogs/Deezer/Spotify IDs from an artist's MusicBrainz URL
// relations and filling only the empty ones (issue #2457).
//
// It reuses the shipped provider.ExtractProviderIDsFromURLs helper (and its
// providerURLParsers table) so the parsing stays identical to the orchestrator
// path, and artist.Service.UpdateProviderField so the write goes through the
// normalized provider-ID table. It never overwrites a non-empty stored ID: the
// operator's (or a prior fetch's) existing value is authoritative.
type ProviderIDBackfillFixer struct {
	fetcher MetadataProvider
	updater ProviderFieldUpdater
	logger  *slog.Logger
}

// NewProviderIDBackfillFixer creates a ProviderIDBackfillFixer.
//
//   - fetcher resolves an artist's MusicBrainz metadata (including URL
//     relations); when nil the fixer reports a non-fatal "not available"
//     result rather than failing the run.
//   - updater persists a filled provider-ID field.
func NewProviderIDBackfillFixer(fetcher MetadataProvider, updater ProviderFieldUpdater, logger *slog.Logger) *ProviderIDBackfillFixer {
	if logger == nil {
		logger = slog.Default()
	}
	return &ProviderIDBackfillFixer{
		fetcher: fetcher,
		updater: updater,
		logger:  logger,
	}
}

// CanFix returns true for the provider_id_missing rule.
func (f *ProviderIDBackfillFixer) CanFix(v *Violation) bool {
	return v.RuleID == RuleProviderIDMissing
}

// ProducerPriority implements StateProducer (issue #2738). Fix requires
// a.MusicBrainzID (set by MetadataFixer.fixMBID, tier -2) and mutates
// a.DiscogsID/DeezerID/SpotifyID in place via setProviderIDForName, which
// other rules read (directly, or through a.ProviderIDMap()). It must
// dispatch after nfo_has_mbid but before any tier-0 consumer, so it sits one
// tier above the MBID producer.
func (f *ProviderIDBackfillFixer) ProducerPriority(_ *Violation) int {
	return -1
}

// fetchMetadata routes the MusicBrainz metadata fetch through the per-artist
// EvaluationContext coalescer when one is attached to ctx (the canonical
// pipeline path), so several metadata fixers firing on the same artist share a
// SINGLE upstream FetchMetadata call instead of each issuing its own duplicate
// -- the coalescing built for #1133/#1134/#1135 to stop the fix-all fanout from
// amplifying one artist into N identical provider round-trips. Without an
// EvaluationContext (a single-violation FixViolation call, or a direct unit
// test) it falls through to the raw fetcher. This mirrors coalescedFetchMetadata
// in fixers.go, but is bound to the narrower MetadataProvider fetcher this fixer
// holds rather than the full metadataOrchestrator.
func (f *ProviderIDBackfillFixer) fetchMetadata(ctx context.Context, mbid, name string, providerIDs map[provider.ProviderName]string) (*provider.FetchResult, error) {
	if ec := EvaluationContextFromContext(ctx); ec != nil {
		return ec.FetchMetadata(ctx, mbid, name, providerIDs)
	}
	return f.fetcher.FetchMetadata(ctx, mbid, name, providerIDs)
}

// Fix fetches the artist's MusicBrainz URL relations, derives the in-scope
// provider IDs from them, and fills only the empty ones. A provider ID that is
// already set is never overwritten; a provider with no derivable relation is
// left untouched. The operation is a no-op (non-fatal FixResult) when the
// artist has no MBID, the fetcher is unwired, or nothing new can be derived.
func (f *ProviderIDBackfillFixer) Fix(ctx context.Context, a *artist.Artist, _ *Violation) (*FixResult, error) {
	if a.MusicBrainzID == "" {
		return &FixResult{
			RuleID:  RuleProviderIDMissing,
			Fixed:   false,
			Message: fmt.Sprintf("cannot backfill provider IDs for %s: no MusicBrainz ID to derive them from", a.Name),
		}, nil
	}
	if f.fetcher == nil {
		return &FixResult{
			RuleID:  RuleProviderIDMissing,
			Fixed:   false,
			Message: "metadata provider is not available",
		}, nil
	}
	if f.updater == nil {
		return &FixResult{
			RuleID:  RuleProviderIDMissing,
			Fixed:   false,
			Message: "artist updater is not available",
		}, nil
	}

	// Nothing to derive when every in-scope ID is already set -- most often
	// this fixer running a second time in the same pass immediately after its
	// own successful backfill (Fix mutates a's flat fields in place, see
	// below), but it also covers an artist a human already filled in by hand.
	// Skipping the fetch here is what keeps a repeat call cheap: a filled
	// artist no longer needs a MusicBrainz round-trip to learn that, and
	// nothing this fixer does depends on data currently in `res`.
	needsFill := false
	for _, name := range inScopeProviderIDs {
		if strings.TrimSpace(providerIDForName(a, name)) == "" {
			needsFill = true
			break
		}
	}
	if !needsFill {
		return &FixResult{
			RuleID:  RuleProviderIDMissing,
			Fixed:   false,
			Message: fmt.Sprintf("no provider IDs are missing for %s", a.Name),
		}, nil
	}

	res, err := f.fetchMetadata(ctx, a.MusicBrainzID, a.Name, a.ProviderIDMap())
	if err != nil {
		return nil, fmt.Errorf("fetching MusicBrainz relations for %s: %w", a.Name, err)
	}
	if res == nil || res.Metadata == nil {
		return &FixResult{
			RuleID:  RuleProviderIDMissing,
			Fixed:   false,
			Message: fmt.Sprintf("no MusicBrainz metadata returned for %s", a.Name),
		}, nil
	}

	// Derive provider IDs strictly from the MusicBrainz URL relations using the
	// shipped helper, so the parsing matches the orchestrator's own path. The
	// scratch carries only the URLs; ExtractProviderIDsFromURLs fills its empty
	// ID fields from them.
	scratch := &provider.ArtistMetadata{URLs: res.Metadata.URLs}
	provider.ExtractProviderIDsFromURLs(scratch)

	// Fill-empty only, scoped to the three derivable providers. Iterate the
	// in-scope order for a deterministic message.
	derivedFor := func(name provider.ProviderName) string {
		switch name {
		case provider.NameDiscogs:
			return scratch.DiscogsID
		case provider.NameDeezer:
			return scratch.DeezerID
		case provider.NameSpotify:
			return scratch.SpotifyID
		default:
			return ""
		}
	}

	var filled []string
	for _, name := range inScopeProviderIDs {
		current := strings.TrimSpace(providerIDForName(a, name))
		if current != "" {
			continue // never overwrite an existing ID
		}
		derived := strings.TrimSpace(derivedFor(name))
		if derived == "" {
			continue // no relation to backfill from
		}
		field := providerIDBackfillField[name]
		if err := f.updater.UpdateProviderField(ctx, a.ID, field, derived); err != nil {
			return nil, fmt.Errorf("setting %s for %s: %w", field, a.Name, err)
		}
		// Mirror the write onto the in-memory artist immediately (issue #2699).
		// UpdateProviderField only persists to the DB; without this, the flat
		// field the checker reads (providerIDForName) stays stale for the rest
		// of this pass. The pipeline's post-fix re-evaluation
		// (Pipeline.updateHealthScore) re-runs Evaluate on this SAME *artist.Artist,
		// so a stale field makes the checker report the just-fixed violation as
		// still open. persistPassResults then treats the rule as "still
		// violated" and skips writing any rule_results row for it -- and
		// because the violation was already dispatched-and-resolved, nothing
		// else writes one either, so the artist is left with NO rule_results
		// row for provider_id_missing despite being freshly, correctly
		// evaluated. That is exactly the "no complete evaluation baseline"
		// freeze offlineHealthScore logs. Every other auto-fixer that mutates
		// artist state (fixMBID, fixBio, fixOrigin in fixers.go) sets the
		// field on `a` for this same reason; this fixer was the one place that
		// didn't.
		setProviderIDForName(a, name, derived)
		filled = append(filled, string(name))
	}

	if len(filled) == 0 {
		return &FixResult{
			RuleID:  RuleProviderIDMissing,
			Fixed:   false,
			Message: fmt.Sprintf("no provider IDs could be derived from MusicBrainz relations for %s", a.Name),
		}, nil
	}

	f.logger.Info("provider IDs backfilled by rule fixer",
		slog.String("artist_id", a.ID),
		slog.String("mbid", a.MusicBrainzID),
		slog.String("filled", strings.Join(filled, ",")))

	return &FixResult{
		RuleID: RuleProviderIDMissing,
		Fixed:  true,
		Message: fmt.Sprintf("backfilled %d provider ID(s) for %s from MusicBrainz relations: %s",
			len(filled), a.Name, strings.Join(filled, ", ")),
	}, nil
}
