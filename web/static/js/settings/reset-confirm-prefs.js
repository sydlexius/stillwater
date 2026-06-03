// Settings: Reset confirmation-dialog preferences (M55 #1808).
// Extracted verbatim from the inline resetConfirmPrefsScript() that used to
// live in web/templates/settings.templ. Clears the per-dialog "don't ask
// again" choices the UI stores under localStorage keys prefixed "ui.confirm.".
//
// DOM contract (Reset confirmations row in settings.templ):
//   a control with onclick="resetConfirmPrefs()" triggers the clear;
//   #confirm-reset-status -- transient confirmation flash, un-hidden for 3s.
//
// Export surface: window.swResetConfirmPrefs doubles as the load-once guard.
// The inline-handler global resetConfirmPrefs is also assigned to window
// because the row's onclick attribute calls it by name -- the same
// inline-handler-global pattern image-cache.js uses for window.clearCache.
// No other names are leaked.
(function () {
  'use strict';

  // Re-init guard: the single window.swResetConfirmPrefs export (assigned at
  // the bottom) doubles as the "already loaded" flag.
  if (window.swResetConfirmPrefs) return;

  function resetConfirmPrefs() {
    try {
      var keys = [];
      for (var i = 0; i < localStorage.length; i++) {
        var key = localStorage.key(i);
        if (key && key.indexOf('ui.confirm.') === 0) {
          keys.push(key);
        }
      }
      for (var j = 0; j < keys.length; j++) {
        localStorage.removeItem(keys[j]);
      }
    } catch (_e) {
      // Sandboxed contexts (private mode, restrictive CSP) may
      // throw on localStorage access; treat as a no-op so the
      // status flash still confirms the click was handled.
    }
    var status = document.getElementById('confirm-reset-status');
    if (status) {
      status.classList.remove('hidden');
      setTimeout(function () { status.classList.add('hidden'); }, 3000);
    }
  }

  // Inline-handler global: the row's onclick attribute calls this by name, so
  // it must live on window (see export-surface note in header).
  window.resetConfirmPrefs = resetConfirmPrefs;

  window.swResetConfirmPrefs = {
    reset: resetConfirmPrefs
  };
})();
