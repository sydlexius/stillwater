// Sidebar navigation controller.
// Manages three states: full (220px, icon+label), icon-only (56px, icons+tooltips),
// hidden (0px, collapsed). State is persisted via the preferences API.
//
// Public API (exposed as window.swSidebar):
//   init()   -- read cached preference and apply initial state
//   cycle()  -- rotate through full -> icon-only -> hidden -> full
(function () {
  'use strict';

  var STATES = ['full', 'icon-only', 'hidden'];

  // Read the base path from the meta tag for sub-path deployments.
  var bpEl = document.querySelector('meta[name="htmx-base-path"]');
  var bp = bpEl ? bpEl.content : '';

  function getNav() {
    return document.getElementById('sw-sidebar');
  }

  function getRestoreBtn() {
    return document.getElementById('sw-sidebar-restore');
  }

  function getCollapseBtn() {
    var nav = getNav();
    return nav ? nav.querySelector('.sw-sidebar-collapse-btn') : null;
  }

  // Apply a sidebar state to the DOM without saving. Used for initial render
  // and for the cycle function (which saves separately).
  function applyState(state) {
    var nav = getNav();
    if (!nav) return;

    nav.setAttribute('data-sidebar-state', state);

    // When hidden, mark the nav as inert so screen readers and keyboard
    // navigation skip it. The restore button is outside the nav and stays
    // focusable.
    if (state === 'hidden') {
      nav.setAttribute('aria-hidden', 'true');
      nav.inert = true;
    } else {
      nav.removeAttribute('aria-hidden');
      nav.inert = false;
    }

    // Update collapse button aria-expanded.
    var collapseBtn = getCollapseBtn();
    if (collapseBtn) {
      collapseBtn.setAttribute('aria-expanded', state === 'full' ? 'true' : 'false');
      // Update label based on state.
      if (state === 'full') {
        collapseBtn.setAttribute('aria-label', 'Collapse sidebar');
      } else if (state === 'icon-only') {
        collapseBtn.setAttribute('aria-label', 'Hide sidebar');
      }
    }

    // Show/hide restore button.
    var restoreBtn = getRestoreBtn();
    if (restoreBtn) {
      if (state === 'hidden') {
        restoreBtn.classList.remove('sw-sidebar-restore-hidden');
      } else {
        restoreBtn.classList.add('sw-sidebar-restore-hidden');
      }
    }

    // Adjust main content margin to accommodate sidebar width.
    var mainContent = document.getElementById('sw-main-content');
    if (mainContent) {
      if (state === 'full') {
        mainContent.style.marginLeft = 'var(--sw-sidebar-width-full)';
      } else if (state === 'icon-only') {
        mainContent.style.marginLeft = 'var(--sw-sidebar-width-icon)';
      } else {
        mainContent.style.marginLeft = '0';
      }
    }

    // Update active link highlighting based on current path.
    highlightActiveLink(nav);
  }

  // Highlight the sidebar link that matches the current URL path.
  function highlightActiveLink(nav) {
    var sidebarState = nav.getAttribute('data-sidebar-state') || 'full';
    var links = nav.querySelectorAll('.sw-sidebar-link[data-path]');
    // Strip base path from current pathname to get the app-relative path.
    var pathname = window.location.pathname;
    if (bp && (pathname === bp || pathname.indexOf(bp + '/') === 0)) {
      pathname = pathname.substring(bp.length);
    }
    // Normalize: ensure leading slash, handle root.
    if (!pathname || pathname === '') {
      pathname = '/';
    }

    var search = window.location.search;
    // Two-pass approach: first determine matches, then keep only the most
    // specific one so parent prefix links don't highlight alongside children.
    var matches = [];

    for (var i = 0; i < links.length; i++) {
      var link = links[i];
      var linkPath = link.getAttribute('data-path');
      var linkQuery = link.getAttribute('data-query');
      var isActive = false;

      if (linkPath === '/') {
        isActive = pathname === '/';
      } else {
        isActive = pathname === linkPath || pathname.indexOf(linkPath + '/') === 0;
      }

      // When a link specifies data-query, require the query parameter is
      // present in the URL for the link to be considered active.
      if (isActive && linkQuery) {
        isActive = ('&' + search.substring(1) + '&').indexOf('&' + linkQuery + '&') !== -1;
      }

      if (isActive) {
        matches.push({ link: link, path: linkPath, query: linkQuery });
      }
    }

    // When multiple links matched, keep only the most specific: prefer the
    // longest data-path, and among equal paths prefer one with data-query.
    var winner = null;
    if (matches.length > 1) {
      for (var j = 0; j < matches.length; j++) {
        var m = matches[j];
        if (!winner ||
            m.path.length > winner.path.length ||
            (m.path.length === winner.path.length && m.query)) {
          winner = m;
        }
      }
    } else if (matches.length === 1) {
      winner = matches[0];
    }

    // Sub-nav children (e.g. Reports > Duplicates) live inside a <ul> that
    // CSS hides outside of full state. If a sub-nav child is the most
    // specific match while the sidebar is in icon-only or hidden mode, the
    // active highlight would land on an invisible element and the user
    // would see no section selected. Promote to the parent <a> so the
    // visible icon keeps the active style. The parent <a> sits as the
    // immediate previous sibling of the <ul.sw-sidebar-subnav>.
    if (winner &&
        sidebarState !== 'full' &&
        winner.link.classList.contains('sw-sidebar-subnav-link')) {
      var subnav = winner.link.closest('.sw-sidebar-subnav');
      var parentLink = subnav ? subnav.previousElementSibling : null;
      if (parentLink && parentLink.classList.contains('sw-sidebar-link')) {
        winner = { link: parentLink, path: parentLink.getAttribute('data-path') || '', query: null };
      }
    }

    for (var k = 0; k < links.length; k++) {
      if (winner && links[k] === winner.link) {
        links[k].classList.add('sw-sidebar-link-active');
      } else {
        links[k].classList.remove('sw-sidebar-link-active');
      }
    }
  }

  // Cycle through states: full -> icon-only -> hidden -> full.
  function cycle() {
    var nav = getNav();
    if (!nav) return;

    var current = nav.getAttribute('data-sidebar-state') || 'full';
    var idx = STATES.indexOf(current);
    var next = STATES[(idx + 1) % STATES.length];

    applyState(next);

    // Save preference via the preferences API.
    if (window.swPreferences) {
      window.swPreferences.set('sidebar_state', next);
    }
  }

  // Initialize sidebar state from cached preferences or default to full.
  function init() {
    var state = 'full';
    if (window.swPreferences) {
      var cached = window.swPreferences.getCache();
      if (cached && cached.sidebar_state) {
        state = cached.sidebar_state;
      }
    }
    applyState(state);
  }

  // Keyboard shortcut: [ to cycle sidebar state.
  // Suppressed inside input, textarea, select, and contenteditable elements.
  document.addEventListener('keydown', function (e) {
    if (e.key !== '[') return;

    var tag = document.activeElement && document.activeElement.tagName;
    if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;
    if (document.activeElement && document.activeElement.isContentEditable) return;

    e.preventDefault();
    cycle();
  });

  // Re-run active-link highlighting after HTMX swaps inject the Duplicates
  // sub-nav child (#1665). highlightActiveLink only runs as part of
  // applyState, which fires on init -- by then the HTMX fragment has not
  // arrived, so a fresh load of /reports/duplicates would briefly highlight
  // the parent Reports link and never the child. Re-running on afterSwap
  // gives the child its highlight as soon as it materializes.
  document.addEventListener('htmx:afterSwap', function (e) {
    if (e && e.target && e.target.id === 'sidebar-duplicates-nav') {
      var nav = getNav();
      if (nav) highlightActiveLink(nav);
    }
  });

  // Listen for preference changes (e.g. from the appearance settings page)
  // to update sidebar state reactively.
  document.addEventListener('sw:preferences-applied', function () {
    if (window.swPreferences) {
      var cached = window.swPreferences.getCache();
      if (cached && cached.sidebar_state) {
        var nav = getNav();
        var current = nav ? nav.getAttribute('data-sidebar-state') : null;
        if (current !== cached.sidebar_state) {
          applyState(cached.sidebar_state);
        }
      }
    }
  });

  // Cycle the color theme through dark -> light -> system -> dark.
  // The preferences API applies the theme immediately (including resolving
  // "system" via the OS media query), so no DOM inspection is needed here.
  function cycleTheme() {
    var ORDER = ['dark', 'light', 'system'];
    var current = 'dark';
    if (window.swPreferences) {
      var cached = window.swPreferences.getCache();
      if (cached && cached.theme) {
        current = cached.theme;
      }
    }
    var idx = ORDER.indexOf(current);
    var next = ORDER[(idx + 1) % ORDER.length];
    if (window.swPreferences) {
      window.swPreferences.set('theme', next);
    }
  }

  // Monotonic sequence number guarding against out-of-order /status
  // responses. Without it, a slow first request racing with a second
  // triggered by `sw:update-status-changed` could overwrite a newer
  // response with stale data. The counter is incremented at the start
  // of every refresh; only responses whose captured sequence still
  // matches the latest get to apply state.
  var updateBadgeRequestSeq = 0;

  // Populate the update-available badge in the sidebar footer.
  //
  // Reads cached status from GET /api/v1/updates/status (never calls /check;
  // that endpoint hits GitHub). If the cached status reports both
  // update_available=true AND a non-empty release_url, set the anchor href
  // and reveal the badge. Any other response (never-checked, not available,
  // network error, non-OK HTTP status, malformed JSON) clears the badge
  // state so stale release links/dots cannot survive a failed refresh
  // after a successful prior response.
  function refreshUpdateBadge() {
    var badge = document.getElementById('sidebar-update-badge');
    if (!badge) return;
    var requestSeq = ++updateBadgeRequestSeq;
    var root = document.getElementById('sw-sidebar');

    // clearBadge drops the href + data-update-available attributes that
    // gate both indicators. Guarded by requestSeq so a late failure
    // cannot wipe state applied by a newer successful response.
    function clearBadge() {
      if (requestSeq !== updateBadgeRequestSeq) return;
      badge.setAttribute('href', '#');
      if (root) root.removeAttribute('data-update-available');
    }

    try {
      var url = (bp || '') + '/api/v1/updates/status';
      fetch(url, { credentials: 'same-origin' })
        .then(function (resp) {
          if (!resp || !resp.ok) {
            clearBadge();
            return null;
          }
          return resp.json();
        })
        .then(function (data) {
          if (!data || requestSeq !== updateBadgeRequestSeq) return;
          var available = data.update_available === true &&
            typeof data.release_url === 'string' &&
            data.release_url !== '';
          // Single source of truth for both indicators (full-state pill and
          // icon-only cog dot): the data-update-available attribute on
          // #sw-sidebar. CSS gates visibility on sidebar state, so we do
          // not touch display classes on the badge itself.
          if (available) {
            badge.setAttribute('href', data.release_url);
            if (root) root.setAttribute('data-update-available', 'true');
          } else {
            clearBadge();
          }
        })
        .catch(function () {
          clearBadge();
        });
    } catch (_e) {
      clearBadge();
    }
  }

  // Expose public API.
  window.swSidebar = {
    init: init,
    cycle: cycle,
    cycleTheme: cycleTheme,
    refreshUpdateBadge: refreshUpdateBadge
  };

  // Refresh the badge when the Updates tab finishes a manual check. The
  // check button uses raw fetch (not HTMX), so it dispatches this custom
  // event after /check succeeds and the UI re-reads /status. The sidebar
  // listens here to keep the pill/dot in sync without requiring a reload.
  document.addEventListener('sw:update-status-changed', function () {
    refreshUpdateBadge();
  });

  // Auto-initialize on DOM ready.
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', function () {
      init();
      refreshUpdateBadge();
    });
  } else {
    init();
    refreshUpdateBadge();
  }
})();
