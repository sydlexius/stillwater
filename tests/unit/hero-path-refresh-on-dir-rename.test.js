// Regression test for the hero-path refresh scoping (#2507).
//
// After a directory_name_mismatch fix renames an artist's on-disk directory,
// the artist-detail hero's #next-hero-path <code> must refresh in place
// (via the existing refreshHero()/rebindHero() pair, the same mechanism the
// name/type history-revert path already uses) -- but ONLY for that one rule.
// Every other fix leaves artist.Path untouched and must NOT trigger a hero
// re-fetch.
//
// The decision itself is factored out of the inline artistDetailPageScript
// (artist_detail.templ) into a standalone first-party module,
// web/static/js/artist-detail/hero-path-sync.js, specifically so it is
// unit-testable: the inline script is embedded in generated templ Go code,
// not a loadable module, so this is the smallest real seam -- see the
// dashboard:action-resolved listener in artist_detail.templ, which gates
// refreshHero()/rebindHero() on window.swHeroPathSync.shouldRefreshHeroPath(evt)
// verbatim. This test does NOT exercise that DOM swap + htmx.ajax call itself
// (untestable without loading the whole inline page script); it proves the
// load-bearing branch condition that gates it.
import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { createDom } from './helpers/dom-harness.js';

function load() {
  const dom = createDom({ modules: ['heroPathSync'] });
  return dom.window.swHeroPathSync;
}

describe('swHeroPathSync.shouldRefreshHeroPath', () => {
  it('is true for a directory_name_mismatch event (the rule that renames the directory)', () => {
    const sync = load();
    const evt = { detail: { ruleId: 'directory_name_mismatch' } };
    assert.equal(sync.shouldRefreshHeroPath(evt), true);
  });

  it('exposes the rule ID it matches, kept in sync with rule.RuleDirectoryNameMismatch', () => {
    const sync = load();
    assert.equal(sync.RULE_ID, 'directory_name_mismatch');
  });

  it('is false for a different rule ID (e.g. a thumb/fanart fix that does not touch artist.Path)', () => {
    const sync = load();
    const evt = { detail: { ruleId: 'thumb_exists' } };
    assert.equal(sync.shouldRefreshHeroPath(evt), false);
  });

  it('is false when the event carries no detail at all (the dashboard-card fix path: a plain HX-Trigger header, no JSON payload)', () => {
    const sync = load();
    assert.equal(sync.shouldRefreshHeroPath({}), false);
    assert.equal(sync.shouldRefreshHeroPath({ detail: null }), false);
    assert.equal(sync.shouldRefreshHeroPath({ detail: {} }), false);
  });

  it('is false for a null/undefined event (defensive: never throws)', () => {
    const sync = load();
    assert.equal(sync.shouldRefreshHeroPath(null), false);
    assert.equal(sync.shouldRefreshHeroPath(undefined), false);
  });
});
