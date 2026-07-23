package artist

import "github.com/sydlexius/stillwater/internal/provider"

// modeledProviders is the canonical set of providers that have a dedicated
// field on the Artist struct and are therefore round-tripped through
// extractProviderIDs / applyProviderIDs. These are the ONLY providers whose
// artist_provider_ids rows the Artist struct can faithfully represent, so they
// are the only rows UpsertAll is allowed to delete-and-replace.
//
// Every other provider (allmusic, duckduckgo, fanarttv, genius, wikipedia) has
// no struct field: it can carry a fetched_at bookkeeping row written directly
// via UpdateProviderFetchedAt, but a round-trip through the Artist struct would
// silently drop it. Scoping UpsertAll's DELETE to this list (rather than an
// unconditional "delete all rows for this artist") is what keeps an ordinary
// Update from destroying those orphan rows -- the data-loss bug fixed in #2725.
//
// INVARIANT: this list and the set of providers extractProviderIDs can emit
// MUST be identical. Adding a struct field + extractProviderIDs case without
// adding the provider here (or vice versa) reintroduces the drift that caused
// the loss. TestModeledProvidersMatchExtractEmitSet enforces this at build/CI
// time; keep them in lockstep.
var modeledProviders = []provider.ProviderName{
	provider.NameMusicBrainz,
	provider.NameAudioDB,
	provider.NameDiscogs,
	provider.NameWikidata,
	provider.NameDeezer,
	provider.NameSpotify,
	provider.NameLastFM,
}
