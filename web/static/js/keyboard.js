// Shared keyboard helper for next/ list-style screens (M55 #1789).
// Single source of truth for page-level action keys, roving focus, bulk
// select, and the shortcut REGISTRY that #1775 (cheat sheet) and #1798
// (hints pref) read. Inert until a screen declares data-sw-* attributes,
// so the stable channel (which carries none) is unaffected.
//
// Public API (exposed as window.swKeyboardShortcuts):
//   list()               -- snapshot of the registry: [{key,label,scope,kind}]
//                           (consumed by #1775 cheat sheet + #1798 hints pref)
//   rebuild()            -- re-scan the DOM and rebuild the registry (also run
//                           automatically on htmx:afterSwap / htmx:load)
//   onContext(scope, fn) -- register the per-screen contextual-key handler
//                           (scope reserved for forward-compat, ignored today)
(function () {
  'use strict';
  // Re-init guard: the single window.swKeyboardShortcuts export (assigned at the
  // bottom) doubles as the "already loaded" flag, so no extra global is leaked
  // (web-frontend.instructions.md: one window.sw<Name> export only).
  if (window.swKeyboardShortcuts) return;

  // registry: array of {key, label, scope, kind} rebuilt on every scan.
  var registry = [];

  function list() { return registry.slice(); }

  // rovingActive: index of the currently roving-focused item (-1 when none).
  // pendingRovingPrevKey: the focused item's stable key captured on
  // htmx:beforeSwap. Held in a module variable (NOT a DOM attribute on the list)
  // because #artist-content is swapped with hx-swap="outerHTML" -- the list
  // element itself is replaced, so an attribute on it would not survive the swap.
  // ctxHandler: optional handler for the contextual key, set via
  // swKeyboardShortcuts.onContext(); falls back to clicking the context target.
  // NOTE: ctxHandler is a single global (one next/ screen is live at a time), so
  // the scope arg on onContext() is reserved for forward-compat and ignored now.
  var rovingActive = -1;
  var pendingRovingPrevKey = '';
  var ctxHandler = null;

  // isTyping: true when the focused element should swallow shortcuts (a real
  // text field, textarea, select, or contentEditable). Ported (refactored into
  // a function) from ArtistsKeyboardShortcuts, which is still live on the stable
  // artists page and retired only from next/. INPUT only counts as typing when
  // its type is text-like; a focused checkbox/radio (Firefox lets rows take
  // focus) must NOT swallow shortcuts.
  function isTyping(el) {
    if (!el) return false;
    var tag = el.tagName;
    if (tag === 'TEXTAREA' || tag === 'SELECT' || el.isContentEditable) return true;
    if (tag === 'INPUT') {
      var nonText = ['checkbox','radio','button','submit','reset','file','image','range','color'];
      var itype = (el.getAttribute('type') || 'text').toLowerCase();
      if (nonText.indexOf(itype) === -1) return true;
    }
    return false;
  }

  // applyPlatformGlyph: on macOS, render the Cmd glyph in every .sw-mod-key
  // chip so the displayed modifier matches the platform. Non-fatal: a throw
  // (e.g. navigator.platform unavailable) just leaves the "Ctrl" default, which
  // is self-evident on screen. NOTE: the stable ArtistsKeyboardShortcuts does
  // the identical swap; the two must stay in sync until the stable handler is
  // de-duped onto this helper at promotion.
  function applyPlatformGlyph() {
    try {
      var isMac = /Mac|iPod|iPhone|iPad/.test(navigator.platform);
      if (!isMac) return;
      var chips = document.querySelectorAll('.sw-mod-key');
      for (var i = 0; i < chips.length; i++) {
        chips[i].textContent = '⌘';
      }
    } catch (err) {
      if (window.console && console.debug) {
        console.debug('[swKbd] platform glyph swap skipped', err);
      }
    }
  }

  // actionTarget: the DOM element a page-level action key acts on.
  function actionTarget(key) {
    return document.querySelector('[data-sw-shortcut="' + key + '"]');
  }

  // activateToggle: toggle a trigger that exposes the aria-expanded +
  // aria-controls flyout contract via the shared swFilterFlyout controller, so
  // pressing the key twice opens then closes. The trigger's own onclick is
  // open-only (OpenFilterFlyout), so a blind .click() would never close it.
  // Falls back to a plain click when the toggle contract is absent.
  function activateToggle(el) {
    var panel = el.getAttribute('aria-controls');
    if (panel && window.swFilterFlyout &&
        el.getAttribute('aria-expanded') !== null) {
      if (el.getAttribute('aria-expanded') === 'true') {
        window.swFilterFlyout.close(panel);
      } else {
        window.swFilterFlyout.open(panel);
      }
      return;
    }
    el.click();
  }

  // warnAdvertisedMissing: a registry-advertised action key was pressed but its
  // target is gone from the DOM (e.g. a conditionally-rendered button). Keys not
  // in the registry stay genuinely inert (the stable-channel guarantee); only
  // advertised-then-missing keys get a dev breadcrumb.
  function warnAdvertisedMissing(key) {
    for (var i = 0; i < registry.length; i++) {
      if (registry[i].key === key && registry[i].kind === 'action') {
        if (window.console && console.warn) {
          console.warn('[swKbd] shortcut "' + key + '" advertised but no target element in DOM');
        }
        return;
      }
    }
  }

  // reservedKey: true when key is owned by the helper's built-in roving layer or
  // by a declared page-action key, so a screen-declared contextual key that
  // collides with it would be silently shadowed.
  function reservedKey(key) {
    if (key === 'j' || key === 'k' || key === 'Enter') return true;
    return !!actionTarget(key);
  }

  // rovingList: the single roving container on the page (or null).
  function rovingList() {
    return document.querySelector('[data-sw-roving-list]');
  }

  // rovingItems: array of roving items within the list (empty if no list).
  function rovingItems() {
    var listEl = rovingList();
    if (!listEl) return [];
    return Array.prototype.slice.call(
      listEl.querySelectorAll('[data-sw-roving-item]')
    );
  }

  // itemKey: stable per-item id used to restore focus across HTMX swaps.
  function itemKey(el) {
    return (el && el.getAttribute('data-sw-roving-key')) || '';
  }

  // focusRoving: move roving focus to idx (clamped), managing tabindex so only
  // the active item is in the tab order, then focus + scroll it into view.
  function focusRoving(idx, items) {
    if (!items) items = rovingItems();
    if (!items.length) { rovingActive = -1; return; }
    if (idx < 0) idx = 0;
    if (idx > items.length - 1) idx = items.length - 1;
    for (var i = 0; i < items.length; i++) {
      items[i].setAttribute('tabindex', i === idx ? '0' : '-1');
    }
    rovingActive = idx;
    var active = items[idx];
    active.focus();
    if (active.scrollIntoView) {
      active.scrollIntoView({ block: 'nearest' });
    }
  }

  // rebuild: rebuild the registry from the current DOM. Called on init and on
  // every HTMX swap/load so the cheat sheet reflects what is actually present.
  function rebuild() {
    registry.length = 0;

    // Layer 1: page-level action keys.
    var actions = document.querySelectorAll('[data-sw-shortcut]');
    for (var i = 0; i < actions.length; i++) {
      var el = actions[i];
      registry.push({
        key: el.getAttribute('data-sw-shortcut'),
        label: el.getAttribute('data-sw-shortcut-label') || '',
        scope: el.getAttribute('data-sw-scope') || 'page',
        kind: 'action'
      });
    }

    // Layer 2: roving descriptors. Built from the list element if present.
    var listEl = rovingList();
    if (listEl) {
      var items = rovingItems();
      // Restore roving focus across an HTMX swap: match the pre-swap item key
      // recorded on the list, else clamp the old index into the new range.
      // Always correct rovingActive to a valid index, but only actually move
      // focus when focus is NOT in a text field. The search input lives
      // outside the swapped container and keeps focus through the swap; yanking
      // focus onto a card mid-typing would be bad UX. After a non-search swap
      // (sort, view toggle, filter, pagination) focus has typically reverted to
      // body (isTyping=false), so the restore still runs.
      if (rovingActive >= 0) {
        var prevKey = pendingRovingPrevKey;
        pendingRovingPrevKey = '';
        var restored = -1;
        if (prevKey) {
          for (var r = 0; r < items.length; r++) {
            if (itemKey(items[r]) === prevKey) { restored = r; break; }
          }
        }
        if (!items.length) {
          rovingActive = -1;
        } else {
          // Compute the corrected index (matched key, else clamped old index).
          var target = restored >= 0
            ? restored
            : (rovingActive > items.length - 1 ? items.length - 1 : rovingActive);
          if (!isTyping(document.activeElement)) {
            focusRoving(target, items);
          } else {
            // Focus is in a text field (search): correct the index and tabindex
            // so j/k resumes correctly later, but do not steal focus.
            rovingActive = target;
            for (var t = 0; t < items.length; t++) {
              items[t].setAttribute('tabindex', t === target ? '0' : '-1');
            }
          }
        }
      }

      var rovingScope = listEl.getAttribute('data-sw-scope') || 'page';
      registry.push({
        key: 'j',
        label: listEl.getAttribute('data-sw-roving-label-j') || '',
        scope: rovingScope,
        kind: 'roving'
      });
      registry.push({
        key: 'k',
        label: listEl.getAttribute('data-sw-roving-label-k') || '',
        scope: rovingScope,
        kind: 'roving'
      });
      registry.push({
        key: 'Enter',
        label: listEl.getAttribute('data-sw-roving-label-Enter') || '',
        scope: rovingScope,
        kind: 'roving'
      });
      var ctxKey = listEl.getAttribute('data-sw-roving-context-key');
      if (ctxKey) {
        if (reservedKey(ctxKey) && window.console && console.warn) {
          console.warn('[swKbd] contextual key "' + ctxKey + '" collides with a reserved/page shortcut and will be shadowed');
        }
        registry.push({
          key: ctxKey,
          label: listEl.getAttribute('data-sw-roving-context-label') || '',
          scope: rovingScope,
          kind: 'roving'
        });
      }
    } else {
      // No roving list on the page: keep the global honest so a stale positive
      // index can't be trusted by a future reader (#1775/#1798).
      rovingActive = -1;
    }
  }

  document.addEventListener('keydown', function (e) {
    if (isTyping(document.activeElement)) return;

    // Layer 3: bulk select parity. Cmd/Ctrl+A selects all, Esc clears, scoped
    // to the container marked data-sw-bulk-scope. Placed before the
    // no-modifier block because Cmd/Ctrl+A requires the modifier.
    var bulkScope = document.querySelector('[data-sw-bulk-scope]');
    if (bulkScope) {
      if ((e.key === 'a' || e.key === 'A') && (e.metaKey || e.ctrlKey)) {
        e.preventDefault();
        var boxes = bulkScope.querySelectorAll('.sw-bulk-select');
        for (var b = 0; b < boxes.length; b++) {
          if (!boxes[b].checked) {
            boxes[b].checked = true;
            boxes[b].dispatchEvent(new Event('change', { bubbles: true }));
          }
        }
        return;
      }
      if (e.key === 'Escape') {
        // Clear selection but do not return: Escape may close other UI too.
        var checked = bulkScope.querySelectorAll('.sw-bulk-select:checked');
        for (var c = 0; c < checked.length; c++) {
          checked[c].checked = false;
          checked[c].dispatchEvent(new Event('change', { bubbles: true }));
        }
      }
    }

    if (!e.metaKey && !e.ctrlKey && !e.altKey) {
      if (e.key === '/') {
        var search = actionTarget('/');
        if (search) { e.preventDefault(); search.focus(); } else { warnAdvertisedMissing('/'); }
        return;
      }
      if (e.key === 'f' || e.key === 'F') {
        var filter = actionTarget('f');
        if (filter) { e.preventDefault(); activateToggle(filter); } else { warnAdvertisedMissing('f'); }
        return;
      }
      if (e.key === 'r' || e.key === 'R') {
        var primary = actionTarget('r');
        if (primary) { e.preventDefault(); primary.click(); } else { warnAdvertisedMissing('r'); }
        return;
      }

      // Layer 2: roving focus + contextual key.
      var listEl = rovingList();
      if (listEl) {
        var items = rovingItems();
        if (e.key === 'j' || e.key === 'J') {
          if (items.length) {
            e.preventDefault();
            focusRoving(rovingActive < 0 ? 0 : rovingActive + 1, items);
          }
          return;
        }
        if (e.key === 'k' || e.key === 'K') {
          if (items.length) {
            e.preventDefault();
            focusRoving(rovingActive < 0 ? 0 : rovingActive - 1, items);
          }
          return;
        }
        if (e.key === 'Enter') {
          if (rovingActive >= 0 && items[rovingActive]) {
            var activate = items[rovingActive]
              .querySelector('[data-sw-roving-activate]');
            if (activate) { e.preventDefault(); activate.click(); }
          }
          return;
        }
        // Contextual key: one per-screen key declared on the list.
        var ctxKey = listEl.getAttribute('data-sw-roving-context-key');
        if (ctxKey && e.key === ctxKey && rovingActive >= 0 && items[rovingActive]) {
          e.preventDefault();
          var item = items[rovingActive];
          if (ctxHandler) {
            ctxHandler(item);
          } else {
            var ctxTarget = item.querySelector('[data-sw-roving-context]');
            if (ctxTarget) ctxTarget.click();
          }
          return;
        }
      }
    }
  });

  function init() {
    applyPlatformGlyph();
    rebuild();
  }

  if (document.readyState !== 'loading') {
    init();
  } else {
    document.addEventListener('DOMContentLoaded', init);
  }
  document.addEventListener('htmx:afterSwap', rebuild);
  document.addEventListener('htmx:load', rebuild);

  // Before an HTMX swap replaces the list, record the focused item's key in a
  // module variable so rebuild() can restore roving focus to the same logical
  // row. Stored off-DOM because the list is swapped with outerHTML (the element
  // itself is replaced), so an attribute on it would be lost.
  document.addEventListener('htmx:beforeSwap', function () {
    if (rovingActive < 0) return;
    var items = rovingItems();
    if (items[rovingActive]) {
      pendingRovingPrevKey = itemKey(items[rovingActive]);
    }
  });

  window.swKeyboardShortcuts = {
    list: list,
    rebuild: rebuild,
    // scope is reserved for forward-compat (per-scope dispatch) and ignored
    // today; ctxHandler is global since one next/ screen is live at a time.
    onContext: function (scope, fn) { ctxHandler = fn; }
  };
})();
