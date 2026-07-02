// Settings: extracted from the inline basePathSaveScript() (M55 #1808).
// Behavior-preserving lift out of web/templates/settings.templ; the JS
// is verbatim except for this IIFE wrapper, the load-once guard, the
// window re-exports below, and CSRF reads routed through the canonical
// window.swCsrfToken() helper (preferences.js) instead of an inline
// cookie-parse regex.
//
// DOM contract (ids bound in settings.templ): base-path-error, base-path-field, base-path-i18n, base-path-restart-banner
// Network: /api/v1/settings
//
// Export surface: window.swBasePath doubles as the load-once guard.
// (saveBasePath is assigned to window inside the body, preserved verbatim.)
(function () {
  'use strict';

  if (window.swBasePath) return;

  window.saveBasePath = function() {
    var input = document.getElementById('base-path-field');
    var banner = document.getElementById('base-path-restart-banner');
    var errEl = document.getElementById('base-path-error');
    var i18n = document.getElementById('base-path-i18n');
    if (!input) return;
    var val = (input.value || '').trim();

    // Translated strings sourced from the hidden #base-path-i18n
    // element. English fallbacks are kept so the validator still
    // reports something useful if the i18n element is absent
    // (older cached templ render, JS exposed on a non-General
    // page, etc.) rather than crashing on a null dataset read.
    var ds = (i18n && i18n.dataset) || {};
    var msgStart = ds.errorMustStartSlash || 'Base path must start with "/".';
    var msgEnd = ds.errorMustNotEndSlash || 'Base path must not end with "/".';
    var msgProtocolRelative = ds.errorProtocolRelative || 'Base path must not start with "//" or "/\\".';
    var msgCharset = ds.errorCharset || 'Base path may only contain letters, numbers, hyphens, underscores, and slashes.';
    var msgSaveFailed = ds.errorSaveFailed || 'Save failed.';
    var msgNetwork = ds.errorNetwork || 'Network error.';

    // Normalise: an empty input is treated as the root "/" so the
    // PUT body always carries an explicit, meaningful value. The
    // server-side validator then trims any trailing slash and
    // stores the canonical form (empty string for root, leading
    // slash preserved for any other prefix).
    if (val === '') {
      val = '/';
    }

    function showError(msg) {
      if (errEl) {
        errEl.textContent = msg;
        errEl.classList.remove('hidden');
      }
      if (banner) banner.classList.add('hidden');
    }

    if (val[0] !== '/') {
      showError(msgStart);
      return;
    }
    // Mirror the server's isValidPersistedBasePath: a second
    // character of "/" or "\\" is rejected to refuse protocol-
    // relative-style and Windows-separator-prefixed values that
    // would otherwise pass the leading-slash test, save
    // successfully, and silently fail to take effect on next
    // process restart.
    if (val.length > 1 && (val.charAt(1) === '/' || val.charAt(1) === '\\')) {
      showError(msgProtocolRelative);
      return;
    }
    if (val.length > 1 && val.charAt(val.length - 1) === '/') {
      showError(msgEnd);
      return;
    }
    if (!/^\/(?:[a-zA-Z0-9_\-]+(?:\/[a-zA-Z0-9_\-]+)*)?$/.test(val)) {
      showError(msgCharset);
      return;
    }

    var bp = (document.querySelector('meta[name="htmx-base-path"]') || {content: ''}).content;
    var csrfToken;
    if (typeof window.swCsrfToken === 'function') {
      csrfToken = window.swCsrfToken();
    } else {
      console.error("swCsrfToken unavailable - preferences.js may have failed to load; state-changing requests will 403");
      csrfToken = '';
    }

    fetch(bp + '/api/v1/settings', {
      method: 'PUT',
      headers: {
        'Content-Type': 'application/json',
        'X-CSRF-Token': csrfToken
      },
      body: JSON.stringify({'server.base_path': val}),
      credentials: 'same-origin'
    }).then(function(r) {
      if (!r.ok) {
        return r.json().catch(function() { return {error: msgSaveFailed}; })
          .then(function(j) {
            showError(j.error || msgSaveFailed);
          });
      }
      // Reflect the normalized value back into the field so the
      // user sees what was persisted. Without this, clearing the
      // input and clicking Save persists "/" but leaves the
      // control visually blank until the next page load.
      input.value = val;
      if (errEl) {
        errEl.classList.add('hidden');
        errEl.textContent = '';
      }
      if (banner) banner.classList.remove('hidden');
    }).catch(function() {
      // Prefer the localized message; the browser-provided
      // err.message is always English (e.g. "Failed to fetch")
      // and would shadow the translated copy on every locale.
      // English fallback is already handled when msgNetwork is
      // derived from #base-path-i18n with a default.
      showError(msgNetwork);
    });
  };

  window.swBasePath = { loaded: true };
})();
