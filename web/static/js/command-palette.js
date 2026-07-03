/* Command palette (next/) — Cmd-K. Vanilla JS, no framework. Exposes
 * window.swCommandPalette = { buildIndex, match, open, hide, toggle, isOpen,
 * activate }. This module owns show/hide, row rendering, live-filter-on-input,
 * dispatch routing (navigate / client action / server POST / inline confirm),
 * and open-scoped ↑/↓/Enter + row-click activation. */
(function () {
  'use strict';

  var SCREEN_HREF = { d: '/next/', a: '/next/artists', r: '/next/reports', l: '/next/logs', f: '/next/reports', s: '/next/settings' };

  // Module state, populated lazily on first open() (elements may not exist
  // yet at script-eval time depending on load order).
  var root = null;
  var input = null;
  var listEl = null;
  var emptyEl = null;
  var items = [];
  var activeIdx = -1;
  var opener = null;
  var wired = false;
  var navWired = false;
  var armedConfirmId = null;
  var CONFIRM_LABEL = 'Press Enter again to confirm — rewrites metadata in bulk';

  var ACTIONS = [
    { id: 'act-theme',     label: 'Toggle theme',        kind: 'action', run: 'theme',   keywords: ['dark', 'light'] },
    { id: 'act-prefs',     label: 'Open preferences',    kind: 'action', run: 'prefs',   keywords: ['settings', 'drawer'] },
    { id: 'act-run-rules', label: 'Run all rules',       kind: 'action', run: 'post', href: '/api/v1/rules/run-all', keywords: ['validate', 'lint', 'check'] },
    { id: 'act-scan',      label: 'Scan library',        kind: 'action', run: 'post', href: '/api/v1/scanner/run', keywords: ['refresh', 'rescan'] },
    { id: 'act-fix-all',   label: 'Auto-fix violations', kind: 'action', run: 'confirm-post', href: '/api/v1/notifications/fix-all', keywords: ['repair', 'resolve'], danger: true },
  ];

  var SETTINGS = [
    { id: 'set-general',     label: 'General',            kind: 'setting', href: '/next/settings#section-general', group: 'Essentials' },
    { id: 'set-libraries',   label: 'Music libraries',    kind: 'setting', href: '/next/settings#section-libraries', group: 'Essentials' },
    { id: 'set-providers',   label: 'Metadata providers', kind: 'setting', href: '/next/settings#section-providers', group: 'Data' },
    { id: 'set-rules',       label: 'Rules & severity',   kind: 'setting', href: '/next/settings#section-rules', group: 'Data' },
    { id: 'set-connections', label: 'Servers',            kind: 'setting', href: '/next/settings#section-connections', group: 'Integrations' },
  ];

  function buildIndex(registryList) {
    var out = [];
    (registryList || []).forEach(function (e) {
      if (e.scope === 'global' && e.kind === 'manual' && e.key && e.key.indexOf('g ') === 0) {
        var letter = e.key.slice(2);
        if (SCREEN_HREF[letter]) {
          out.push({ id: 'scr-' + letter, label: e.label, kind: 'screen', href: SCREEN_HREF[letter], shortcut: ['g', letter], keywords: [] });
        }
      }
    });
    out = out.concat(SETTINGS).concat(ACTIONS);
    return out;
  }

  function match(item, q) {
    if (!q) return true;
    var ql = q.toLowerCase();
    if ((item.label || '').toLowerCase().indexOf(ql) !== -1) return true;
    return (item.keywords || []).some(function (k) { return k.toLowerCase().indexOf(ql) !== -1; });
  }

  // ensureEls binds the DOM references on first use; safe to call repeatedly.
  function ensureEls() {
    if (!root) root = document.getElementById('sw-cmdk');
    if (!input) input = document.getElementById('sw-cmdk-input');
    if (!listEl) listEl = document.querySelector('[data-cmdk-list]');
    if (!emptyEl) emptyEl = document.getElementById('sw-cmdk-empty');
  }

  // makeRow builds one <button class="sw-cmdk-row"> for an index entry. Rows
  // for screens get a two-key .sw-kbd chip pair (mirrors the cheat-sheet
  // modal's kbd markup); other kinds get a plain kind tag.
  function makeRow(item, idx) {
    var row = document.createElement('button');
    row.type = 'button';
    row.className = 'sw-cmdk-row';
    row.setAttribute('role', 'option');
    row.setAttribute('data-idx', String(idx));
    row.setAttribute('data-id', item.id);
    row.id = 'sw-cmdk-option-' + idx;

    var label = document.createElement('span');
    label.className = 'sw-cmdk-row-label';
    label.textContent = (item.id && item.id === armedConfirmId) ? CONFIRM_LABEL : item.label;
    row.appendChild(label);

    if (item.kind === 'screen' && item.shortcut && item.shortcut.length === 2) {
      var chips = document.createElement('span');
      chips.className = 'sw-cmdk-row-shortcut';
      var k1 = document.createElement('kbd');
      k1.className = 'sw-kbd';
      k1.textContent = item.shortcut[0];
      var k2 = document.createElement('kbd');
      k2.className = 'sw-kbd';
      k2.textContent = item.shortcut[1];
      chips.appendChild(k1);
      chips.appendChild(k2);
      row.appendChild(chips);
    } else {
      var tag = document.createElement('span');
      tag.className = 'sw-cmdk-row-kind';
      tag.textContent = item.kind;
      row.appendChild(tag);
    }

    if (idx === activeIdx) {
      row.setAttribute('data-active', 'true');
      row.setAttribute('aria-selected', 'true');
    } else {
      row.setAttribute('aria-selected', 'false');
    }
    row.addEventListener('click', function () {
      window.swCommandPalette.activate(item);
    });
    return row;
  }

  var SECTION_LABELS = { screen: 'Screens', setting: 'Settings', action: 'Actions' };

  // makeSectionLabel builds the presentational group header inserted before
  // the first row of a new kind. It is NOT a .sw-cmdk-row and carries no
  // data-idx, so it never consumes a slot in the items/activeIdx indexing
  // that arrow-nav and makeRow rely on.
  function makeSectionLabel(kind) {
    var label = document.createElement('div');
    label.className = 'sw-cmdk-section-label';
    label.setAttribute('role', 'presentation');
    label.textContent = SECTION_LABELS[kind] || kind;
    return label;
  }

  // render rebuilds the row list from the live registry, filtered by q, and
  // toggles the empty state. Called on open() and on every input event.
  function render(q) {
    ensureEls();
    if (!listEl) return;
    var all = buildIndex(window.swKeyboardShortcuts ? window.swKeyboardShortcuts.list() : []);
    items = all.filter(function (item) { return match(item, q); });
    if (activeIdx < 0 || activeIdx >= items.length) activeIdx = items.length ? 0 : -1;

    listEl.innerHTML = '';
    var prevKind = null;
    items.forEach(function (item, idx) {
      if (item.kind !== prevKind) {
        listEl.appendChild(makeSectionLabel(item.kind));
        prevKind = item.kind;
      }
      listEl.appendChild(makeRow(item, idx));
    });

    if (emptyEl) {
      if (items.length === 0) {
        emptyEl.classList.remove('hidden');
      } else {
        emptyEl.classList.add('hidden');
      }
    }

    updateActiveDescendant();
  }

  // updateActiveDescendant syncs the input's aria-activedescendant with the
  // current activeIdx, so screen readers announce the highlighted row
  // without moving DOM focus off the input.
  function updateActiveDescendant() {
    if (!input) return;
    if (activeIdx >= 0 && activeIdx < items.length) {
      input.setAttribute('aria-activedescendant', 'sw-cmdk-option-' + activeIdx);
    } else {
      input.removeAttribute('aria-activedescendant');
    }
  }

  function onInput() {
    render(input ? input.value : '');
  }

  // basePath reads the app's mount prefix from the layout's meta tag, used to
  // build absolute URLs for navigation and server-POST dispatch.
  function basePath() {
    var meta = document.querySelector('meta[name="htmx-base-path"]');
    return (meta && meta.getAttribute('content')) || '';
  }

  function navigate(url) {
    if (typeof window.swNavigate === 'function') {
      window.swNavigate(url);
    } else {
      window.location.href = url;
    }
  }

  // callClientFn invokes a named window.<obj>.<method>() action, warning
  // loudly (never silently) when the target isn't wired up.
  function callClientFn(obj, method, warnMsg) {
    var target = window[obj];
    if (target && typeof target[method] === 'function') {
      target[method]();
      return true;
    }
    console.error('[command-palette] missing client target: window.' + obj + '.' + method);
    if (typeof window.showWarningToast === 'function') window.showWarningToast(warnMsg);
    return false;
  }

  // doPost issues the server-POST dispatch for run:'post' (and the confirmed
  // second activation of run:'confirm-post'), toasting success/failure.
  function doPost(item) {
    var url = basePath() + item.href;
    var headers = {};
    if (typeof window.swCsrfToken === 'function') {
      headers['X-CSRF-Token'] = window.swCsrfToken();
    } else {
      console.error("swCsrfToken unavailable - preferences.js may have failed to load; state-changing requests will 403");
    }
    return fetch(url, { method: 'POST', headers: headers })
      .then(function (res) {
        if (res && res.ok) {
          if (typeof window.showSuccessToast === 'function') window.showSuccessToast(item.label + ' started.');
        } else {
          if (typeof window.showWarningToast === 'function') window.showWarningToast(item.label + ' failed.');
        }
      })
      .catch(function () {
        if (typeof window.showWarningToast === 'function') window.showWarningToast(item.label + ' failed.');
      });
  }

  // activate dispatches an index entry by kind/run. Returns a Promise for
  // POST-backed actions (server dispatch, confirmed fix-all) so callers can
  // await completion; other kinds return undefined.
  function activate(item) {
    if (!item) return;

    if (item.kind === 'screen' || item.kind === 'setting') {
      navigate(basePath() + item.href);
      hide();
      return;
    }

    if (item.run === 'theme') {
      callClientFn('swSidebar', 'cycleTheme', 'Theme toggle is unavailable on this page.');
      hide();
      return;
    }

    if (item.run === 'prefs') {
      callClientFn('swPrefsDrawer', 'open', 'Preferences are unavailable on this page.');
      hide();
      return;
    }

    if (item.run === 'confirm-post') {
      if (armedConfirmId !== item.id) {
        // First activation arms the confirm state and re-renders the row
        // with the confirm label; no request is made yet.
        armedConfirmId = item.id;
        render(input ? input.value : '');
        return;
      }
      // Second activation of the same item: disarm and perform the POST.
      armedConfirmId = null;
      var result = doPost(item);
      hide();
      return result;
    }

    if (item.run === 'post') {
      var p = doPost(item);
      hide();
      return p;
    }

    console.error('[command-palette] unknown item, cannot activate: ' + JSON.stringify(item));
    if (typeof window.showWarningToast === 'function') window.showWarningToast('This command is not available.');
    hide();
  }

  // onKeydown is attached only while the palette is open (see open()/hide()),
  // so it never collides with keyboard.js's roving j/k handler which is
  // scoped to non-modal contexts.
  function onKeydown(e) {
    if (e.isComposing || e.keyCode === 229) return;
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      if (items.length) {
        activeIdx = (activeIdx < 0) ? 0 : Math.min(activeIdx + 1, items.length - 1);
        render(input ? input.value : '');
      }
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      if (items.length) {
        activeIdx = (activeIdx < 0) ? 0 : Math.max(activeIdx - 1, 0);
        render(input ? input.value : '');
      }
    } else if (e.key === 'Enter') {
      if (activeIdx >= 0 && activeIdx < items.length) {
        e.preventDefault();
        window.swCommandPalette.activate(items[activeIdx]);
      }
    }
  }

  function open() {
    ensureEls();
    if (!root) return;
    opener = document.activeElement;
    root.classList.remove('hidden');
    if (input) {
      input.value = '';
      input.setAttribute('aria-expanded', 'true');
    }
    // Pre-select the first row (index 0) so ArrowDown/Enter can activate
    // without an initial keypress just to "enter" the list.
    activeIdx = 0;
    armedConfirmId = null;
    render('');
    if (!wired && input) {
      input.addEventListener('input', onInput);
      wired = true;
    }
    if (!navWired) {
      document.addEventListener('keydown', onKeydown);
      navWired = true;
    }
    if (input && typeof input.focus === 'function') input.focus();
  }

  function hide() {
    ensureEls();
    if (!root) return;
    root.classList.add('hidden');
    if (input) {
      input.value = '';
      input.setAttribute('aria-expanded', 'false');
      input.removeAttribute('aria-activedescendant');
    }
    armedConfirmId = null;
    if (navWired) {
      document.removeEventListener('keydown', onKeydown);
      navWired = false;
    }
    if (opener && typeof opener.focus === 'function') opener.focus();
    opener = null;
  }

  function isOpen() {
    ensureEls();
    return !!root && !root.classList.contains('hidden');
  }

  function toggle() {
    if (isOpen()) {
      hide();
    } else {
      open();
    }
  }

  window.swCommandPalette = {
    buildIndex: buildIndex,
    match: match,
    open: open,
    hide: hide,
    toggle: toggle,
    isOpen: isOpen,
    activate: activate,
  };
})();
