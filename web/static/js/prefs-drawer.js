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
    document.querySelectorAll('[data-sw-prefs-trigger]').forEach(function (el) {
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
    document.querySelectorAll('[data-sw-prefs-trigger]').forEach(function (el) {
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

  function saveSectionOrder() {
    var list = document.getElementById('sw-prefs-layout-list');
    if (!list || !window.swPreferences) return;
    var rows = Array.prototype.slice.call(list.querySelectorAll('[data-section-id]'));
    var order  = rows.map(function (r) { return r.getAttribute('data-section-id'); });
    var hidden = rows.filter(function (r) { return r.getAttribute('data-hidden') === 'true'; })
                     .map(function (r) { return r.getAttribute('data-section-id'); });

    var bp = (function () {
      var el = document.querySelector('meta[name="htmx-base-path"]');
      return el ? el.content : '';
    })();
    var csrf = typeof window.swCsrfToken === 'function' ? window.swCsrfToken() : '';

    fetch(bp + '/api/v1/preferences', {
      method: 'PATCH',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
      body: JSON.stringify({
        artist_detail_section_order:   order,
        artist_detail_hidden_sections: hidden
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
        btn.setAttribute('aria-label', current ? 'Hide section' : 'Show section');
        var icon = btn.querySelector('.sw-prefs-layout-eye-icon');
        if (icon) {
          icon.removeAttribute('aria-label');
        }
        saveSectionOrder();
      }
    });
  }

  // --- Preference tile (radiogroup) controls -------------------------------
  //
  // Roving tabindex for tile groups. Each [data-prefs-tiles] is a radiogroup;
  // arrow keys move selection. Activate with Space/Enter.

  function initTiles() {
    document.querySelectorAll('[data-prefs-tiles]').forEach(function (group) {
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
    document.querySelectorAll('[data-prefs-seg]').forEach(function (group) {
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
    var current = btn.getAttribute('aria-checked') === 'true';
    var newVal  = !current;
    var valStr  = String(newVal);

    btn.setAttribute('aria-checked', valStr);
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
      groupsContainer.querySelectorAll('[data-group-id]').forEach(function (trigger) {
        state[trigger.getAttribute('data-group-id')] = trigger.getAttribute('aria-expanded') !== 'false';
      });
      return state;
    }

    var savedState = null;

    input.addEventListener('input', function () {
      var query = input.value.trim().toLowerCase();

      if (!query) {
        // Clear filter: restore every row and group to its prior state.
        groupsContainer.querySelectorAll('.sw-prefs-row').forEach(function (row) {
          row.hidden = false;
        });
        if (savedState) {
          groupsContainer.querySelectorAll('[data-group-id]').forEach(function (trigger) {
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
          groupsContainer.querySelectorAll('.sw-prefs-group').forEach(function (g) {
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

      groupsContainer.querySelectorAll('[data-group-id]').forEach(function (trigger) {
        var groupId  = trigger.getAttribute('data-group-id');
        var body     = document.getElementById('group-body-' + groupId);
        var groupEl  = trigger.closest('.sw-prefs-group');

        if (!body) return;

        // Check each row within this group for a substring match across all text.
        var hasMatch = false;
        body.querySelectorAll('.sw-prefs-row').forEach(function (row) {
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
          emptyEl.textContent = 'No preferences match “' + input.value + '”';
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
    var csrf = typeof window.swCsrfToken === 'function' ? window.swCsrfToken() : '';
    fetch(bp + '/api/v1/preferences', {
      method: 'PATCH',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
      body: JSON.stringify({ artist_detail_section_order: [], artist_detail_hidden_sections: [] })
    }).then(function (r) {
      if (r.ok && typeof htmx !== 'undefined') {
        // Close first so the scrim, aria-expanded triggers, and data-prefs-open are
        // all cleaned up before the outerHTML swap replaces #sw-prefs-drawer.
        // Without this the scrim stays visible and blocks reopening the drawer.
        close();
        htmx.ajax('GET', window.location.pathname, { target: '#sw-prefs-drawer', swap: 'outerHTML', select: '#sw-prefs-drawer' });
      }
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
      theme: 'dark',
      sidebar_state: 'full',
      content_width: 'narrow',
      font_family: 'inter',
      font_size: 'medium',
      letter_spacing: 'normal',
      thumbnail_size: 'medium',
      reduced_motion: 'system',
      lite_mode: 'off',
      notification_enabled: 'true',
      auto_fetch_images: 'false',
      bg_opacity: '85',
      page_size: '50',
      density: 'comfortable',
      mono_font: 'jetbrains',
      kbd_hints: 'show',
      language: 'en'
    };
    var bp = (function () {
      var el = document.querySelector('meta[name="htmx-base-path"]');
      return el ? el.content : '';
    })();
    var csrf = typeof window.swCsrfToken === 'function' ? window.swCsrfToken() : '';
    // PATCH all scalar defaults at once, then PATCH artist-detail layout reset.
    fetch(bp + '/api/v1/preferences', {
      method: 'PATCH',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
      body: JSON.stringify(DEFAULTS)
    }).then(function (r) {
      if (r.ok) {
        window.swPreferences.applyAll(DEFAULTS);
        // Close first so the scrim, aria-expanded triggers, and data-prefs-open are
        // all cleaned up before the outerHTML swap replaces #sw-prefs-drawer.
        close();
        // Reload the drawer to reflect reset values.
        if (typeof htmx !== 'undefined') {
          htmx.ajax('GET', window.location.pathname, { target: '#sw-prefs-drawer', swap: 'outerHTML', select: '#sw-prefs-drawer' });
        }
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
    document.querySelectorAll('[data-sw-prefs-trigger]').forEach(function (el) {
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

    // Tile clicks.
    drawer.querySelectorAll('.sw-prefs-tile').forEach(function (t) {
      t.addEventListener('click', function () { onTileClick(t); });
    });

    // Toggle clicks.
    drawer.querySelectorAll('.sw-prefs-toggle').forEach(function (b) {
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
        var label = FONT_SIZE_LABELS[stop] || FONT_SIZE_LABELS[1] || FONT_SIZE_STOPS[stop];
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
    var bgSlider = drawer.querySelector('#pref-d-bg-opacity');
    if (bgSlider) {
      bgSlider.addEventListener('input', function () {
        var label = drawer.querySelector('#pref-d-bg-opacity-value');
        if (label) label.textContent = bgSlider.value + '%';
        if (typeof window.swUpdateBgOpacity === 'function') {
          window.swUpdateBgOpacity(bgSlider.value);
        }
      });
      bgSlider.addEventListener('change', function () {
        if (typeof window.swSaveBgOpacity === 'function') {
          window.swSaveBgOpacity(bgSlider.value);
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
    drawer.querySelectorAll('[data-group-id]').forEach(function (trigger) {
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

    // Scrim click: close.
    var scrim = getScrim();
    if (scrim) {
      scrim.addEventListener('click', close);
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

  // After HTMX swaps the lazy-loaded drawer into the DOM, wire it up and
  // open it (the user already clicked the trigger, so we should open immediately).
  document.body.addEventListener('htmx:afterSwap', function (ev) {
    var target = ev.detail && ev.detail.target;
    // Initial lazy-mount: #sw-prefs-mount is replaced by the scrim+drawer pair.
    // Reset swaps: resetLayout/resetAll swap #sw-prefs-drawer outerHTML; the new
    // element has no open class and no event listeners, so wire()+open() must run
    // here just as they do for the initial mount.
    if (target && (target.id === 'sw-prefs-mount' || target.id === 'sw-prefs-drawer')) {
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
