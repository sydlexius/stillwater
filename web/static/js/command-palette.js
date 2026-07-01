/* Command palette (next/) — Cmd-K. Vanilla JS, no framework. Exposes
 * window.swCommandPalette = { buildIndex, match }. Open/render/dispatch land
 * in later tasks. */
(function () {
  'use strict';

  var SCREEN_HREF = { d: '/next/', a: '/next/artists', r: '/next/reports', l: '/next/logs', f: '/next/reports', s: '/next/settings' };

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

  window.swCommandPalette = { buildIndex: buildIndex, match: match };
})();
