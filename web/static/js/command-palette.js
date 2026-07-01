/* Command palette (next/) — Cmd-K. Vanilla JS, no framework. Exposes
 * window.swCommandPalette = { buildIndex, match, open, hide, toggle, isOpen }.
 * Keyboard binding (Cmd-K) and row activation/dispatch land in later tasks;
 * this module only owns show/hide, row rendering, and live-filter-on-input. */
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

  var ACTIONS = [
    { id: 'act-theme',     label: 'Toggle theme',        kind: 'action', run: 'theme',   keywords: ['dark', 'light'] },
    { id: 'act-prefs',     label: 'Open preferences',    kind: 'action', run: 'prefs',   keywords: ['settings', 'drawer'] },
    { id: 'act-run-rules', label: 'Run all rules',       kind: 'action', run: 'post', href: '/api/v1/rules/run-all', keywords: ['validate', 'lint', 'check'] },
    { id: 'act-scan',      label: 'Scan library',        kind: 'action', run: 'post', href: '/api/v1/scanner/run', keywords: ['refresh', 'rescan'] },
    { id: 'act-fix-all',   label: 'Auto-fix violations', kind: 'action', run: 'confirm-post', href: '/api/v1/notifications/fix-all', keywords: ['repair', 'resolve'], danger: true },
  ];

  var SETTINGS = [
    { id: 'set-general',     label: 'General',            kind: 'setting', href: '/next/settings#general', group: 'Essentials' },
    { id: 'set-libraries',   label: 'Music libraries',    kind: 'setting', href: '/next/settings#libraries', group: 'Essentials' },
    { id: 'set-providers',   label: 'Metadata providers', kind: 'setting', href: '/next/settings#providers', group: 'Data' },
    { id: 'set-rules',       label: 'Rules & severity',   kind: 'setting', href: '/next/settings#rules', group: 'Data' },
    { id: 'set-connections', label: 'Servers',            kind: 'setting', href: '/next/settings#connections', group: 'Integrations' },
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

    var label = document.createElement('span');
    label.className = 'sw-cmdk-row-label';
    label.textContent = item.label;
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

    if (idx === activeIdx) row.setAttribute('data-active', 'true');
    return row;
  }

  // render rebuilds the row list from the live registry, filtered by q, and
  // toggles the empty state. Called on open() and on every input event.
  function render(q) {
    ensureEls();
    if (!listEl) return;
    var all = buildIndex(window.swKeyboardShortcuts ? window.swKeyboardShortcuts.list() : []);
    items = all.filter(function (item) { return match(item, q); });
    if (activeIdx >= items.length) activeIdx = items.length ? 0 : -1;

    listEl.innerHTML = '';
    items.forEach(function (item, idx) {
      listEl.appendChild(makeRow(item, idx));
    });

    if (emptyEl) {
      if (items.length === 0) {
        emptyEl.classList.remove('hidden');
      } else {
        emptyEl.classList.add('hidden');
      }
    }
  }

  function onInput() {
    render(input ? input.value : '');
  }

  function open() {
    ensureEls();
    if (!root) return;
    opener = document.activeElement;
    root.classList.remove('hidden');
    if (input) {
      input.value = '';
    }
    activeIdx = -1;
    render('');
    if (!wired && input) {
      input.addEventListener('input', onInput);
      wired = true;
    }
    if (input && typeof input.focus === 'function') input.focus();
  }

  function hide() {
    ensureEls();
    if (!root) return;
    root.classList.add('hidden');
    if (input) input.value = '';
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
  };
})();
