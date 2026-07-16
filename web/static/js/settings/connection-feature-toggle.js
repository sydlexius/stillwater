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
//   shows a value the server did not persist. A data-inflight guard ignores
//   re-entrant clicks while the PATCH is pending so overlapping requests cannot
//   resolve out of order.
//
// Export surface: window.swConnectionFeatureToggle doubles as the load-once
// guard. toggleConnectionFeature is assigned to window because the switch
// calls it by bare name.
(function () {
  'use strict';

  if (window.swConnectionFeatureToggle) return;

  function toggleConnectionFeature(btn) {
    // Serialize: ignore clicks while a PATCH is in flight so overlapping
    // requests cannot resolve out of order and revert the switch to a stale
    // state.
    if (btn.dataset.inflight === '1') return;
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
    btn.dataset.inflight = '1';
    applyState(newVal);

    var bp = (document.querySelector('meta[name="htmx-base-path"]') || {content: ''}).content;
    var csrfToken;
    if (typeof window.swCsrfToken === 'function') {
      csrfToken = window.swCsrfToken();
    } else {
      console.error("swCsrfToken unavailable - preferences.js may have failed to load; state-changing requests will 403");
      csrfToken = '';
    }
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
    }).then(function() {
      delete btn.dataset.inflight;
    });
  }

  window.toggleConnectionFeature = toggleConnectionFeature;

  // swPathMappingsAfterRequest reports the outcome of the Lidarr-only path
  // mapping form's hx-post (hx-swap="none") in the adjacent inline result span.
  // On success it shows the localized "saved" confirmation, THEN refreshes the
  // "connections" settings-fragment via swRefreshSettingsSection (the same
  // targeted-refresh helper the adjacent Test button already uses on this row
  // -- see section-refresh.js) so the server-rendered row list -- including a
  // fresh trailing blank pathMappingRow -- replaces the stale in-DOM rows.
  // Without this, hx-swap="none" leaves the just-saved values sitting in the
  // form with no blank row to enter the NEXT mapping, forcing a manual page
  // reload to add a second mapping (#2322 CR-4). On failure it shows the
  // server's error text (falling back to the localized data-sw-error) and does
  // NOT refresh, since nothing changed server-side.
  // Bound via hx-on:htmx:after-request="swPathMappingsAfterRequest(this, event)".
  window.swPathMappingsAfterRequest = function (formEl, event) {
    var connID = formEl && formEl.dataset ? formEl.dataset.connId : '';
    var resultEl;
    var msg;
    var errText;
    // Fail loudly on a missing data-conn-id: without it the result span cannot
    // be resolved, so a render regression that drops the attribute must surface.
    if (!connID) {
      console.error('swPathMappingsAfterRequest: missing data-conn-id on form; cannot show result', formEl);
      return;
    }
    resultEl = document.getElementById('path-mapping-result-' + connID);
    if (!resultEl) {
      console.error('swPathMappingsAfterRequest: missing result span for connection', connID);
      return;
    }
    if (event.detail.successful) {
      resultEl.textContent = (formEl.dataset && formEl.dataset.swOk) || 'Path mappings saved.';
      resultEl.classList.remove('hidden', 'text-red-600', 'dark:text-red-400');
      resultEl.classList.add('text-green-600', 'dark:text-green-400');
      if (typeof window.swRefreshSettingsSection === 'function') {
        window.swRefreshSettingsSection('connections');
      } else {
        console.error('swPathMappingsAfterRequest: swRefreshSettingsSection unavailable; new mapping rows will not appear without a reload');
      }
    } else {
      errText = '';
      try {
        errText = (JSON.parse(event.detail.xhr.responseText || '{}') || {}).error || '';
      } catch (_) {
        errText = '';
      }
      msg = errText || (formEl.dataset && formEl.dataset.swError) || 'Could not save the path mappings. Try again.';
      resultEl.textContent = msg;
      resultEl.classList.remove('hidden', 'text-green-600', 'dark:text-green-400');
      resultEl.classList.add('text-red-600', 'dark:text-red-400');
    }
  };

  window.swConnectionFeatureToggle = { toggle: toggleConnectionFeature };
})();
