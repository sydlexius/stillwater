// Settings: extracted from the inline connectionFeatureToggleScript() (M55 #1808).
// Behavior-preserving lift out of web/templates/settings.templ; the JS is
// verbatim except for this IIFE wrapper, the load-once guard, the window
// re-export of toggleConnectionFeature (called by name from the connection
// card's feature switch), and the CSRF read routed through the canonical
// window.swCsrfToken() helper (preferences.js) instead of an inline
// cookie-parse regex.
//
// DOM contract: bound via onclick="toggleConnectionFeature(this)" on a
//   role=switch button carrying data-conn-id and data-feature; toggles
//   aria-checked and the bg-/translate- Tailwind classes on the knob span.
// Network: PATCH {base}/api/v1/connections/{id}/features with
//   {"feature_<name>": bool}; sends csrf_token as X-CSRF-Token.
// Save error-handling hardened per the #1808 acceptance criteria: on a
//   non-2xx/network failure the optimistic switch state is rolled back to its
//   prior value (in addition to the showToast() feedback) so the toggle never
//   shows a value the server did not persist.
//
// Export surface: window.swConnectionFeatureToggle doubles as the load-once
// guard. toggleConnectionFeature is assigned to window because the switch
// calls it by bare name.
(function () {
  'use strict';

  if (window.swConnectionFeatureToggle) return;

  function toggleConnectionFeature(btn) {
    var connID = btn.dataset.connId;
    var feature = btn.dataset.feature;
    var isOn = btn.getAttribute('aria-checked') === 'true';
    var newVal = !isOn;
    var knob = btn.querySelector('span');

    function applyState(value) {
      btn.setAttribute('aria-checked', String(value));
      if (value) {
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
    }

    // Optimistic UI update, rolled back to the prior state if the save fails.
    applyState(newVal);

    var bp = (document.querySelector('meta[name="htmx-base-path"]') || {content: ''}).content;
    var csrfToken = (typeof window.swCsrfToken === 'function') ? window.swCsrfToken() : '';
    var body = {};
    body['feature_' + feature] = newVal;
    fetch(bp + '/api/v1/connections/' + connID + '/features', {
      method: 'PATCH',
      headers: {
        'Content-Type': 'application/json',
        'X-CSRF-Token': csrfToken
      },
      body: JSON.stringify(body)
    }).then(function(r) {
      if (!r.ok) {
        applyState(isOn);
        if (typeof showToast === 'function') {
          showToast('Failed to update feature toggle.');
        }
      }
    }).catch(function() {
      applyState(isOn);
      if (typeof showToast === 'function') {
        showToast('Network error updating feature toggle.');
      }
    });
  }

  window.toggleConnectionFeature = toggleConnectionFeature;

  window.swConnectionFeatureToggle = { toggle: toggleConnectionFeature };
})();
