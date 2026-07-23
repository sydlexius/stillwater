package rule

import (
	"log/slog"

	"github.com/sydlexius/stillwater/internal/artist"
)

// providerCapability enumerates the provider-access capabilities a rule must
// declare before its checker can reach a provider handle. It is the structural
// guard introduced for issue #2476, and is deliberately DISTINCT from
// ruleCapability in capability.go: that one is a per-(rule, artist)
// ELIGIBILITY predicate that produces a skip; this one is a rule-static ACCESS
// gate that returns a nil handle (after logging) so a checker physically
// cannot reach a provider it did not declare. This guard covers the CHECKER
// surface only -- fixers obtain provider handles through their own
// constructor-injected dependencies and are out of scope here (see the note
// near ruleProviderCapabilities). The typed accessors below are the ONLY path
// from a checker to a provider, so the declaration here cannot drift out of
// sync with the code -- the code cannot bypass it.
type providerCapability string

const (
	// capExternalProvider marks a rule that reaches a THIRD-PARTY API
	// (MusicBrainz, Discogs, Deezer, Spotify, Wikipedia, ...). A rule WITHOUT
	// this capability must run fully offline: this is the "bad internet
	// citizen" / offline-invariant guard. capExternalProvider is a strict
	// subset of capNetworkDependent -- any third-party call is also outbound.
	capExternalProvider providerCapability = "external_provider"

	// capNetworkDependent marks a rule that makes ANY outbound call, including
	// to the user's LOCAL media server (Emby/Jellyfin) via the platform image
	// bridge. It does NOT by itself permit a third-party API; that needs
	// capExternalProvider.
	capNetworkDependent providerCapability = "network_dependent"
)

// ruleProviderCapabilities is the SINGLE authority for which rules may reach a
// provider, and which kind. It is the only declaration site: adding a rule here
// is the deliberate act of granting it network access. capExternalProvider
// implies capNetworkDependent, so the external rules list BOTH --
// ruleHasProviderCapability is a plain lookup and does not auto-expand, which
// keeps this map fully self-describing (you can read a rule's exact reach off
// one line).
//
//   - discography_populated / name_language_pref -> third-party MusicBrainz et al.
//   - logo_padding / extraneous_images           -> local Emby/Jellyfin only.
//
// A rule absent from this map (e.g. provider_id_missing, which only reads
// provider AVAILABILITY from config and does no outbound fetch) has no
// capability and every accessor below returns nil for it.
//
// Scope note: this map, and the accessors below, gate the CHECKER surface
// only. Fixers (e.g. LogoPaddingFixer) receive their provider handles through
// their own constructor-injected dependencies and are not routed through this
// gate. Extending the gate to the fixer surface is a separate follow-up (a
// unified provider-access-layer refactor), deliberately out of scope here.
var ruleProviderCapabilities = map[string]map[providerCapability]bool{
	RuleDiscographyPopulated: {capExternalProvider: true, capNetworkDependent: true},
	RuleNameLanguagePref:     {capExternalProvider: true, capNetworkDependent: true},
	RuleLogoPadding:          {capNetworkDependent: true},
	RuleExtraneousImages:     {capNetworkDependent: true},
}

// ruleHasProviderCapability reports whether ruleID declares capability in the
// single authority table. A rule that is absent from the table has none.
func (e *Engine) ruleHasProviderCapability(ruleID string, capability providerCapability) bool {
	return ruleProviderCapabilities[ruleID][capability]
}

// denyProvider logs the structural-guard violation. It is called from every
// accessor on the undeclared-capability branch so an undeclared checker fails
// LOUD (log.Error, never a silent nil) while still degrading rather than
// crashing -- the caller treats the nil handle exactly as an unwired provider.
func (e *Engine) denyProvider(ruleID string, capability providerCapability, handle string) {
	e.logger.Error("rule requested a provider handle without declaring the required capability; returning nil",
		slog.String("rule_id", ruleID),
		slog.String("required_capability", string(capability)),
		slog.String("handle", handle))
}

