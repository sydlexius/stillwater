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

  // swVerifyPathAfterUpdateAfterRequest syncs the Lidarr-only "Verify path
  // after rename" switch from the server's authoritative response after its
  // hx-post (hx-swap="none") returns. It mirrors swStillwaterManagedAfterRequest
  // (conflict-gate.js): on success it reads verify_path_after_update from the
  // JSON body and re-paints the button's aria-checked / class / knob / hx-vals
  // from the self-describing data-sw-{btn,knob}-{on,off} attributes (so no
  // Tailwind utility names are hardcoded here); on failure it reveals the
  // adjacent inline error span using the localized data-sw-error text. Bound via
  // hx-on:htmx:after-request="swVerifyPathAfterUpdateAfterRequest(this, event)".
  window.swVerifyPathAfterUpdateAfterRequest = function (triggerEl, event) {
    // Declarations hoisted to the function top (Biome noInnerDeclarations). `var`
    // is function-scoped, so this is behaviorally identical to declaring inline.
    var errEl;
    var resp;
    var enabled;
    var knob;
    var msg;
    var connID = triggerEl && triggerEl.dataset ? triggerEl.dataset.connId : '';
    // Fail loudly on a missing data-conn-id rather than silently no-op'ing:
    // without it we cannot resolve the inline error span or re-sync the button,
    // so a render regression that drops the attribute must surface, not hide.
    if (!connID) {
      console.error('swVerifyPathAfterUpdateAfterRequest: missing data-conn-id on toggle; cannot sync state', triggerEl);
      return;
    }
    errEl = document.getElementById('verify-path-error-' + connID);
    if (event.detail.successful) {
      if (errEl) {
        errEl.textContent = '';
        errEl.classList.add('hidden');
      }
      try {
        resp = JSON.parse(event.detail.xhr.responseText || '{}');
        if (typeof resp.verify_path_after_update === 'boolean') {
          enabled = resp.verify_path_after_update;
          knob = triggerEl.querySelector('span');
          triggerEl.setAttribute('aria-checked', String(enabled));
          triggerEl.setAttribute('class', enabled ? triggerEl.dataset.swBtnOn : triggerEl.dataset.swBtnOff);
          triggerEl.setAttribute('hx-vals', JSON.stringify({ enabled: !enabled }));
          if (knob) {
            knob.setAttribute('class', enabled ? triggerEl.dataset.swKnobOn : triggerEl.dataset.swKnobOff);
          }
        }
      } catch (_) {
        // Non-JSON response; leave the button as-is and rely on a later page
        // load to re-sync from the server-rendered state.
      }
    } else {
      msg =
        (triggerEl && triggerEl.dataset && triggerEl.dataset.swError) ||
        'Could not update the verify-path setting. Try again or reload the page.';
      if (errEl) {
        errEl.textContent = msg;
        errEl.classList.remove('hidden');
      } else if (typeof showToast === 'function') {
        showToast(msg);
      }
    }
  };

  window.swConnectionFeatureToggle = { toggle: toggleConnectionFeature };
})();
