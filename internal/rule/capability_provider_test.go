package rule

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// unlockedTestArtist is the artist the capability-gate tests pass when they are
// exercising the CAPABILITY dimension only. It is explicitly unlocked so a nil
// handle in those tests can only have come from the capability gate, never from
// the lock gate added for #2754.
func unlockedTestArtist() *artist.Artist {
	return &artist.Artist{ID: "artist-1", Name: "Test Artist", Locked: false}
}

// lockedTestArtist is the same artist with the operator lock set.
func lockedTestArtist() *artist.Artist {
	return &artist.Artist{ID: "artist-1", Name: "Test Artist", Locked: true}
}

// Issue #2476 (Item 2): provider access is gated STRUCTURALLY. The three typed
// accessors are the ONLY path from a rule to a provider handle, and each returns
// nil (logging an error) unless the serving rule declares the matching provider
// capability in ruleProviderCapabilities. These tests pin that gate: a rule not
// in the authority table physically cannot reach a provider, and the omission is
// logged rather than swallowed.

// captureErrorLogger returns a logger that writes JSON records into buf so a
// test can assert a specific error was logged (not just that the call returned
// nil). Level is Debug so nothing is filtered.
func captureErrorLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// gateTestEngine builds an Engine with all three provider handles WIRED (so a
// nil return can only come from the capability gate, never from an unwired
// handle) and a capturing logger.
func gateTestEngine(buf *bytes.Buffer) *Engine {
	return &Engine{
		logger:              captureErrorLogger(buf),
		metadataProvider:    &countingEvalProvider{},
		releaseGroupFetcher: &stubReleaseGroupFetcher{},
		imageFetcher:        &mockImageFetcher{},
	}
}

// assertDenyLogged fails unless buf holds an ERROR record for the given rule and
// capability. This is what makes the "returns nil" assertion non-vacuous: a nil
// return with no log would be a SILENT failure, exactly what the gate must not do.
func assertDenyLogged(t *testing.T, buf *bytes.Buffer, ruleID, capability string) {
	t.Helper()
	var found bool
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q (%v)", line, err)
		}
		if rec["level"] == "ERROR" && rec["rule_id"] == ruleID && rec["required_capability"] == capability {
			found = true
		}
	}
	if !found {
		t.Errorf("no ERROR log for rule %q missing capability %q; the gate returned nil SILENTLY. "+
			"Log dump:\n%s", ruleID, capability, buf.String())
	}
}

// TestProviderAccessors_DeclaredRulesGetHandle is the positive side and the
// drift-guard anchor. Each rule that legitimately reaches a provider must get a
// non-nil handle back.
//
// REVERT-AND-RERUN: delete any of these rules from ruleProviderCapabilities and
// its accessor returns nil here, taking the matching sub-assertion RED -- that
// is the structural guarantee made executable.
func TestProviderAccessors_DeclaredRulesGetHandle(t *testing.T) {
	var buf bytes.Buffer
	e := gateTestEngine(&buf)

	// External-provider rules reach the metadata + release-group handles.
	if e.metadataProviderFor(RuleNameLanguagePref, unlockedTestArtist()) == nil {
		t.Errorf("name_language_pref declares external_provider but metadataProviderFor returned nil")
	}
	if e.releaseGroupFetcherFor(RuleDiscographyPopulated, unlockedTestArtist()) == nil {
		t.Errorf("discography_populated declares external_provider but releaseGroupFetcherFor returned nil")
	}

	// Network-dependent (local media server) rules reach the image fetcher.
	if e.platformImageFetcherFor(RuleLogoPadding, unlockedTestArtist()) == nil {
		t.Errorf("logo_padding declares network_dependent but platformImageFetcherFor returned nil")
	}
	if e.platformImageFetcherFor(RuleExtraneousImages, unlockedTestArtist()) == nil {
		t.Errorf("extraneous_images declares network_dependent but platformImageFetcherFor returned nil")
	}

	// The external rules also satisfy the network_dependent subset, so the image
	// fetcher is reachable for them too.
	if e.platformImageFetcherFor(RuleDiscographyPopulated, unlockedTestArtist()) == nil {
		t.Errorf("discography_populated declares external_provider (subset of network_dependent) but " +
			"platformImageFetcherFor returned nil")
	}

	// Nothing above should have logged a denial.
	if strings.Contains(buf.String(), "ERROR") {
		t.Errorf("a declared rule triggered a denial log; gate is over-restrictive:\n%s", buf.String())
	}
}

