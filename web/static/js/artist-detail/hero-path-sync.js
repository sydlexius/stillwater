// Artist detail: hero path refresh scoping (#2507).
//
// The directory_name_mismatch fixer is the only rule fixer that mutates
// artist.Path -- DirectoryRenameFixer.Fix (internal/rule/fixers.go) renames
// the on-disk directory to match the canonical name. Every other fixer
// leaves a.Path untouched, so the hero's #next-hero-path <code> must NOT be
// re-fetched for them.
//
// shouldRefreshHeroPath() is the scoping predicate artist_detail.templ's
// dashboard:action-resolved listener consults before calling the inline
// refreshHero()/rebindHero() pair (the same re-fetch-and-swap mechanism
// already used for the name/type history-revert path). It is pulled out
// into its own first-party module -- rather than left as an inline
// condition -- so this scoping logic is unit-testable without loading the
// whole inline artistDetailPageScript.
//
// Export surface: window.swHeroPathSync doubles as the load-once guard.
(function () {
  'use strict';
  if (window.swHeroPathSync) return;

  var DIRECTORY_RENAME_RULE_ID = 'directory_name_mismatch';

  // shouldRefreshHeroPath returns true only when evt.detail.ruleId is the
  // directory-rename rule. artistViolationFix (artist_violations_tab.templ)
  // sets detail.ruleId from the fixed violation's row; the dashboard-card fix
  // path sets no detail at all (plain HX-Trigger header), so this is also
  // false there -- which is correct, since the dashboard page never has an
  // artist-detail hero in the DOM to refresh.
  function shouldRefreshHeroPath(evt) {
    return !!(evt && evt.detail && evt.detail.ruleId === DIRECTORY_RENAME_RULE_ID);
  }

  window.swHeroPathSync = {
    shouldRefreshHeroPath: shouldRefreshHeroPath,
    RULE_ID: DIRECTORY_RENAME_RULE_ID,
  };
})();
