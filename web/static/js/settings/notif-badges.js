// Settings: Notification Badge controls (M55 #1806 spike).
// Extracted verbatim from the inline badgeSettingScript() that used to live in
// web/templates/settings.templ. Persists the notification-badge toggle and the
// three per-severity count checkboxes on the Settings Automation tab.
//
// DOM contract (Notification Badge Settings card in settings.templ): the badge
// enable toggle and the error/warning/info checkboxes each carry
// onclick="updateSetting('<key>', this)". Keys in use:
//   notif_badge_enabled, notif_badge_severity_error,
//   notif_badge_severity_warning, notif_badge_severity_info.
//
// Network contract (base-path aware via meta[name="htmx-base-path"]):
//   PUT {base}/api/v1/settings -- {"<key>": "true"|"false"}
// The csrf_token cookie is sent as X-CSRF-Token, read via the canonical
// window.swCsrfToken() helper from preferences.js (loaded in layout.templ
// before this module) rather than an inline cookie-parse regex.
//
// Save error-handling hardened per the optimistic-rollback pattern established
// in rule-toggle.js and connection-feature-toggle.js (#1828): on a non-2xx or
// network failure the checkbox is rolled back to its prior state and showToast()
// surfaces the error, so the UI never shows a value the server did not persist.
// A data-inflight guard ignores re-entrant clicks while a PUT is in flight so
// overlapping requests cannot resolve out of order.
//
// Export surface: window.swNotifBadges doubles as the load-once guard. The
// inline-handler global updateSetting is also assigned to window because the
// card's onclick attributes call it by name -- the same inline-handler-global
// pattern ContextHelp uses for window.swContextHelpToggle. No other names are
// leaked.
(function () {
  'use strict';

  // Re-init guard: the single window.swNotifBadges export (assigned at the
  // bottom) doubles as the "already loaded" flag.
  if (window.swNotifBadges) return;

  function updateSetting(key, el) {
    // Serialize: ignore clicks while a PUT is in flight so overlapping requests
    // cannot resolve out of order and leave the checkbox in a stale state.
    // The browser pre-toggles el.checked before onclick fires, so we must
    // revert that pre-toggle before returning or the UI desynchronises from
    // the server value while no request is actually in flight.
    if (el.dataset.inflight === '1') {
      el.checked = !el.checked;
      return;
    }

    // The browser has already toggled el.checked before onclick fires; capture
    // the new value and compute the prior value for rollback.
    var newVal = el.checked;
    var oldVal = !newVal;

    var bp = (document.querySelector('meta[name="htmx-base-path"]') || { content: '' }).content;
    var csrfToken = (typeof window.swCsrfToken === 'function') ? window.swCsrfToken() : '';
    var body = {};
    body[key] = newVal ? 'true' : 'false';

    el.dataset.inflight = '1';
    fetch(bp + '/api/v1/settings', {
      method: 'PUT',
      headers: {
        'Content-Type': 'application/json',
        'X-CSRF-Token': csrfToken
      },
      body: JSON.stringify(body)
    }).then(function(r) {
      if (!r.ok) {
        el.checked = oldVal;
        if (typeof showToast === 'function') {
          showToast('Failed to save notification badge setting.');
        }
      }
    }).catch(function() {
      el.checked = oldVal;
      if (typeof showToast === 'function') {
        showToast('Network error saving notification badge setting.');
      }
    }).then(function() {
      delete el.dataset.inflight;
    });
  }

  // Inline-handler global: the card's onclick attributes call this by name, so
  // it must live on window (see export-surface note in header).
  window.updateSetting = updateSetting;

  window.swNotifBadges = {
    updateSetting: updateSetting
  };
})();