// TestProviderAccessors_UndeclaredRuleGetsNilAndLogsError is the core drift
// guard: a rule NOT in the authority table cannot reach ANY provider, and each
// denial is logged at ERROR. A future checker that grabs a handle without
// declaring the capability lands here -- nil, not the network.
func TestProviderAccessors_UndeclaredRuleGetsNilAndLogsError(t *testing.T) {
	const undeclared = "a_future_rule_that_forgot_to_declare"

	if _, ok := ruleProviderCapabilities[undeclared]; ok {
		t.Fatalf("precondition: %q must be ABSENT from the authority table", undeclared)
	}

	t.Run("metadata", func(t *testing.T) {
		var buf bytes.Buffer
		e := gateTestEngine(&buf)
		if e.metadataProviderFor(undeclared, unlockedTestArtist()) != nil {
			t.Errorf("undeclared rule reached the metadata provider; the gate did not hold")
		}
		assertDenyLogged(t, &buf, undeclared, string(capExternalProvider))
	})

	t.Run("release_groups", func(t *testing.T) {
		var buf bytes.Buffer
		e := gateTestEngine(&buf)
		if e.releaseGroupFetcherFor(undeclared, unlockedTestArtist()) != nil {
			t.Errorf("undeclared rule reached the release-group fetcher; the gate did not hold")
		}
		assertDenyLogged(t, &buf, undeclared, string(capExternalProvider))
	})

	t.Run("platform_images", func(t *testing.T) {
		var buf bytes.Buffer
		e := gateTestEngine(&buf)
		if e.platformImageFetcherFor(undeclared, unlockedTestArtist()) != nil {
			t.Errorf("undeclared rule reached the platform image fetcher; the gate did not hold")
		}
		assertDenyLogged(t, &buf, undeclared, string(capNetworkDependent))
	})
}

// TestProviderAccessors_LocalOnlyRulesCannotReachThirdPartyAPIs pins the whole
// point of TWO capabilities: a rule that only touches the LOCAL media server
// (network_dependent, NOT external_provider) must be denied a third-party API
// handle. Collapsing the two flags into one would silently let these rules reach
// MusicBrainz et al.
func TestProviderAccessors_LocalOnlyRulesCannotReachThirdPartyAPIs(t *testing.T) {
	for _, ruleID := range []string{RuleLogoPadding, RuleExtraneousImages} {
		t.Run(ruleID, func(t *testing.T) {
			var buf bytes.Buffer
			e := gateTestEngine(&buf)

			if e.metadataProviderFor(ruleID, unlockedTestArtist()) != nil {
				t.Errorf("%s (network_dependent only) reached the third-party metadata provider; "+
					"it must be denied without external_provider", ruleID)
			}
			assertDenyLogged(t, &buf, ruleID, string(capExternalProvider))

			buf.Reset()
			if e.releaseGroupFetcherFor(ruleID, unlockedTestArtist()) != nil {
				t.Errorf("%s (network_dependent only) reached the third-party release-group fetcher", ruleID)
			}
			assertDenyLogged(t, &buf, ruleID, string(capExternalProvider))

			// ...but it CAN reach the local image fetcher, which is its actual need.
			buf.Reset()
			if e.platformImageFetcherFor(ruleID, unlockedTestArtist()) == nil {
				t.Errorf("%s declares network_dependent but was denied the local image fetcher", ruleID)
			}
			if strings.Contains(buf.String(), "ERROR") {
				t.Errorf("%s was denied its declared local image fetcher:\n%s", ruleID, buf.String())
			}
		})
	}
}

