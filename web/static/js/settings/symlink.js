// Settings: extracted from the inline symlinkToggleScript() (M55 #1808).
// Behavior-preserving lift out of web/templates/settings.templ; the JS
// is verbatim except for this IIFE wrapper, the load-once guard, the
// window re-exports below, and CSRF reads routed through the canonical
// window.swCsrfToken() helper (preferences.js) instead of an inline
// cookie-parse regex.
// Network: /api/v1/platforms/
//
// Export surface: window.swSymlink doubles as the load-once guard;
// the following are re-exported to window because markup event
// handlers or sibling modules call them by name: toggleSymlinks.
(function () {
  'use strict';

  if (window.swSymlink) return;

      function toggleSymlinks(btn) {
        var isOn = btn.getAttribute('aria-checked') === 'true';
        var newVal = !isOn;
        var profileId = btn.getAttribute('data-profile-id');
        var bp = (document.querySelector('meta[name="htmx-base-path"]') || {content: ''}).content;
        var csrfToken = (typeof window.swCsrfToken === 'function') ? window.swCsrfToken() : '';
        fetch(bp + '/api/v1/platforms/' + profileId, {
          method: 'PUT',
          headers: {
            'Content-Type': 'application/json',
            'X-CSRF-Token': csrfToken
          },
          body: JSON.stringify({'use_symlinks': newVal}),
          credentials: 'same-origin'
        }).then(function(r) {
          if (r.ok) {
            btn.setAttribute('aria-checked', String(newVal));
            var knob = btn.querySelector('span');
            if (newVal) {
              btn.classList.remove('bg-gray-200', 'dark:bg-gray-600');
              btn.classList.add('bg-blue-600');
              knob.classList.remove('translate-x-0');
              knob.classList.add('translate-x-5');
            } else {
              btn.classList.remove('bg-blue-600');
              btn.classList.add('bg-gray-200', 'dark:bg-gray-600');
              knob.classList.remove('translate-x-5');
              knob.classList.add('translate-x-0');
            }
          } else {
            if (typeof showToast === 'function') {
              showToast('Failed to update symlink setting.');
            }
          }
        }).catch(function() {
          if (typeof showToast === 'function') {
            showToast('Network error while updating setting.');
          }
        });
      }

  // Re-exports: markup on* handlers and sibling settings modules call
  // these by bare name, so they must live on window.
  window.toggleSymlinks = toggleSymlinks;

  window.swSymlink = { toggleSymlinks: toggleSymlinks };
})();
