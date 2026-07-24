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

  // registry: array of {key, label, scope, kind} rebuilt on every DOM scan.
  // manualRegistry: stable entries registered by pages for shortcuts handled by
  // inline scripts (not discoverable from data-sw-shortcut attributes). These
  // survive rebuild() calls; they are only removed by unregister(scope).
  var registry = [];
  var manualRegistry = [];

  // g-leader nav state (#1775). leaderActive is set when 'g' is pressed and
  // cleared after the next key (consumed) or after LEADER_TIMEOUT_MS. The
  // timeout value is overridable via window.SW_LEADER_TIMEOUT_MS for tests.
  var leaderActive = false;
  var leaderTimeout = null;
  var LEADER_TIMEOUT_MS = (window.SW_LEADER_TIMEOUT_MS || 1500);

  function clearLeader() {
    leaderActive = false;
    leaderTimeout = null;
  }

  // isNextPage: true when the current page is a next/ channel page (#1775).
  // Gates the g-leader, '?', and Esc-cheat-sheet handlers so they are inert on
  // stable. Base-path-aware via meta[name="htmx-base-path"] for sub-path deploys.
  // Tests override via window.SW_IS_NEXT_PAGE (same pattern as SW_LEADER_TIMEOUT_MS).
  function isNextPage() {
    if (typeof window.SW_IS_NEXT_PAGE !== 'undefined') return !!window.SW_IS_NEXT_PAGE;
    var bpEl = document.querySelector('meta[name="htmx-base-path"]');
    var bp = bpEl ? bpEl.content : '';
    var p = window.location.pathname;
    return p === bp + '/next' || p.indexOf(bp + '/next/') === 0;
  }

  // navigate: prefix path with the htmx base-path from the meta tag, then
  // assign to window.location.href. Tests may override via window.swNavigate.
  function navigate(path) {
    var bp = '';
    try {
      var bpEl = document.querySelector('meta[name="htmx-base-path"]');
      bp = bpEl ? bpEl.content : '';
    } catch (err) {}
    var url = bp + path;
    if (typeof window.swNavigate === 'function') {
      window.swNavigate(url);
    } else {
      window.location.href = url;
    }
  }

  function list() { return registry.slice().concat(manualRegistry.slice()); }

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

  // pendingBoundaryFocus: '' | 'first' | 'last'. OPT-IN seamless paging boundary
  // (M55 #1790, next/ dashboard only). When a roving list declares
  // data-sw-roving-boundary-next / -prev (CSS selectors for paginated Next/Prev
  // controls), pressing j past the last item (or k before the first) clicks the
  // paging control instead of clamping, then -- once the new page's items swap in
  // and rebuild() runs -- seats focus at the first ('first') or last ('last')
  // item of the new page so j/k advance feels seamless across the page boundary.
  // Lists WITHOUT those attributes (e.g. next/ artists) never set this, so it
  // stays '' and the roving-restore path behaves exactly as before.
  var pendingBoundaryFocus = '';

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
    // h/l are reserved only on a list that opts into paging (declares boundary
    // controls); elsewhere they are free for a screen's own use.
    if (key === 'h' || key === 'l') {
      var listEl = rovingList();
      if (listEl && listEl.getAttribute(
        key === 'h' ? 'data-sw-roving-boundary-prev' : 'data-sw-roving-boundary-next'
      )) return true;
    }
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

  // boundaryControl: resolve the paging control for a roving boundary (#1790,
  // opt-in). attr is 'data-sw-roving-boundary-next' or '-prev'; its value is a
  // CSS selector for the Next/Prev control. Returns the element only when it
  // exists AND is enabled (no disabled attribute / aria-disabled), so a list
  // without the attribute (artists) or a control on the last/first page (which
  // is disabled) yields null and the caller falls through to the normal clamp.
  function boundaryControl(listEl, attr) {
    var sel = listEl.getAttribute(attr);
    if (!sel) return null;
    var ctrl = document.querySelector(sel);
    if (!ctrl) return null;
    if (ctrl.disabled || ctrl.getAttribute('aria-disabled') === 'true') return null;
    return ctrl;
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
      // Seamless paging boundary (opt-in, #1790): a j/k edge auto-advance OR an
      // h/l page jump clicked the Next/Prev control, swapping in a new page of
      // items. Honor it FIRST and regardless of the prior rovingActive (h/l can
      // page from an UNFOCUSED list), seating focus at the new page's first
      // ('first') or last ('last') item. Clearing pendingRovingPrevKey too
      // prevents a stale pre-swap key from also matching. When pendingBoundaryFocus
      // is '' (artists + every normal swap) this is skipped and the original
      // key-match/clamp restore (gated on rovingActive >= 0) runs unchanged.
      if (pendingBoundaryFocus) {
        pendingRovingPrevKey = '';
        if (items.length) {
          focusRoving(pendingBoundaryFocus === 'first' ? 0 : items.length - 1, items);
        } else {
          rovingActive = -1;
        }
        pendingBoundaryFocus = '';
      } else if (rovingActive >= 0) {
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
      // Page-nav keys (opt-in, #1790): advertised ONLY when the list declares
      // paging boundary controls. h = previous page, l = next page (vim
      // horizontal, pairing with j/k vertical). They reuse the same
      // data-sw-roving-boundary-{prev,next} controls the j/k boundary
      // auto-advance uses, and jump a whole page from anywhere in the list.
      if (listEl.getAttribute('data-sw-roving-boundary-prev')) {
        registry.push({
          key: 'h',
          label: listEl.getAttribute('data-sw-roving-label-h') || '',
          scope: rovingScope,
          kind: 'roving'
        });
      }
      if (listEl.getAttribute('data-sw-roving-boundary-next')) {
        registry.push({
          key: 'l',
          label: listEl.getAttribute('data-sw-roving-label-l') || '',
          scope: rovingScope,
          kind: 'roving'
        });
      }
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
      // index can't be trusted by a future reader (#1775/#1798). Also drop any
      // pending boundary intent so it cannot leak into a later, unrelated list.
      rovingActive = -1;
      pendingBoundaryFocus = '';
    }
  }

  document.addEventListener('keydown', function (e) {
    // Esc exits the search box: on every screen where '/' focuses search,
    // pressing Esc while focus is IN that box blurs it back to the page so j/k
    // roving + page-nav resume. This runs BEFORE the isTyping guard because the
    // search box IS a text field (isTyping true), which would otherwise swallow
    // the key. We preventDefault() to suppress the native <input type="search">
    // "clear on Escape" behavior: exit != reset, so the typed query (and the
    // filtered results) are PRESERVED. Returns so Esc here is a dedicated "leave
    // search" action and does not also clear a bulk selection in the same press.
    if (e.key === 'Escape') {
      // PR2 hook: command palette (existence-guarded no-op until PR2 ships).
      if (window.swCommandPalette && typeof window.swCommandPalette.hide === 'function') {
        window.swCommandPalette.hide();
        return;
      }
      // Close the cheat sheet modal when open (#1775). Both channels: Layout
      // mounts the modal unconditionally, and LayoutNext delegates to Layout.
      var cheatModal = document.getElementById('cheat-sheet-modal');
      if (cheatModal && !cheatModal.classList.contains('hidden')) {
        if (typeof window.hideCheatSheet === 'function') window.hideCheatSheet();
        return;
      }
      var searchBox = actionTarget('/');
      if (searchBox && document.activeElement === searchBox) {
        e.preventDefault();
        searchBox.blur();
        return;
      }
    }

    // Cmd-K / Ctrl-K: open the command palette (both channels -- the palette DOM
    // and window.swCommandPalette are mounted unconditionally by Layout, #2768).
    if ((e.key === 'k' || e.key === 'K') && (e.metaKey || e.ctrlKey) && !e.altKey && !e.shiftKey) {
      e.preventDefault();
      if (window.swCommandPalette && typeof window.swCommandPalette.toggle === 'function') {
        window.swCommandPalette.toggle();
      } else if (window.console && console.error) {
        console.error('[swKbd] Cmd-K pressed but window.swCommandPalette.toggle is unavailable');
      }
      return;
    }

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
      // g-leader nav + '?' cheat-sheet: next/ only (#1775). These handlers are
      // inert on stable (channel-gated so g/? fall through to the existing stable
      // handlers; the stable '?' keydown listener in LayoutGlobalChrome fires).
      if (isNextPage()) {
        if (leaderActive) {
          leaderActive = false;
          clearTimeout(leaderTimeout);
          leaderTimeout = null;
          var leaderTargets = {
            d: '/next/', a: '/next/artists', r: '/reports',
            l: '/logs', f: '/reports', s: '/settings'
          };
          if (leaderTargets.hasOwnProperty(e.key)) {
            // M1: suppress nav while the cheat-sheet modal is open.
            var cheatOpen = document.getElementById('cheat-sheet-modal');
            if (!cheatOpen || cheatOpen.classList.contains('hidden')) {
              e.preventDefault();
              navigate(leaderTargets[e.key]);
            }
          }
          return;
        }
        if (e.key === 'g') {
          e.preventDefault();
          leaderActive = true;
          leaderTimeout = setTimeout(clearLeader, LEADER_TIMEOUT_MS);
          return;
        }
        if (e.key === '?') {
          if (typeof window.showCheatSheet === 'function') {
            e.preventDefault();
            window.showCheatSheet();
          }
          return;
        }
      }
      if (e.key === '/') {
        var search = actionTarget('/');
        if (search) { e.preventDefault(); search.focus(); } else { warnAdvertisedMissing('/'); }
        return;
      }
      // "s" focuses a secondary search box (#1757 PR-4: the reports rail
      // search; "/" stays the content search, matching /artists). Mirrors the
      // "/" branch above, with one deliberate difference: when no
      // [data-sw-shortcut="s"] target exists on the page, fall THROUGH instead
      // of returning, so the key stays available to the roving/contextual
      // layers below and screens without the binding see a genuine no-op.
      if (e.key === 's') {
        var railSearch = actionTarget('s');
        if (railSearch) { e.preventDefault(); railSearch.focus(); return; }
        warnAdvertisedMissing('s');
      }
      if (e.key === 'f' || e.key === 'F') {
        var filter = actionTarget('f');
        if (filter) { e.preventDefault(); activateToggle(filter); } else { warnAdvertisedMissing('f'); }
        return;
      }
      if (e.key === 'r' || e.key === 'R') {
        // Case-sensitive lookup first: a page may register a distinct uppercase
        // 'R' shortcut (e.g. Run Rules alongside lowercase 'r' for Refresh).
        // Fall back to the lowercase element so pages with only 'r' still fire.
        var primary = actionTarget(e.key);
        if (!primary) primary = actionTarget('r');
        if (primary) { e.preventDefault(); primary.click(); } else { warnAdvertisedMissing(e.key); }
        return;
      }

      // Layer 2: roving focus + contextual key.
      var listEl = rovingList();
      if (listEl) {
        var items = rovingItems();
        if (e.key === 'j' || e.key === 'J') {
          if (items.length) {
            e.preventDefault();
            // Seamless paging boundary (opt-in): only when focus is ALREADY on
            // the last item, try to advance to the next page. rovingActive < 0
            // still seats at 0 (never a boundary), matching prior behavior.
            if (rovingActive >= 0 && rovingActive === items.length - 1) {
              var nextCtrl = boundaryControl(listEl, 'data-sw-roving-boundary-next');
              if (nextCtrl) {
                pendingBoundaryFocus = 'first';
                nextCtrl.click();
                return;
              }
            }
            focusRoving(rovingActive < 0 ? 0 : rovingActive + 1, items);
          }
          return;
        }
        if (e.key === 'k' || e.key === 'K') {
          if (items.length) {
            e.preventDefault();
            // Symmetric boundary: only when focus is on the first item, try to
            // step back to the previous page, landing on its last item.
            if (rovingActive === 0) {
              var prevCtrl = boundaryControl(listEl, 'data-sw-roving-boundary-prev');
              if (prevCtrl) {
                pendingBoundaryFocus = 'last';
                prevCtrl.click();
                return;
              }
            }
            focusRoving(rovingActive < 0 ? 0 : rovingActive - 1, items);
          }
          return;
        }
        // Page-nav keys (opt-in, #1790): h = previous page, l = next page. They
        // jump a WHOLE page from anywhere in the list (independent of the j/k
        // roving position), clicking the same boundary control the j/k edge
        // auto-advance uses, then landing focus on the new page's FIRST item
        // (the conventional "jump to a page, start at its top" behavior). Only
        // active when the list opts in via data-sw-roving-boundary-{prev,next};
        // otherwise these keys fall through untouched (no preventDefault) so a
        // screen without paging is unaffected.
        if (e.key === 'l' || e.key === 'L') {
          var pageNext = boundaryControl(listEl, 'data-sw-roving-boundary-next');
          if (pageNext) {
            e.preventDefault();
            pendingBoundaryFocus = 'first';
            pageNext.click();
            return;
          }
        }
        if (e.key === 'h' || e.key === 'H') {
          var pagePrev = boundaryControl(listEl, 'data-sw-roving-boundary-prev');
          if (pagePrev) {
            e.preventDefault();
            pendingBoundaryFocus = 'first';
            pagePrev.click();
            return;
          }
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

  // A boundary key (h/l, or j/k at an edge) sets pendingBoundaryFocus BEFORE
  // clicking the paging control, expecting the resulting swap's rebuild() to
  // consume it. If that request ERRORS (e.g. a 500 on the page fragment), no
  // afterSwap fires, so the intent would stay armed and the NEXT unrelated swap
  // (a filter/sort) would wrongly yank focus to first/last. Clear it on a failed
  // request so a stale intent can't leak across an error.
  document.addEventListener('htmx:responseError', function () {
    pendingBoundaryFocus = '';
  });
  document.addEventListener('htmx:sendError', function () {
    pendingBoundaryFocus = '';
  });

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
    onContext: function (scope, fn) { ctxHandler = fn; },
    // register: add programmatic shortcut entries for shortcuts handled by
    // inline scripts that are not discoverable via data-sw-shortcut attributes.
    // Entries survive rebuild() calls; use unregister(scope) to remove them.
    // Each entry: { key, label } -- scope and kind:'manual' are set here.
    register: function (scope, entries) {
      var i;
      if (!Array.isArray(entries)) {
        if (window.console && console.error) {
          console.error('[swKbd] register: entries must be an array');
        }
        return;
      }
      for (i = 0; i < entries.length; i++) {
        if (entries[i] == null || typeof entries[i] !== 'object') {
          if (window.console && console.error) {
            console.error('[swKbd] register: entry at index ' + i + ' is not an object; skipping');
          }
          continue;
        }
        manualRegistry.push({
          key:   entries[i].key   || '',
          label: entries[i].label || '',
          scope: scope            || 'page',
          kind:  'manual'
        });
      }
    },
    // unregister: remove all manually-registered entries for the given scope.
    // Call before re-registering (e.g. if labels can change) or when a page
    // that registered shortcuts is torn down via HTMX full-swap.
    unregister: function (scope) {
      var i;
      for (i = manualRegistry.length - 1; i >= 0; i--) {
        if (manualRegistry[i].scope === scope) {
          manualRegistry.splice(i, 1);
        }
      }
    }
  };

  // Register global shortcuts (#1775) on next/ only: the g-leader nav and the
  // Esc-close handler below are genuinely channel-gated by isNextPage(), so
  // advertising them on stable would promise keys that do nothing there.
  //
  // NOTE on '?': the cheat-sheet MODAL is in fact mounted on both channels (by
  // the canonical Layout), and '?' does open it on stable via the ungated
  // handler in LayoutGlobalChrome. It stays in this next/-only list only
  // because its Esc-close counterpart is still gated -- see the KNOWN GAP note
  // in web/components/cheat_sheet_modal.templ. Widening it belongs with the fix
  // for that gap, not with #2768.
  if (isNextPage()) {
    window.swKeyboardShortcuts.register('global', [
      { key: 'g d', label: 'Go to Dashboard' },
      { key: 'g a', label: 'Go to Artists' },
      { key: 'g r', label: 'Go to Reports' },
      { key: 'g l', label: 'Go to Logs' },
      { key: 'g f', label: 'Go to Findings' },
      { key: 'g s', label: 'Go to Settings' },
      { key: '?',   label: 'Show Keyboard Shortcuts' },
      { key: 'Esc', label: 'Close / clear focus' }
    ]);
  }
  // Cmd-K is registered unconditionally (#2768): the palette now works on both
  // channels (the isNextPage() gate above it was the stale part), so the
  // cheat sheet should advertise it everywhere it fires.
  window.swKeyboardShortcuts.register('global', [
    { key: '⌘K', label: 'Command palette (Ctrl-K)' }
  ]);
})();