// TestProviderCapabilities_ExternalImpliesNetworkDependent pins the subset
// invariant the mapping relies on: every rule granted external_provider also
// carries network_dependent, so a single "does this rule do ANY outbound?" query
// (has network_dependent) stays correct.
func TestProviderCapabilities_ExternalImpliesNetworkDependent(t *testing.T) {
	for ruleID, caps := range ruleProviderCapabilities {
		if caps[capExternalProvider] && !caps[capNetworkDependent] {
			t.Errorf("%s declares external_provider without network_dependent; the subset invariant "+
				"is broken (external is a strict subset of network-dependent)", ruleID)
		}
	}
}

// Issue #2754: the artist LOCK is an additional gate on the same three
// accessors. A locked artist is one the operator has declared finished, so
// Stillwater must not reach a provider on its behalf -- not even in the
// background, which is exactly what happens today: a manual edit (which the lock
// explicitly still permits) publishes ArtistUpdated, the rule-health subscriber
// turns that into a full Evaluate, and the discography checker fires an outbound
// MusicBrainz request for the artist the operator just locked.

// TestProviderAccessors_LockedArtistDeniedEveryHandle is the class-level guard.
// Every accessor -- including the local-media-server one -- must return nil for
// a locked artist even though the rule fully declares the capability and the
// handle is wired. Gating here rather than inside one checker is what makes a
// FUTURE provider-backed checker inherit the protection.
//
// REVERT-AND-RERUN: drop the providerDeniedByLock call from any accessor and
// that accessor's sub-assertion goes RED.
func TestProviderAccessors_LockedArtistDeniedEveryHandle(t *testing.T) {
	var buf bytes.Buffer
	e := gateTestEngine(&buf)
	locked := lockedTestArtist()

	if e.releaseGroupFetcherFor(RuleDiscographyPopulated, locked) != nil {
		t.Error("locked artist reached the release-group fetcher; the lock gate did not hold")
	}
	if e.metadataProviderFor(RuleNameLanguagePref, locked) != nil {
		t.Error("locked artist reached the metadata provider; the lock gate did not hold")
	}
	if e.platformImageFetcherFor(RuleLogoPadding, locked) != nil {
		t.Error("locked artist reached the platform image fetcher; the lock gate did not hold")
	}

	// The denial is a normal operating decision, not a programming error, so it
	// logs at Debug -- but it must not be SILENT.
	if !strings.Contains(buf.String(), "skipping provider fetch: artist is locked") {
		t.Errorf("lock denial was not logged at all; a silent skip is unexplainable in the field. "+
			"Log dump:\n%s", buf.String())
	}

	// POSITIVE CONTROL: the identical calls with an UNLOCKED artist must return
	// live handles. Without this the assertions above pass vacuously whenever the
	// gate over-denies (or the test engine is simply unwired).
	buf.Reset()
	unlocked := unlockedTestArtist()
	if e.releaseGroupFetcherFor(RuleDiscographyPopulated, unlocked) == nil {
		t.Error("unlocked artist was denied the release-group fetcher; the gate over-denies")
	}
	if e.metadataProviderFor(RuleNameLanguagePref, unlocked) == nil {
		t.Error("unlocked artist was denied the metadata provider; the gate over-denies")
	}
	if e.platformImageFetcherFor(RuleLogoPadding, unlocked) == nil {
		t.Error("unlocked artist was denied the platform image fetcher; the gate over-denies")
	}
	if strings.Contains(buf.String(), "artist is locked") {
		t.Errorf("an UNLOCKED artist triggered the lock denial:\n%s", buf.String())
	}
}

// TestProviderAccessors_NilArtistDoesNotDeny pins the nil-artist contract. A nil
// artist cannot be locked, so it must not be treated as locked -- the opposite
// would silently disable provider access at any call site that has no artist in
// hand, which is an over-gate that would look exactly like a broken provider.
func TestProviderAccessors_NilArtistDoesNotDeny(t *testing.T) {
	var buf bytes.Buffer
	e := gateTestEngine(&buf)

	if e.releaseGroupFetcherFor(RuleDiscographyPopulated, nil) == nil {
		t.Error("a nil artist was treated as locked; nil is unknown, not locked")
	}
	if strings.Contains(buf.String(), "artist is locked") {
		t.Errorf("nil artist triggered the lock denial:\n%s", buf.String())
	}
}
