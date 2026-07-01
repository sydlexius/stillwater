// next/ Settings rail controller (M55 #1339). Firefox/Chrome about:preferences
// model: every section renders in one scrollable pane and the rail is a
// scroll-spy + jump nav.
//
//   1. Scroll-spy -- an IntersectionObserver marks the rail item for the section
//      currently in view aria-current="true" + .sw-next-rail-item-active (the
//      3px accent; styling is CSS, no solid-blue fill). Deep-links
//      (/next/settings#section-<id>) scroll to the section on load.
//   2. Click-to-jump -- a rail item smooth-scrolls to its #section-<id> anchor
//      and updates the hash (back/forward friendly).
//   3. Keyword filter -- matches each rail item's label OR its data-keywords
//      index; a non-label match shows an inline "↳ <matched-keyword>" line. The
//      filter NARROWS THE RAIL (its jump targets): non-matching items hide, empty
//      groups collapse, a no-match state offers Clear. The pane stays fully
//      scrollable. Persists to localStorage('sw-settings-filter').
//   4. Keyboard -- '/' focus filter, Esc clear, ↑/↓ move the rail selection AND
//      jump to that section, Enter jump to the selected item, ⌘S/Ctrl+S blur-to-
//      save the focused control.
//   5. Mobile -- a hamburger ([data-rail-toggle]) collapses/expands the section
//      nav on narrow viewports; the toggle label tracks the active section.
//   6. settings.changed SSE -- debounced cross-tab refresh, suppressed while the
//      user is editing here.
//
// DOM contract (web/templates/next/settings.templ):
//   #sw-next-settings-pane                            -- scrollable content column
//   section[data-rail-section][id="section-<id>"]     -- one section (scroll target)
//   a.sw-next-rail-item[data-rail-link][data-keywords][data-label] -- rail item
//   .sw-next-rail-hit[data-rail-hit]                  -- per-item ↳ line span
//   .sw-next-rail-group[data-rail-group]              -- a rail group
//   [data-rail-empty][data-empty-template]            -- empty-state ("{query}")
//   [data-rail-clear]                                 -- clear-filter button
//   [data-rail-toggle][data-rail-toggle-label]        -- mobile nav hamburger
//   #settings-search-input                            -- the filter box
//
// Export surface: window.swNextSettings doubles as the load-once guard.
(function () {
  'use strict';

  if (window.swNextSettings) return;

  var FILTER_STORAGE_KEY = 'sw-settings-filter';

  function list(root, sel) {
    return Array.prototype.slice.call((root || document).querySelectorAll(sel));
  }

  // M55 #2117 (next-only a11y): the pane composes shared section bodies, one of
  // which (the Users table) wraps its content in a horizontally scrollable
  // `.overflow-x-auto` div. axe `scrollable-region-focusable` requires a scroll
  // container to be keyboard-reachable. We can't add the attribute in the shared
  // template without changing the stable v1 render, so we enhance it here on the
  // next/ page only: make each scrollable region focusable (tabindex=0) and, when
  // a section heading is available, expose it as a labelled region (role=region +
  // localized aria-label from the heading). Idempotent + re-run-safe.
  function swMakeScrollRegionsFocusable(root) {
    var regions = list(root, '.overflow-x-auto, .overflow-auto');
    for (var i = 0; i < regions.length; i++) {
      var el = regions[i];
      if (el.dataset.swScrollA11y === '1') continue;
      el.dataset.swScrollA11y = '1';
      if (!el.hasAttribute('tabindex')) el.setAttribute('tabindex', '0');
      if (!el.getAttribute('aria-label') && !el.getAttribute('aria-labelledby')) {
        var section = el.closest('section[data-rail-section]');
        var heading = section && section.querySelector('.sw-next-section-heading');
        var label = heading ? heading.textContent.trim() : '';
        if (label) {
          if (!el.getAttribute('role')) el.setAttribute('role', 'region');
          el.setAttribute('aria-label', label);
        }
      }
    }
  }

  function swInitNextSettings() {
    var pane = document.getElementById('sw-next-settings-pane');
    if (!pane) return;

    swMakeScrollRegionsFocusable(pane);

    // Re-apply after any HTMX swap inside the pane so scroll regions added by
    // dynamic content (e.g. Users table refresh / pagination) stay
    // keyboard-accessible. Idempotent via the swScrollA11y guard. (CR #2159)
    pane.addEventListener('htmx:afterSwap', function () {
      swMakeScrollRegionsFocusable(pane);
    });

    var input = document.getElementById('settings-search-input');
    var sections = list(pane, 'section[data-rail-section]');
    var items = list(document, 'a.sw-next-rail-item[data-rail-link]');
    var groups = list(document, '.sw-next-rail-group[data-rail-group]');
    var emptyEl = document.querySelector('[data-rail-empty]');
    var rail = document.querySelector('.sw-next-settings-rail');
    var toggle = document.querySelector('[data-rail-toggle]');
    var toggleLabel = document.querySelector('[data-rail-toggle-label]');
    var toggleDefault = toggleLabel ? toggleLabel.textContent : '';

    var sectionById = {};
    sections.forEach(function (s) { sectionById[s.getAttribute('data-rail-section')] = s; });
    var itemById = {};
    items.forEach(function (a) { itemById[a.getAttribute('data-rail-link')] = a; });

    // ---- Active state (scroll-spy + click) ------------------------------

    var activeId = '';
    function setActive(id) {
      if (!id || id === activeId) return;
      activeId = id;
      items.forEach(function (a) {
        var on = a.getAttribute('data-rail-link') === id;
        a.classList.toggle('sw-next-rail-item-active', on);
        if (on) {
          a.setAttribute('aria-current', 'true');
          if (a.scrollIntoView) a.scrollIntoView({ block: 'nearest' });
        } else {
          a.removeAttribute('aria-current');
        }
      });
      // Mobile: surface the active section in the (collapsed) hamburger label.
      if (toggleLabel) {
        var act = itemById[id];
        toggleLabel.textContent = act ? (act.getAttribute('data-label') || toggleDefault) : toggleDefault;
      }
    }

    function prefersReducedMotion() {
      return window.matchMedia('(prefers-reduced-motion: reduce)').matches
        || document.documentElement.dataset.motion === 'on';
    }

    function jumpTo(id, smooth) {
      var section = sectionById[id];
      if (!section) return;
      section.scrollIntoView({ behavior: (smooth && !prefersReducedMotion()) ? 'smooth' : 'auto', block: 'start' });
      setActive(id);
    }

    var hashTimer = null;
    function updateHash(id) {
      if (hashTimer) clearTimeout(hashTimer);
      hashTimer = setTimeout(function () {
        var newHash = '#section-' + id;
        if (window.location.hash !== newHash) {
          history.replaceState(null, '', window.location.pathname + window.location.search + newHash);
        }
      }, 150);
    }

    // Rail click: smooth-scroll to the section + set a pushState hash so Back
    // returns to the prior section. preventDefault keeps the browser from also
    // doing its own instant jump.
    items.forEach(function (a) {
      a.addEventListener('click', function (e) {
        e.preventDefault();
        var id = a.getAttribute('data-rail-link');
        if (window.location.hash !== '#section-' + id) {
          history.pushState(null, '', window.location.pathname + window.location.search + '#section-' + id);
        }
        jumpTo(id, true);
        setNavOpen(false); // collapse the mobile nav after a jump
      });
    });

    // ---- Scroll-spy ------------------------------------------------------

    if ('IntersectionObserver' in window && sections.length) {
      // Bias the active zone to the upper third so the highlighted item matches
      // what the user is reading, not what is merely visible at the bottom.
      var visible = {};
      var observer = new IntersectionObserver(function (entries) {
        entries.forEach(function (entry) {
          var id = entry.target.getAttribute('data-rail-section');
          if (entry.isIntersecting) { visible[id] = entry.intersectionRatio; } else { delete visible[id]; }
        });
        // First section (document order) currently in the active zone wins.
        var found = null;
        for (var i = 0; i < sections.length; i++) {
          var sid = sections[i].getAttribute('data-rail-section');
          if (visible[sid] !== undefined && !sections[i].hidden) { found = sid; break; }
        }
        if (found) { setActive(found); updateHash(found); }
      }, { rootMargin: '-10% 0px -70% 0px', threshold: [0, 0.01, 0.5, 1] });
      sections.forEach(function (s) { observer.observe(s); });
    }

    // Deep-link on load: if the URL targets a section, scroll to it.
    (function scrollToHash() {
      var h = window.location.hash;
      if (!h || h.indexOf('#section-') !== 0) return;
      jumpTo(h.slice('#section-'.length), false);
    })();
    window.addEventListener('hashchange', function () {
      var h = window.location.hash;
      if (h && h.indexOf('#section-') === 0) jumpTo(h.slice('#section-'.length), true);
    });

    // ---- Keyword filter (narrows the rail) -------------------------------

    function matchItem(a, q) {
      var label = (a.getAttribute('data-label') || '').toLowerCase();
      if (label.indexOf(q) !== -1) return { ok: true, hit: null };
      var kws = (a.getAttribute('data-keywords') || '').split('|');
      for (var i = 0; i < kws.length; i++) {
        if (kws[i] && kws[i].toLowerCase().indexOf(q) !== -1) return { ok: true, hit: kws[i] };
      }
      return { ok: false, hit: null };
    }

    function setHit(a, kw) {
      var hit = a.querySelector('[data-rail-hit]');
      if (!hit) return;
      if (kw) {
        hit.textContent = '↳ ' + kw;
        hit.hidden = false;
        a.classList.add('sw-next-rail-item-hashit');
      } else {
        hit.textContent = '';
        hit.hidden = true;
        a.classList.remove('sw-next-rail-item-hashit');
      }
    }

    function applyFilter(query) {
      var q = (query || '').trim().toLowerCase();
      var anyVisible = false;

      items.forEach(function (a) {
        if (!q) { a.hidden = false; setHit(a, null); anyVisible = true; return; }
        var m = matchItem(a, q);
        a.hidden = !m.ok;
        setHit(a, m.ok ? m.hit : null);
        if (m.ok) anyVisible = true;
      });

      // Collapse rail groups with no visible items.
      groups.forEach(function (g) {
        var anyOn = list(g, 'a.sw-next-rail-item').some(function (a) { return !a.hidden; });
        g.hidden = !anyOn;
      });

      // Empty state.
      if (emptyEl) {
        if (q && !anyVisible) {
          var tpl = emptyEl.getAttribute('data-empty-template') || 'No settings match "{query}".';
          var txt = emptyEl.querySelector('[data-rail-empty-text]');
          if (txt) txt.textContent = tpl.replace('{query}', query.trim());
          emptyEl.hidden = false;
        } else {
          emptyEl.hidden = true;
        }
      }
      highlightIndex = -1;
      updateHighlight();
    }

    // ---- Keyboard selection (↑/↓ move the rail selection and jump) -------

    var highlightIndex = -1;
    function visibleItems() {
      return items.filter(function (a) { return !a.hidden; });
    }
    function updateHighlight() {
      items.forEach(function (a) { a.classList.remove('sw-next-rail-item-highlight'); });
      var vis = visibleItems();
      if (highlightIndex >= 0 && highlightIndex < vis.length) {
        var a = vis[highlightIndex];
        a.classList.add('sw-next-rail-item-highlight');
        if (a.scrollIntoView) a.scrollIntoView({ block: 'nearest' });
      }
    }
    function moveHighlight(delta) {
      var vis = visibleItems();
      if (!vis.length) return;
      if (highlightIndex < 0) {
        highlightIndex = delta > 0 ? 0 : vis.length - 1;
      } else {
        highlightIndex = (highlightIndex + delta + vis.length) % vis.length;
      }
      updateHighlight();
      // "moves the rail selection AND jumps" -- scroll the chosen section in.
      var sel = vis[highlightIndex];
      if (sel) jumpTo(sel.getAttribute('data-rail-link'), true);
    }

    function clearFilter() {
      if (input) input.value = '';
      try { window.localStorage.removeItem(FILTER_STORAGE_KEY); } catch (err) { /* private mode */ }
      applyFilter('');
    }

    if (input) {
      input.addEventListener('input', function (e) {
        var v = e.target.value;
        try { window.localStorage.setItem(FILTER_STORAGE_KEY, v); } catch (err) { /* private mode */ }
        applyFilter(v);
      });
      input.addEventListener('keydown', function (e) {
        if (e.key === 'Escape') { e.preventDefault(); clearFilter(); return; }
        if (e.key === 'ArrowDown') { e.preventDefault(); moveHighlight(1); return; }
        if (e.key === 'ArrowUp') { e.preventDefault(); moveHighlight(-1); return; }
        if (e.key === 'Enter') {
          e.preventDefault();
          var vis = visibleItems();
          var target = (highlightIndex >= 0 && highlightIndex < vis.length) ? vis[highlightIndex] : vis[0];
          if (target) target.click();
        }
      });

      // Restore the persisted query (the search you left, FF-style).
      var saved = '';
      try { saved = window.localStorage.getItem(FILTER_STORAGE_KEY) || ''; } catch (err) { saved = ''; }
      if (saved) input.value = saved;
      applyFilter(input.value);
    }

    if (emptyEl) {
      var clearBtn = emptyEl.querySelector('[data-rail-clear]');
      if (clearBtn) clearBtn.addEventListener('click', function () { clearFilter(); if (input) input.focus(); });
    }

    // '/' focuses the filter when not already typing in a text field.
    document.addEventListener('keydown', function (e) {
      if (e.key !== '/') return;
      if (e.ctrlKey || e.metaKey || e.altKey) return;
      var t = e.target;
      if (!t) return;
      var tag = (t.tagName || '').toLowerCase();
      if (tag === 'input' || tag === 'textarea' || t.isContentEditable) return;
      if (!input) return;
      e.preventDefault();
      input.focus();
      input.select();
    });

    // ⌘S / Ctrl+S inside the pane: blur the focused control so its existing
    // auto-save-on-blur fires immediately (matches the spec's ⌘S semantics).
    document.addEventListener('keydown', function (e) {
      if (e.key !== 's' && e.key !== 'S') return;
      if (!(e.metaKey || e.ctrlKey)) return;
      var ae = document.activeElement;
      if (ae && pane.contains(ae) && /^(input|select|textarea)$/i.test(ae.tagName)) {
        e.preventDefault();
        ae.blur();
      }
    });

    // ---- Mobile hamburger ------------------------------------------------

    function setNavOpen(open) {
      if (!rail || !toggle) return;
      rail.setAttribute('data-nav-open', open ? 'true' : 'false');
      toggle.setAttribute('aria-expanded', open ? 'true' : 'false');
    }
    if (toggle) {
      toggle.addEventListener('click', function () {
        setNavOpen(rail.getAttribute('data-nav-open') !== 'true');
      });
    }

    // ---- settings.changed cross-tab SSE consumer ------------------------

    var lastLocalChange = 0;
    document.body.addEventListener('htmx:afterRequest', function () { lastLocalChange = Date.now(); });
    pane.addEventListener('submit', function () { lastLocalChange = Date.now(); }, true);

    function paneHasFocus() {
      var ae = document.activeElement;
      return !!(ae && pane.contains(ae) && /^(input|select|textarea|button)$/i.test(ae.tagName));
    }

    var refreshTimer = null;
    document.body.addEventListener('sse:settings.changed', function () {
      if (refreshTimer) clearTimeout(refreshTimer);
      refreshTimer = setTimeout(function () {
        if (paneHasFocus() || (Date.now() - lastLocalChange) < 2000) return;
        // FLAGGED (M55 #1339, out of scope for items 1+6): this is a cross-TAB
        // refresh driven by the settings.changed SSE event, which carries no
        // per-section granularity -- we cannot know which section a *different*
        // tab changed, so a targeted swRefreshSettingsSection() is not possible
        // here. It stays a full reload for now (suppressed while this tab is
        // editing). A future change could have the SSE event name the changed
        // section so this path can target it too.
        window.location.reload();
      }, 600);
    });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', swInitNextSettings);
  } else {
    swInitNextSettings();
  }

  window.swNextSettings = { init: swInitNextSettings };
})();