// providerDeniedByLock reports whether a is locked, and logs the denial when it
// is. A locked artist is one the operator has declared finished: Stillwater must
// not reach out to a provider on its behalf, because any answer it gets back is
// something it is forbidden to act on. The lock explicitly still permits manual
// edits, and a manual edit publishes ArtistUpdated, which the rule-health
// subscriber turns into a full Evaluate -- so without this gate a locked artist
// generates unrequested outbound provider traffic in the background with no
// operator action at all (#2754).
//
// The gate lives HERE, on the shared accessors, rather than inside any one
// checker, so it covers the whole CLASS: every present and future checker
// reaches a provider through these three functions and nowhere else (that is the
// #2476 structural guarantee), so a new provider-backed checker inherits the
// lock gate without its author having to remember it.
//
// Deliberately narrow: this suppresses the provider FETCH only. It does not
// touch rule eligibility, so a locked artist is still fully evaluated by every
// local-state rule and still receives a real health score. Gating at eligibility
// instead would blank the health score of every locked artist.
//
// a may be nil at call sites that have no artist in hand; a nil artist cannot be
// locked, so the fetch proceeds.
func (e *Engine) providerDeniedByLock(ruleID string, a *artist.Artist, handle string) bool {
	if a == nil || !a.Locked {
		return false
	}
	e.logger.Debug("skipping provider fetch: artist is locked",
		slog.String("rule_id", ruleID),
		slog.String("artist_id", a.ID),
		slog.String("artist", a.Name),
		slog.String("handle", handle))
	return true
}

// metadataProviderFor is the SOLE path to e.metadataProvider. It returns the
// provider only when ruleID declares capExternalProvider AND the artist is not
// locked; otherwise it returns nil (logging the reason) so an undeclared checker
// cannot reach a third-party metadata API and a locked artist generates no
// outbound traffic. A returned nil may also simply mean the provider is unwired
// (SetMetadataProvider never called) -- all three are handled identically by the
// caller, which degrades to a no-op.
func (e *Engine) metadataProviderFor(ruleID string, a *artist.Artist) MetadataProvider {
	if !e.ruleHasProviderCapability(ruleID, capExternalProvider) {
		e.denyProvider(ruleID, capExternalProvider, "metadata_provider")
		return nil
	}
	if e.providerDeniedByLock(ruleID, a, "metadata_provider") {
		return nil
	}
	return e.metadataProvider
}

// releaseGroupFetcherFor is the SOLE path to e.releaseGroupFetcher. It returns
// the fetcher only when ruleID declares capExternalProvider and the artist is
// not locked; otherwise it returns nil after logging the reason.
func (e *Engine) releaseGroupFetcherFor(ruleID string, a *artist.Artist) ReleaseGroupFetcher {
	if !e.ruleHasProviderCapability(ruleID, capExternalProvider) {
		e.denyProvider(ruleID, capExternalProvider, "release_group_fetcher")
		return nil
	}
	if e.providerDeniedByLock(ruleID, a, "release_group_fetcher") {
		return nil
	}
	return e.releaseGroupFetcher
}

// platformImageFetcherFor is the SOLE path to e.imageFetcher. It returns the
// fetcher only when ruleID declares capNetworkDependent and the artist is not
// locked; otherwise it returns nil after logging the reason. capExternalProvider
// rules also satisfy the capability side (the subset relationship), but none of
// the current provider-dependent rules need both handles.
func (e *Engine) platformImageFetcherFor(ruleID string, a *artist.Artist) PlatformImageFetcher {
	if !e.ruleHasProviderCapability(ruleID, capNetworkDependent) {
		e.denyProvider(ruleID, capNetworkDependent, "platform_image_fetcher")
		return nil
	}
	if e.providerDeniedByLock(ruleID, a, "platform_image_fetcher") {
		return nil
	}
	return e.imageFetcher
}
