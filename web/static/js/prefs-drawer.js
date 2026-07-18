// Preferences flyout drawer controller for the next/ UI (M55 #1774).
//
// Manages open/close state, focus trap, Esc close, and keyboard trigger (,).
// All preference persistence goes through window.swPreferences (preferences.js);
// this module owns only the drawer chrome and the artist-detail layout card.
//
// Public API (exposed as window.swPrefsDrawer):
//   open()            -- open the drawer and focus the first control
//   close()           -- close the drawer and return focus to the trigger
//   toggle()          -- toggle open/closed
//   initLayoutCard()  -- wire SortableJS drag + arrow-button keyboard fallback
//                        for the artist-detail section order card
(function () {
  'use strict';

  var DRAWER_ID = 'sw-prefs-drawer';
  var SCRIM_ID  = 'sw-prefs-scrim';

  // Focusable selector used for focus-trap logic.
  var FOCUSABLE = [
    'a[href]',
    'button:not([disabled])',
    'input:not([disabled])',
    'select:not([disabled])',
    'textarea:not([disabled])',
    '[tabindex]:not([tabindex="-1"])'
  ].join(', ');

  var _lastFocusedBefore = null;
  var _keydownHandler   = null;

  // --- Internal helpers ----------------------------------------------------

  function getDrawer() { return document.getElementById(DRAWER_ID); }
  function getScrim()  { return document.getElementById(SCRIM_ID); }

  function isOpen() {
    var d = getDrawer();
    return d ? d.classList.contains('sw-prefs-drawer--open') : false;
  }

  // Build a focus-trap keydown handler that cycles Tab within the drawer.
  function makeTrapHandler(drawer) {
    return function (ev) {
      if (ev.key === 'Escape') {
        ev.preventDefault();
        close();
        return;
      }
      if (ev.key !== 'Tab') return;
      var focusable = Array.prototype.slice.call(drawer.querySelectorAll(FOCUSABLE));
      if (!focusable.length) { ev.preventDefault(); return; }
      var first = focusable[0];
      var last  = focusable[focusable.length - 1];
      if (ev.shiftKey) {
        if (document.activeElement === first) {
          ev.preventDefault();
          last.focus();
        }
      } else {
        if (document.activeElement === last) {
          ev.preventDefault();
          first.focus();
        }
      }
    };
  }

  // --- Public API ----------------------------------------------------------

  function open() {
    // If the drawer hasn't been loaded yet (lazy HTMX mount), dispatch the
    // sw:prefs-open event so the #sw-prefs-mount container fetches the content.
    // After HTMX swaps in the real drawer, wire() is called via htmx:afterSwap;
    // the next open() call then finds #sw-prefs-drawer and proceeds normally.
    var drawer = getDrawer();
    if (!drawer) {
      _lastFocusedBefore = document.activeElement;
      document.body.dispatchEvent(new CustomEvent('sw:prefs-open'));
      return;
    }

    var scrim  = getScrim();
    if (isOpen()) return;

    _lastFocusedBefore = document.activeElement;

    drawer.classList.add('sw-prefs-drawer--open');
    drawer.setAttribute('aria-hidden', 'false');
    drawer.removeAttribute('inert');

    // Push the app canvas right while open (wide viewports only; the CSS rule
    // is media-queried) so the live preview stays unobstructed beside the
    // drawer (maintainer call 2026-06-09).
    document.documentElement.setAttribute('data-prefs-open', '');

    if (scrim) {
      scrim.classList.add('sw-prefs-scrim--visible');
    }

    // Update trigger aria-expanded.
    Array.prototype.slice.call(document.querySelectorAll('[data-sw-prefs-trigger]')).forEach(function (el) {
      el.setAttribute('aria-expanded', 'true');
    });

    // Focus first focusable element inside the drawer.
    var focusable = drawer.querySelectorAll(FOCUSABLE);
    if (focusable.length) {
      focusable[0].focus();
    }

    // Install focus trap.
    _keydownHandler = makeTrapHandler(drawer);
    drawer.addEventListener('keydown', _keydownHandler);
  }

  function close() {
    var drawer = getDrawer();
    var scrim  = getScrim();
    if (!drawer || !isOpen()) return;

    drawer.classList.remove('sw-prefs-drawer--open');
    drawer.setAttribute('aria-hidden', 'true');
    drawer.setAttribute('inert', '');

    document.documentElement.removeAttribute('data-prefs-open');

    if (scrim) {
      scrim.classList.remove('sw-prefs-scrim--visible');
    }

    // Update trigger aria-expanded.
    Array.prototype.slice.call(document.querySelectorAll('[data-sw-prefs-trigger]')).forEach(function (el) {
      el.setAttribute('aria-expanded', 'false');
    });

    // Remove focus trap.
    if (_keydownHandler) {
      drawer.removeEventListener('keydown', _keydownHandler);
      _keydownHandler = null;
    }

    // Return focus to the element that opened the drawer.
    if (_lastFocusedBefore && typeof _lastFocusedBefore.focus === 'function') {
      _lastFocusedBefore.focus();
      _lastFocusedBefore = null;
    }
  }

  function toggle() {
    if (isOpen()) { close(); } else { open(); }
  }

  // --- Artist detail layout card ------------------------------------------
  //
  // Wires SortableJS drag reordering and up/down arrow-button keyboard
  // fallback for the artist-detail section order list inside the drawer.
  // Persists via PATCH /api/v1/preferences to artist_detail_section_order
  // and artist_detail_hidden_sections.

  // cachedArray reads a stored-preference array out of window.swPreferences'
  // sessionStorage cache (preferences.js). Returns [] when the cache, the key,
  // or swPreferences itself is unavailable, never throws.
  function cachedArray(key) {
    var cache = window.swPreferences && typeof window.swPreferences.getCache === 'function'
      ? window.swPreferences.getCache()
      : null;
    var val = cache && cache[key];
    return Array.isArray(val) ? val : [];
  }

  // mergeAbsent appends any id from storedIDs that is not already in liveIDs,
  // preserving storedIDs' relative order.
  //
  // The drawer's layout list only ever renders the fixed default section set
  // (orderedPrefsLayoutSections in prefs_drawer.templ), so a section the
  // ON-PAGE controls manage but the drawer does not -- "debug", which only
  // exists in the DOM when show_platform_debug is on (section-layout.js) --
  // is never a row here. Without this merge, saving from the drawer would
  // silently drop that section's stored order/collapsed state (#2108).
  function mergeAbsent(liveIDs, storedIDs) {
    var seen = {};
    liveIDs.forEach(function (id) { seen[id] = true; });
    var merged = liveIDs.slice();
    storedIDs.forEach(function (id) {
      if (!seen[id]) {
        merged.push(id);
        seen[id] = true;
      }
    });
    return merged;
  }

  // applyLiveLayout forwards an order/collapsed pair to section-layout.js's
  // live-apply API so a layout change made in the drawer takes effect
  // immediately on an already-open artist-detail page (#2110), instead of
  // only being visible after the next full page load. Gated inside
  // applyLayout itself to a no-op when the artist-detail section container
  // is not present (e.g. drawer opened on a different page).
  function applyLiveLayout(order, collapsed) {
    if (window.swArtistSectionLayout && typeof window.swArtistSectionLayout.applyLayout === 'function') {
      window.swArtistSectionLayout.applyLayout(order, collapsed);
    } else if (window.console) {
      console.error('swPrefsDrawer: window.swArtistSectionLayout.applyLayout unavailable; layout change will not live-apply on this page');
    }
  }

  function saveSectionOrder() {
    var list = document.getElementById('sw-prefs-layout-list');
    if (!list || !window.swPreferences) return;
    var rows = Array.prototype.slice.call(list.querySelectorAll('[data-section-id]'));
    var liveOrder  = rows.map(function (r) { return r.getAttribute('data-section-id'); });
    var liveHidden = rows.filter(function (r) { return r.getAttribute('data-hidden') === 'true'; })
                     .map(function (r) { return r.getAttribute('data-section-id'); });
    var liveCollapsed = rows.filter(function (r) { return r.getAttribute('data-collapsed') === 'true'; })
                        .map(function (r) { return r.getAttribute('data-section-id'); });

    // Merge in any stored id absent from this drawer's fixed row set (#2108).
    var order = mergeAbsent(liveOrder, cachedArray('artist_detail_section_order'));
    var hidden = mergeAbsent(liveHidden, cachedArray('artist_detail_hidden_sections'));
    var collapsed = mergeAbsent(liveCollapsed, cachedArray('artist_detail_collapsed_sections'));

    applyLiveLayout(order, collapsed);

    var bp = (function () {
      var el = document.querySelector('meta[name="htmx-base-path"]');
      return el ? el.content : '';
    })();
    var csrf;
    if (typeof window.swCsrfToken === 'function') {
      csrf = window.swCsrfToken();
    } else {
      console.error("swCsrfToken unavailable - preferences.js may have failed to load; state-changing requests will 403");
      csrf = '';
    }

    fetch(bp + '/api/v1/preferences', {
      method: 'PATCH',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
      body: JSON.stringify({
        artist_detail_section_order:      order,
        artist_detail_hidden_sections:    hidden,
        artist_detail_collapsed_sections: collapsed
      })
    }).then(function (r) {
      if (!r.ok && window.console) {
        console.warn('swPrefsDrawer: failed to save section layout (HTTP ' + r.status + ')');
      }
    }).catch(function (e) {
      if (window.console) {
        console.warn('swPrefsDrawer: network error saving section layout', e);
      }
    });
  }

  function initLayoutCard() {
    var list = document.getElementById('sw-prefs-layout-list');
    if (!list) return;

    // SortableJS drag reordering.
    if (typeof Sortable !== 'undefined') {
      Sortable.create(list, {
        handle: '.sw-prefs-layout-handle',
        animation: 150,
        ghostClass: 'sortable-ghost',
        chosenClass: 'sortable-chosen',
        onEnd: saveSectionOrder
      });
    }

    // Arrow-button keyboard fallback: up/down move the row within the list.
    list.addEventListener('click', function (ev) {
      var btn = ev.target.closest('.sw-prefs-layout-btn');
      if (!btn) return;
      var row = btn.closest('[data-section-id]');
      if (!row) return;

      var action = btn.getAttribute('data-action');

      if (action === 'move-up') {
        var prev = row.previousElementSibling;
        if (prev) {
          list.insertBefore(row, prev);
          btn.focus();
          saveSectionOrder();
        }
      } else if (action === 'move-down') {
        var next = row.nextElementSibling;
        if (next) {
          list.insertBefore(next, row);
          btn.focus();
          saveSectionOrder();
        }
      } else if (action === 'toggle-visibility') {
        var current = row.getAttribute('data-hidden') === 'true';
        row.setAttribute('data-hidden', current ? 'false' : 'true');
        var nameEl = row.querySelector('.sw-prefs-layout-name');
        var sectionName = nameEl ? nameEl.textContent.trim() : '';
        btn.setAttribute('aria-label', current ? ('Hide ' + sectionName + ' section') : ('Show ' + sectionName + ' section'));
        var icon = btn.querySelector('.sw-prefs-layout-eye-icon');
        if (icon) {
          icon.removeAttribute('aria-label');
        }
        saveSectionOrder();
      } else if (action === 'toggle-collapsed') {
        var wasCollapsed = row.getAttribute('data-collapsed') === 'true';
        row.setAttribute('data-collapsed', wasCollapsed ? 'false' : 'true');
        btn.setAttribute('aria-pressed', wasCollapsed ? 'false' : 'true');
        var cName = row.querySelector('.sw-prefs-layout-name');
        var cSectionName = cName ? cName.textContent.trim() : '';
        btn.setAttribute('aria-label', wasCollapsed
          ? (btn.getAttribute('data-label-collapse') || ('Collapse ' + cSectionName))
          : (btn.getAttribute('data-label-expand')   || ('Expand '   + cSectionName)));
        saveSectionOrder();
      }
    });
  }

  // --- Preference tile (radiogroup) controls -------------------------------
  //
  // Roving tabindex for tile groups. Each [data-prefs-tiles] is a radiogroup;
  // arrow keys move selection. Activate with Space/Enter.

  function initTiles() {
    Array.prototype.slice.call(document.querySelectorAll('[data-prefs-tiles]')).forEach(function (group) {
      var tiles = Array.prototype.slice.call(group.querySelectorAll('.sw-prefs-tile'));
      if (!tiles.length) return;

      // Ensure initial tabindex state: only the selected tile is tabbable.
      tiles.forEach(function (t) {
        t.setAttribute('tabindex', t.getAttribute('aria-checked') === 'true' ? '0' : '-1');
      });

      group.addEventListener('keydown', function (ev) {
        if (ev.key !== 'ArrowLeft' && ev.key !== 'ArrowRight' &&
            ev.key !== 'ArrowUp' && ev.key !== 'ArrowDown' &&
            ev.key !== 'Home' && ev.key !== 'End') return;
        var current = tiles.indexOf(document.activeElement);
        if (current === -1) return;
        ev.preventDefault();
        var next = current;
        if (ev.key === 'ArrowRight' || ev.key === 'ArrowDown') { next = (current + 1) % tiles.length; }
        if (ev.key === 'ArrowLeft'  || ev.key === 'ArrowUp')   { next = (current - 1 + tiles.length) % tiles.length; }
        if (ev.key === 'Home')       { next = 0; }
        if (ev.key === 'End')        { next = tiles.length - 1; }
        if (next !== current) {
          tiles[current].setAttribute('tabindex', '-1');
          tiles[next].setAttribute('tabindex', '0');
          tiles[next].focus();
          // Move-and-select: activate immediately (radio pattern).
          tiles[next].click();
        }
      });
    });
  }

  // --- Segmented control (radiogroup) -------------------------------------
  //
  // Same roving-tabindex + arrow-key pattern as tiles, but for .sw-prefs-seg-btn.

  function onSegClick(btn) {
    var group = btn.closest('[data-prefs-seg]');
    if (!group) return;
    var prefKey = group.getAttribute('data-prefs-seg');
    var value   = btn.getAttribute('data-value');
    if (!prefKey || !value) return;

    var btns = Array.prototype.slice.call(group.querySelectorAll('.sw-prefs-seg-btn'));
    btns.forEach(function (b) {
      var isThis = b === btn;
      b.setAttribute('aria-checked', isThis ? 'true' : 'false');
      b.setAttribute('tabindex', isThis ? '0' : '-1');
    });

    if (window.swPreferences) {
      window.swPreferences.set(prefKey, value).catch(function () {
        if (window.showToast) { showToast('Failed to save preference'); }
      });
    }
  }

  function initSegs() {
    Array.prototype.slice.call(document.querySelectorAll('[data-prefs-seg]')).forEach(function (group) {
      var btns = Array.prototype.slice.call(group.querySelectorAll('.sw-prefs-seg-btn'));
      if (!btns.length) return;

      // Ensure initial tabindex state.
      var hasSelected = btns.some(function (b) { return b.getAttribute('aria-checked') === 'true'; });
      btns.forEach(function (b, i) {
        if (!hasSelected && i === 0) {
          b.setAttribute('tabindex', '0');
        } else {
          b.setAttribute('tabindex', b.getAttribute('aria-checked') === 'true' ? '0' : '-1');
        }
        b.addEventListener('click', function () { onSegClick(b); });
      });

      group.addEventListener('keydown', function (ev) {
        if (ev.key !== 'ArrowLeft' && ev.key !== 'ArrowRight' &&
            ev.key !== 'Home' && ev.key !== 'End') return;
        var current = btns.indexOf(document.activeElement);
        if (current === -1) return;
        ev.preventDefault();
        var next = current;
        if (ev.key === 'ArrowRight') { next = (current + 1) % btns.length; }
        if (ev.key === 'ArrowLeft')  { next = (current - 1 + btns.length) % btns.length; }
        if (ev.key === 'Home')       { next = 0; }
        if (ev.key === 'End')        { next = btns.length - 1; }
        if (next !== current) {
          btns[current].setAttribute('tabindex', '-1');
          btns[next].setAttribute('tabindex', '0');
          btns[next].focus();
          btns[next].click();
        }
      });
    });
  }

  // Handle clicks on individual tiles.
  function onTileClick(tile) {
    var group = tile.closest('[data-prefs-tiles]');
    if (!group) return;
    var prefKey = group.getAttribute('data-prefs-tiles');
    var value   = tile.getAttribute('data-value');
    if (!prefKey || !value) return;

    // Optimistic: update aria-checked + tabindex in the group.
    var tiles = Array.prototype.slice.call(group.querySelectorAll('.sw-prefs-tile'));
    tiles.forEach(function (t) {
      var isThis = t === tile;
      t.setAttribute('aria-checked', isThis ? 'true' : 'false');
      t.setAttribute('tabindex', isThis ? '0' : '-1');
    });

    // Persist via swPreferences.
    if (window.swPreferences) {
      window.swPreferences.set(prefKey, value).catch(function () {
        if (window.showToast) { showToast('Failed to save preference'); }
      });
    }
  }

  // Handle toggle (role=switch) clicks.
  function onToggleClick(btn) {
    var prefKey = btn.getAttribute('data-pref-key');
    if (!prefKey) return;
    var current  = btn.getAttribute('aria-checked') === 'true';
    var newVal   = !current;
    // Per-toggle on/off vocabulary: data-pref-on/data-pref-off override the
    // default "true"/"false" strings. This lets toggles like kbd_hints emit
    // "show"/"hide" while aria-checked remains the boolean switch state.
    var onValue  = btn.getAttribute('data-pref-on')  || 'true';
    var offValue = btn.getAttribute('data-pref-off') || 'false';
    var valStr   = newVal ? onValue : offValue;

    btn.setAttribute('aria-checked', String(newVal));
    var knob = btn.querySelector('.sw-prefs-toggle-knob');
    // Knob slides via CSS transition on the toggle class.

    if (window.swPreferences) {
      window.swPreferences.set(prefKey, valStr).catch(function () {
        // Rollback on failure.
        btn.setAttribute('aria-checked', String(current));
        if (window.showToast) { showToast('Failed to save preference'); }
      });
    }
  }

  // --- Filter/search -------------------------------------------------------
  //
  // Filter behavior: rows that do not match are hidden; groups containing a
  // match auto-expand; groups with no match collapse and hide. Clearing the
  // input restores the exact expansion state that existed before the first
  // character was typed (non-destructive, snapshot-and-restore).

  function initSearch() {
    var input = document.getElementById('sw-prefs-search');
    if (!input) return;

    var groupsContainer = document.getElementById('sw-prefs-groups');
    var emptyEl = document.getElementById('sw-prefs-search-empty');
    if (!groupsContainer) return;

    // Snapshot expansion state before search so we can restore it exactly.
    function getExpansionState() {
      var state = {};
      Array.prototype.slice.call(groupsContainer.querySelectorAll('[data-group-id]')).forEach(function (trigger) {
        state[trigger.getAttribute('data-group-id')] = trigger.getAttribute('aria-expanded') !== 'false';
      });
      return state;
    }

    var savedState = null;

    input.addEventListener('input', function () {
      var query = input.value.trim().toLowerCase();

      if (!query) {
        // Clear filter: restore every row and group to its prior state.
        Array.prototype.slice.call(groupsContainer.querySelectorAll('.sw-prefs-row, .sw-prefs-layout-row')).forEach(function (row) {
          row.hidden = false;
        });
        if (savedState) {
          Array.prototype.slice.call(groupsContainer.querySelectorAll('[data-group-id]')).forEach(function (trigger) {
            var id = trigger.getAttribute('data-group-id');
            var body = document.getElementById('group-body-' + id);
            var expanded = savedState[id];
            trigger.setAttribute('aria-expanded', expanded ? 'true' : 'false');
            if (body) {
              body.hidden = !expanded;
              body.removeAttribute('style'); // clear any display:none we may have set
            }
          });
          // Restore group containers themselves (they were never hidden in this path,
          // but be explicit).
          Array.prototype.slice.call(groupsContainer.querySelectorAll('.sw-prefs-group')).forEach(function (g) {
            g.hidden = false;
          });
          savedState = null;
        }
        if (emptyEl) { emptyEl.hidden = true; }
        return;
      }

      // Snapshot state on the FIRST character entered (before we mutate anything).
      if (!savedState) {
        savedState = getExpansionState();
      }

      var anyGroupVisible = false;

      Array.prototype.slice.call(groupsContainer.querySelectorAll('[data-group-id]')).forEach(function (trigger) {
        var groupId  = trigger.getAttribute('data-group-id');
        var body     = document.getElementById('group-body-' + groupId);
        var groupEl  = trigger.closest('.sw-prefs-group');

        if (!body) return;

        // Check each row within this group for a substring match across all text.
        var hasMatch = false;
        Array.prototype.slice.call(body.querySelectorAll('.sw-prefs-row, .sw-prefs-layout-row')).forEach(function (row) {
          var text = row.textContent.toLowerCase();
          if (text.indexOf(query) !== -1) {
            row.hidden = false;
            hasMatch = true;
          } else {
            row.hidden = true;
          }
        });

        if (hasMatch) {
          // Expand so the matching rows are visible.
          trigger.setAttribute('aria-expanded', 'true');
          body.hidden = false;
          if (groupEl) { groupEl.hidden = false; }
          anyGroupVisible = true;
        } else {
          // Collapse and hide the entire group.
          trigger.setAttribute('aria-expanded', 'false');
          body.hidden = true;
          if (groupEl) { groupEl.hidden = true; }
        }
      });

      // Show/hide empty state.
      if (emptyEl) {
        emptyEl.hidden = anyGroupVisible;
        if (!anyGroupVisible) {
          var tmpl = emptyEl.dataset.emptyTmpl || 'No preferences match “%s”';
          emptyEl.textContent = tmpl.replace('%s', input.value);
        }
      }
    });
  }

  // --- Accordion group toggle ----------------------------------------------

  function toggleGroup(trigger) {
    var groupId = trigger.getAttribute('data-group-id');
    var body    = document.getElementById('group-body-' + groupId);
    if (!body) return;
    var expanded = trigger.getAttribute('aria-expanded') !== 'false';
    trigger.setAttribute('aria-expanded', expanded ? 'false' : 'true');
    body.hidden = expanded;
  }

  // --- Reset artist detail layout order and visibility ---------------------

  function resetLayout() {
    var bp = (function () {
      var el = document.querySelector('meta[name="htmx-base-path"]');
      return el ? el.content : '';
    })();
    var csrf;
    if (typeof window.swCsrfToken === 'function') {
      csrf = window.swCsrfToken();
    } else {
      console.error("swCsrfToken unavailable - preferences.js may have failed to load; state-changing requests will 403");
      csrf = '';
    }

    // Default section order - mirrors defaultPrefsLayoutSections in prefs_drawer.templ.
    var DEFAULT_ORDER = ['metadata', 'artwork', 'findings', 'providers', 'discography', 'identifiers'];

    // Reorder rows and unhide all entirely in-place. The drawer element is never
    // replaced, so open-state, scrim visibility, and all event listeners are
    // unaffected. list.appendChild(row) moves an existing child to the end, which
    // is the correct re-sort primitive when iterating the desired order.
    var list = document.getElementById('sw-prefs-layout-list');
    if (list) {
      var rowMap = {};
      Array.prototype.slice.call(list.querySelectorAll('[data-section-id]')).forEach(function (r) {
        rowMap[r.getAttribute('data-section-id')] = r;
      });
      DEFAULT_ORDER.forEach(function (id) {
        var row = rowMap[id];
        if (!row) { return; }
        row.setAttribute('data-hidden', 'false');
        row.setAttribute('data-collapsed', 'false');
        var collapseBtn = row.querySelector('[data-action="toggle-collapsed"]');
        if (collapseBtn) {
          collapseBtn.setAttribute('aria-pressed', 'false');
          var clb = collapseBtn.getAttribute('data-label-collapse');
          if (clb) { collapseBtn.setAttribute('aria-label', clb); }
        }
        list.appendChild(row);
      });
    }

    // Live-apply the reset to an already-open artist-detail page (#2110).
    applyLiveLayout(DEFAULT_ORDER, []);

    // Persist reset to server.
    fetch(bp + '/api/v1/preferences', {
      method: 'PATCH',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
      body: JSON.stringify({ artist_detail_section_order: DEFAULT_ORDER, artist_detail_hidden_sections: [], artist_detail_collapsed_sections: [] })
    }).then(function (r) {
      if (r.ok && window.showSuccessToast) { showSuccessToast('Layout reset'); }
      else if (!r.ok && window.showToast) { showToast('Failed to reset layout'); }
    }).catch(function () {
      if (window.showToast) { showToast('Failed to reset layout'); }
    });
  }

  // --- Reset to defaults ---------------------------------------------------

  function resetAll() {
    if (!window.swPreferences) return;
    var DEFAULTS = {
      theme:                'dark',
      sidebar_state:        'full',
      content_width:        'narrow',
      font_family:          'inter',
      font_size:            'medium',
      letter_spacing:       'normal',
      thumbnail_size:       'medium',
      reduced_motion:       'system',
      lite_mode:            'off',
      notification_enabled:  'true',
      auto_fetch_images:     'false',
      show_platform_debug:   'false',
      bg_opacity:            '85',
      page_size:             '50',
      density:               'comfortable',
      mono_font:             'jetbrains',
      kbd_hints:             'show',
      language:              'en'
    };
    var bp = (function () {
      var el = document.querySelector('meta[name="htmx-base-path"]');
      return el ? el.content : '';
    })();
    var csrf;
    if (typeof window.swCsrfToken === 'function') {
      csrf = window.swCsrfToken();
    } else {
      console.error("swCsrfToken unavailable - preferences.js may have failed to load; state-changing requests will 403");
      csrf = '';
    }

    // Apply visual/CSS defaults immediately (theme, font, density, etc.).
    window.swPreferences.applyAll(DEFAULTS);

    // Update every drawer control in-place. The drawer element is never replaced,
    // so open-state, scrim visibility, content position, and all event listeners
    // are unaffected by construction.
    var drawer = getDrawer();
    if (drawer) {
      // Tile groups: aria-checked + roving tabindex.
      ['theme', 'sidebar_state', 'content_width', 'thumbnail_size',
       'font_family', 'mono_font', 'letter_spacing'].forEach(function (prefKey) {
        var group = drawer.querySelector('[data-prefs-tiles="' + prefKey + '"]');
        if (!group) { return; }
        var val = DEFAULTS[prefKey];
        Array.prototype.slice.call(group.querySelectorAll('.sw-prefs-tile')).forEach(function (t) {
          var sel = t.getAttribute('data-value') === val;
          t.setAttribute('aria-checked', sel ? 'true' : 'false');
          t.setAttribute('tabindex',     sel ? '0'    : '-1');
        });
      });

      // Segmented controls: aria-checked + roving tabindex.
      ['density', 'reduced_motion', 'lite_mode'].forEach(function (prefKey) {
        var group = drawer.querySelector('[data-prefs-seg="' + prefKey + '"]');
        if (!group) { return; }
        var val = DEFAULTS[prefKey];
        Array.prototype.slice.call(group.querySelectorAll('.sw-prefs-seg-btn')).forEach(function (b) {
          var sel = b.getAttribute('data-value') === val;
          b.setAttribute('aria-checked', sel ? 'true' : 'false');
          b.setAttribute('tabindex',     sel ? '0'    : '-1');
        });
      });

      // Toggle switches (role=switch). kbd_hints uses 'show' as its on-value;
      // the others use 'true'.
      [
        { key: 'notification_enabled', on: 'true' },
        { key: 'auto_fetch_images',    on: 'true' },
        { key: 'show_platform_debug',  on: 'true' },
        { key: 'kbd_hints',            on: 'show' }
      ].forEach(function (cfg) {
        var btn = drawer.querySelector('.sw-prefs-toggle[data-pref-key="' + cfg.key + '"]');
        if (!btn) { return; }
        btn.setAttribute('aria-checked', DEFAULTS[cfg.key] === cfg.on ? 'true' : 'false');
      });

      // Font-size discrete slider (0=small 1=medium 2=large 3=x-large 4=xx-large).
      var fontSizeSlider = drawer.querySelector('#pref-d-font-size-slider');
      if (fontSizeSlider) {
        var FS_STOPS = { small: 0, medium: 1, large: 2, 'x-large': 3, 'xx-large': 4 };
        var fsStop  = FS_STOPS[DEFAULTS.font_size] !== undefined ? FS_STOPS[DEFAULTS.font_size] : 1;
        fontSizeSlider.value = String(fsStop);
        // data-stop-names always carries the 5 localized stop labels (server
        // contract, prefs_drawer.templ), so fsLabels[fsStop] (stop in 0..4) is
        // always defined -- no raw-enum fallback needed. The || '' on the
        // attribute guards a missing attribute from throwing on .split; the
        // || '' on the label keeps that same defensive path from surfacing a
        // literal "undefined" in the value/aria-valuetext if the contract is
        // ever violated (degrades to blank, not the raw enum).
        var fsLabels = (fontSizeSlider.getAttribute('data-stop-names') || '').split('|');
        var fsLabel  = fsLabels[fsStop] || '';
        fontSizeSlider.setAttribute('aria-valuetext', fsLabel);
        var fsValueEl = drawer.querySelector('#pref-d-font-size-value');
        if (fsValueEl) { fsValueEl.textContent = fsLabel; }
      }

      // Background opacity slider + live value label.
      var bgSlider = drawer.querySelector('#pref-d-bg-opacity');
      if (bgSlider) {
        bgSlider.value = DEFAULTS.bg_opacity;
        var bgLabel = drawer.querySelector('#pref-d-bg-opacity-value');
        if (bgLabel) { bgLabel.textContent = DEFAULTS.bg_opacity + '%'; }
        // Live-preview the default via the same self-contained path as the
        // slider (M55 #1773). The persisted reset happens in the single PATCH
        // below (it includes bg_opacity), so applySingle here only updates the
        // visible surface immediately. Fail loudly, never silently no-op.
        if (window.swPreferences && typeof window.swPreferences.applySingle === 'function') {
          window.swPreferences.applySingle('bg_opacity', DEFAULTS.bg_opacity);
        } else {
          console.error('prefs-drawer: window.swPreferences.applySingle unavailable; bg-opacity reset preview skipped');
        }
      }

      // Page size number input.
      var pageSizeInput = drawer.querySelector('#pref-d-page-size');
      if (pageSizeInput) { pageSizeInput.value = DEFAULTS.page_size; }

      // Language select.
      var langSelect = drawer.querySelector('#pref-d-language');
      if (langSelect) { langSelect.value = DEFAULTS.language; }

      // Artist-detail layout list: reorder to default, unhide all.
      var DEFAULT_ORDER = ['metadata', 'artwork', 'findings', 'providers', 'discography', 'identifiers'];
      var list = document.getElementById('sw-prefs-layout-list');
      if (list) {
        var rowMap = {};
        Array.prototype.slice.call(list.querySelectorAll('[data-section-id]')).forEach(function (r) {
          rowMap[r.getAttribute('data-section-id')] = r;
        });
        DEFAULT_ORDER.forEach(function (id) {
          var row = rowMap[id];
          if (!row) { return; }
          row.setAttribute('data-hidden', 'false');
          row.setAttribute('data-collapsed', 'false');
          var collapseBtn = row.querySelector('[data-action="toggle-collapsed"]');
          if (collapseBtn) {
            collapseBtn.setAttribute('aria-pressed', 'false');
            var clb = collapseBtn.getAttribute('data-label-collapse');
            if (clb) { collapseBtn.setAttribute('aria-label', clb); }
          }
          list.appendChild(row);
        });
      }

      // Live-apply the reset to an already-open artist-detail page (#2110).
      applyLiveLayout(DEFAULT_ORDER, []);
    }

    // Persist all scalar defaults + layout reset in a single PATCH.
    fetch(bp + '/api/v1/preferences', {
      method: 'PATCH',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
      body: JSON.stringify({
        theme:                          DEFAULTS.theme,
        sidebar_state:                  DEFAULTS.sidebar_state,
        content_width:                  DEFAULTS.content_width,
        font_family:                    DEFAULTS.font_family,
        font_size:                      DEFAULTS.font_size,
        letter_spacing:                 DEFAULTS.letter_spacing,
        thumbnail_size:                 DEFAULTS.thumbnail_size,
        reduced_motion:                 DEFAULTS.reduced_motion,
        lite_mode:                      DEFAULTS.lite_mode,
        notification_enabled:           DEFAULTS.notification_enabled,
        auto_fetch_images:              DEFAULTS.auto_fetch_images,
        show_platform_debug:            DEFAULTS.show_platform_debug,
        bg_opacity:                     DEFAULTS.bg_opacity,
        page_size:                      DEFAULTS.page_size,
        density:                        DEFAULTS.density,
        mono_font:                      DEFAULTS.mono_font,
        kbd_hints:                      DEFAULTS.kbd_hints,
        language:                       DEFAULTS.language,
        artist_detail_section_order:    ['metadata', 'artwork', 'findings', 'providers', 'discography', 'identifiers'],
        artist_detail_hidden_sections:  [],
        artist_detail_collapsed_sections: []
      })
    }).then(function (r) {
      if (r.ok) {
        if (window.showSuccessToast) { showSuccessToast('Preferences reset to defaults'); }
      } else {
        if (window.showToast) { showToast('Failed to reset preferences'); }
      }
    }).catch(function () {
      if (window.showToast) { showToast('Failed to reset preferences'); }
    });
  }

  // --- Global keyboard shortcut: , opens the drawer -----------------------

  function installKeyboardShortcut() {
    document.addEventListener('keydown', function (ev) {
      if (ev.defaultPrevented || ev.metaKey || ev.ctrlKey || ev.altKey) return;
      var tag = (ev.target.tagName || '').toUpperCase();
      var isTyping = (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' ||
                      (ev.target.isContentEditable));
      if (isTyping) return;
      if (ev.key === ',') {
        ev.preventDefault();
        toggle();
      }
    });
  }

  // --- DOM event wiring (called once on DOMContentLoaded or init) ----------

  // wireTriggers binds click->toggle on every [data-sw-prefs-trigger] element.
  // Called unconditionally at init so the sidebar Preferences link intercepts
  // navigation BEFORE the drawer is lazy-loaded. Without this separation the
  // link falls through to href="/next/preferences" on first click because
  // wire() early-returns when the drawer has not been mounted yet.
  function wireTriggers() {
    Array.prototype.slice.call(document.querySelectorAll('[data-sw-prefs-trigger]')).forEach(function (el) {
      // Guard against double-binding on subsequent wireTriggers calls.
      if (el.dataset.swPrefsTriggerBound) return;
      el.dataset.swPrefsTriggerBound = '1';
      el.addEventListener('click', function (ev) {
        ev.preventDefault();
        toggle();
      });
    });
  }

  function wire() {
    var drawer = getDrawer();
    if (!drawer) return;

    // Idempotency guard: bind the drawer's controls exactly once per node.
    // wire() can be invoked more than once for the same mounted drawer -- the
    // htmx lazy-mount swaps in a fragment with several root nodes (scrim +
    // drawer + the ContextHelp <script>) via hx-swap="outerHTML", and htmx
    // fires htmx:afterSwap per swapped node, each one matching the
    // #sw-prefs-mount target guard in the afterSwap handler below. Without this
    // guard every control's listener was bound once per afterSwap, so a single
    // toggle click fired N identical PUTs and an immediate navigation could
    // abort the write -> the setting reverted on the next screen (#1798/#2037).
    // A freshly swapped drawer is a NEW node with no flag, so it still wires;
    // re-wiring the SAME node is a no-op. Mirrors wireTriggers()'s per-node
    // dataset guard.
    if (drawer.dataset.swPrefsWired) return;
    drawer.dataset.swPrefsWired = '1';

    // Tile clicks.
    Array.prototype.slice.call(drawer.querySelectorAll('.sw-prefs-tile')).forEach(function (t) {
      t.addEventListener('click', function () { onTileClick(t); });
    });

    // Toggle clicks.
    Array.prototype.slice.call(drawer.querySelectorAll('.sw-prefs-toggle')).forEach(function (b) {
      b.addEventListener('click', function () { onToggleClick(b); });
    });

    // Font-size discrete slider (5-stop: 0=small, 1=medium, 2=large, 3=x-large, 4=xx-large).
    // Display labels come localized from the server via data-stop-names
    // (pipe-joined in stop order) so no English is duplicated here.
    var FONT_SIZE_STOPS = ['small', 'medium', 'large', 'x-large', 'xx-large'];
    var fontSizeSlider = drawer.querySelector('#pref-d-font-size-slider');
    if (fontSizeSlider) {
      var FONT_SIZE_LABELS = (fontSizeSlider.getAttribute('data-stop-names') || '').split('|');
      var fontSizeValueLabel = drawer.querySelector('#pref-d-font-size-value');
      fontSizeSlider.addEventListener('input', function () {
        var stop = parseInt(fontSizeSlider.value, 10);
        // FONT_SIZE_LABELS is the 5-label data-stop-names contract; stop is
        // clamped to the slider's 0..4 range, so the label is always defined.
        // The || '' degrades to blank (not "undefined") if the contract is
        // ever violated, without reintroducing the raw-enum fallback.
        var label = FONT_SIZE_LABELS[stop] || '';
        fontSizeSlider.setAttribute('aria-valuetext', label);
        if (fontSizeValueLabel) { fontSizeValueLabel.textContent = label; }
      });
      fontSizeSlider.addEventListener('change', function () {
        var stop = parseInt(fontSizeSlider.value, 10);
        var value = FONT_SIZE_STOPS[stop] || 'medium';
        if (window.swPreferences) {
          window.swPreferences.set('font_size', value).catch(function () {
            if (window.showToast) { showToast('Failed to save font size'); }
          });
        }
      });
    }

    // Range slider: bg_opacity live update + save on change.
    //
    // The drawer is self-contained (M55 #1773): it drives bg_opacity through
    // window.swPreferences (preferences.js), which is present on every next/
    // page. It does NOT use the legacy standalone preferences page's inline
    // window.swUpdateBgOpacity/swSaveBgOpacity globals -- those are undefined
    // on next/ pages, so the previous typeof-guarded calls silently no-opped
    // and the slider did nothing but move its % label.
    //   - `input`  (dragging): live-preview via applySingle(), no persist.
    //   - `change` (release):  persist via set(), which also re-applies.
    // Each call is guarded, but the guard FAILS LOUDLY (console.error) instead
    // of silently no-opping, so a missing dependency is diagnosable, not
    // invisible.
    var bgSlider = drawer.querySelector('#pref-d-bg-opacity');
    if (bgSlider) {
      bgSlider.addEventListener('input', function () {
        var label = drawer.querySelector('#pref-d-bg-opacity-value');
        if (label) label.textContent = bgSlider.value + '%';
        if (window.swPreferences && typeof window.swPreferences.applySingle === 'function') {
          window.swPreferences.applySingle('bg_opacity', bgSlider.value);
        } else {
          console.error('prefs-drawer: window.swPreferences.applySingle unavailable; bg-opacity live preview disabled');
        }
      });
      bgSlider.addEventListener('change', function () {
        if (window.swPreferences && typeof window.swPreferences.set === 'function') {
          var requested = String(bgSlider.value);
          window.swPreferences.set('bg_opacity', requested).then(function (saved) {
            if (String(saved) !== requested) {
              bgSlider.value = String(saved);
              var label = drawer.querySelector('#pref-d-bg-opacity-value');
              if (label) { label.textContent = String(saved) + '%'; }
              if (window.showToast) { showToast('Failed to save background opacity'); }
            }
          }).catch(function () {
            if (window.showToast) { showToast('Failed to save background opacity'); }
          });
        } else {
          console.error('prefs-drawer: window.swPreferences.set unavailable; bg-opacity not persisted');
        }
      });
    }

    // Number input: page_size - save on blur (normalizes value per spec).
    var pageSizeInput = drawer.querySelector('#pref-d-page-size');
    if (pageSizeInput) {
      pageSizeInput.addEventListener('blur', function () {
        var n = parseInt(pageSizeInput.value, 10);
        if (isNaN(n)) n = 50;
        n = Math.max(10, Math.min(500, n));
        // Snap to nearest multiple of 5.
        n = Math.round(n / 5) * 5;
        pageSizeInput.value = n;
        if (typeof window.swSavePageSizePref === 'function') {
          window.swSavePageSizePref(String(n));
        } else if (window.swPreferences) {
          window.swPreferences.set('page_size', String(n)).catch(function () {
            if (window.showToast) { showToast('Failed to save page size'); }
          });
        }
      });
    }

    // Select: language.
    var langSelect = drawer.querySelector('#pref-d-language');
    if (langSelect) {
      langSelect.addEventListener('change', function () {
        if (window.swPreferences) {
          window.swPreferences.set('language', langSelect.value);
        }
      });
    }

    // Group accordion triggers.
    Array.prototype.slice.call(drawer.querySelectorAll('[data-group-id]')).forEach(function (trigger) {
      trigger.addEventListener('click', function () { toggleGroup(trigger); });
    });

    // Close button.
    var closeBtn = drawer.querySelector('.sw-prefs-drawer-close');
    if (closeBtn) {
      closeBtn.addEventListener('click', close);
    }

    // Footer reset button (resets all preferences to defaults).
    var footerResetBtn = drawer.querySelector('.sw-prefs-drawer-footer .sw-prefs-reset-btn');
    if (footerResetBtn) {
      footerResetBtn.addEventListener('click', resetAll);
    }

    // Layout-card reset button (resets section order and visibility only).
    var layoutResetBtn = drawer.querySelector('[data-action="reset-layout"]');
    if (layoutResetBtn) {
      layoutResetBtn.addEventListener('click', resetLayout);
    }

    // Wire any triggers that were added after the initial wireTriggers call
    // (e.g. a profile-menu item injected by HTMX after the drawer loads).
    wireTriggers();

    // (?) help popovers inside the drawer: hover/focus reveals via CSS;
    // click pins for touch users; Esc/outside-click dismisses.
    // Delegates to the existing sw-context-help global (swContextHelpToggle /
    // swContextHelpClose) which ContextHelpScript registers on the document.
    // No additional wiring needed here -- the global document listener catches
    // clicks on .sw-context-help-btn regardless of when the drawer is mounted.

    initSearch();
    initTiles();
    initSegs();
    initLayoutCard();
  }

  // --- Init ----------------------------------------------------------------

  // After HTMX swaps the lazy-loaded drawer into the DOM, wire it up and open it
  // (the user already clicked the trigger, so we should open immediately).
  // Only the initial lazy-mount uses this path. The reset functions (resetLayout,
  // resetAll) update the drawer in-place and never swap #sw-prefs-drawer.
  document.body.addEventListener('htmx:afterSwap', function (ev) {
    var target = ev.detail && ev.detail.target;
    if (target && target.id === 'sw-prefs-mount') {
      wire();
      open();
    }
  });

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', function () {
      // wireTriggers runs FIRST (unconditionally) so the sidebar Preferences
      // link is intercepted even before the drawer is lazy-loaded.
      wireTriggers();
      installKeyboardShortcut();
      wire();
    });
  } else {
    wireTriggers();
    installKeyboardShortcut();
    wire();
  }

  // Expose public API.
  window.swPrefsDrawer = {
    open: open,
    close: close,
    toggle: toggle,
    initLayoutCard: initLayoutCard,
    wire: wire
  };

})();
