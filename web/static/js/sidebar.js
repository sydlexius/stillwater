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
    var links = nav.querySelectorAll('.sw-sidebar-link[data-path]');
    // Strip base path from current pathname to get the app-relative path.
    var pathname = window.location.pathname;
    if (bp && pathname.indexOf(bp) === 0) {
      pathname = pathname.substring(bp.length);
    }
    // Normalize: ensure leading slash, handle root.
    if (!pathname || pathname === '') {
      pathname = '/';
    }

    for (var i = 0; i < links.length; i++) {
      var link = links[i];
      var linkPath = link.getAttribute('data-path');
      var isActive = false;

      if (linkPath === '/') {
        // Dashboard: exact match only (root).
        isActive = pathname === '/';
      } else {
        // Other links: prefix match (e.g. /artists matches /artists/123).
        isActive = pathname === linkPath || pathname.indexOf(linkPath + '/') === 0;
      }

      if (isActive) {
        link.classList.add('sw-sidebar-link-active');
      } else {
        link.classList.remove('sw-sidebar-link-active');
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

  // Expose public API.
  window.swSidebar = {
    init: init,
    cycle: cycle
  };

  // Auto-initialize on DOM ready.
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
