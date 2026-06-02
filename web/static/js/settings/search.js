// Settings: Keyword search / tab filtering (M55 #1808).
// Extracted verbatim from the inline settingsSearchScript() that used to live
// in web/templates/settings.templ. Filters the stable settings tab bar by
// label, help text, and currently-rendered control values, badges each tab
// with a match count, and Enter-navigates to the first match.
//
// DOM contract (settings search box + tab bar in settings.templ):
//   #settings-search-input            -- the search box this wires.
//   [data-tab="<id>"]                 -- tab buttons; dimmed + badged per match.
//   control ids enumerated in the search index -- highlighted with
//                                        data-search-match / data-search-flash.
//
// Data contract: reads the static index emitted inline by
// settingsSearchIndexScript() as window.swSettingsSearchIndex (that data-bearing
// emitter STAYS inline; only this behavior is externalized). Falls back to an
// empty list if the index has not been set.
//
// Export surface: window.swSettingsSearch doubles as the load-once guard. The
// search wiring self-initializes (DOMContentLoaded or immediately if the DOM is
// already parsed); no inline-handler globals are leaked.
(function () {
  'use strict';

  // Re-init guard: the single window.swSettingsSearch export (assigned at the
  // bottom) doubles as the "already loaded" flag.
  if (window.swSettingsSearch) return;

  // Defer wiring until the input + tab nav exist; this script may be emitted
  // before the elements it queries are parsed by the browser.
  function swInitSettingsSearch() {
    var input = document.getElementById('settings-search-input');
    if (!input) return;
    var index = window.swSettingsSearchIndex || [];

    // matchedEntries tracks which search index entries matched the last
    // applyFilter call so Enter can navigate to the first one without
    // re-running the filter.
    var matchedEntries = [];

    function clearFilter() {
      document.querySelectorAll('[data-tab]').forEach(function (t) {
        t.classList.remove('opacity-40', 'pointer-events-none');
        t.removeAttribute('data-match-count');
      });
      // Remove per-control match highlights and any lingering flash state.
      document.querySelectorAll('[data-search-match]').forEach(function (el) {
        el.removeAttribute('data-search-match');
      });
      document.querySelectorAll('[data-search-flash]').forEach(function (el) {
        el.removeAttribute('data-search-flash');
      });
      matchedEntries = [];
    }

    // controlValuesAt collects the searchable text content of the section
    // rooted at el: <input>/<select>/<textarea>.value plus the visible
    // text of the container (so a current base path like "/stillwater"
    // or a selected option label like "stable" is searchable even though
    // the index doesn't carry the rendered value server-side).
    function controlValuesAt(el) {
      if (!el) return '';
      var parts = [];
      // Find a reasonable scope: prefer the nearest parent "card" so we
      // capture the help span's sibling controls, not the whole tab.
      var scope = el.closest('.sw-card, [data-search-scope], .rounded-lg, .rounded-xl, .rounded-2xl') || el.parentElement || el;
      scope.querySelectorAll('input, select, textarea').forEach(function (f) {
        if (f.type === 'password' || f.type === 'hidden') return;
        if (f.tagName === 'SELECT') {
          var opt = f.options[f.selectedIndex];
          if (opt && opt.text) parts.push(opt.text);
        } else if (f.value) {
          parts.push(f.value);
        }
      });
      // Capture the visible text in status/value spans (e.g. "Active",
      // "512 MB used (2266 files, 741 artists)", read-only display).
      var text = (scope.innerText || scope.textContent || '').slice(0, 1000);
      parts.push(text);
      return parts.join(' ').toLowerCase();
    }

    function applyFilter(query) {
      var q = query.trim().toLowerCase();
      if (!q) { clearFilter(); return; }

      // Clear previous per-control highlights before re-computing.
      document.querySelectorAll('[data-search-match]').forEach(function (el) {
        el.removeAttribute('data-search-match');
      });

      var matchCountByTab = {};
      matchedEntries = [];
      index.forEach(function (e) {
        // Skip entries whose target control is not in the DOM on
        // this render (the index is static; some controls only
        // render under data-dependent branches). Counting them
        // would inflate the tab badge and dead-end Enter-nav.
        var el = document.getElementById(e.id);
        if (!el) return;
        // Match against label, help text, AND current rendered values
        // in the surrounding card so queries like "stable", "/music",
        // or "512 MB" find the relevant control.
        var hit = e.label.toLowerCase().indexOf(q) !== -1 ||
          e.help.toLowerCase().indexOf(q) !== -1 ||
          controlValuesAt(el).indexOf(q) !== -1;
        if (!hit) return;
        matchCountByTab[e.tab] = (matchCountByTab[e.tab] || 0) + 1;
        matchedEntries.push(e);
        el.setAttribute('data-search-match', '1');
      });

      document.querySelectorAll('[data-tab]').forEach(function (t) {
        var tabId = t.getAttribute('data-tab');
        var count = matchCountByTab[tabId] || 0;
        if (count === 0) {
          t.classList.add('opacity-40', 'pointer-events-none');
          t.removeAttribute('data-match-count');
        } else {
          t.classList.remove('opacity-40', 'pointer-events-none');
          t.setAttribute('data-match-count', String(count));
        }
      });
    }

    // navigateToFirstMatch switches to the tab of the first matched entry
    // and scrolls the control into view with a brief flash animation. Called
    // on Enter keypress while the search input is focused.
    function navigateToFirstMatch() {
      if (matchedEntries.length === 0) return;
      var first = matchedEntries[0];
      // Switch to the tab that contains this entry.
      var tabLink = document.querySelector('[data-tab="' + first.tab + '"]');
      if (tabLink) { tabLink.click(); }
      // Scroll and flash the matched control element.
      var el = document.getElementById(first.id);
      if (!el) return;
      el.scrollIntoView({ behavior: 'smooth', block: 'center' });
      el.setAttribute('data-search-flash', '1');
      // Remove the flash attribute after the animation so it can re-trigger
      // if the user navigates to the same match again.
      setTimeout(function () { el.removeAttribute('data-search-flash'); }, 650);
    }

    input.addEventListener('input', function (e) { applyFilter(e.target.value); });

    input.addEventListener('keydown', function (e) {
      if (e.key === 'Enter') {
        e.preventDefault();
        navigateToFirstMatch();
      }
    });

    // Re-apply on init so any residual input.value (browser BFCache /
    // session form restore) does not desync the visible tab state.
    applyFilter(input.value);

    // '/' shortcut: focus the search box when pressed outside an editable element.
    // Guard against synthetic events where e.target may be null so an
    // uncaught TypeError does not break the entire keydown chain.
    document.addEventListener('keydown', function (e) {
      if (e.key !== '/') return;
      if (e.ctrlKey || e.metaKey || e.altKey) return;
      var t = e.target;
      if (!t) return;
      var tag = (t.tagName || '').toLowerCase();
      if (tag === 'input' || tag === 'textarea' || t.isContentEditable) return;
      e.preventDefault();
      input.focus();
      input.select();
    });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', swInitSettingsSearch);
  } else {
    swInitSettingsSearch();
  }

  window.swSettingsSearch = {
    init: swInitSettingsSearch
  };
})();
