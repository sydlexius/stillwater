// Settings: Notification Badge controls (M55 #1806 spike).
// Extracted verbatim from the inline badgeSettingScript() that used to live in
// web/templates/settings.templ. Persists the notification-badge toggle and the
// three per-severity count checkboxes on the Settings Automation tab.
//
// DOM contract (Notification Badge Settings card in settings.templ): the badge
// enable toggle and the error/warning/info checkboxes each carry
// onclick="updateSetting('<key>', this.checked)". Keys in use:
//   notif_badge_enabled, notif_badge_severity_error,
//   notif_badge_severity_warning, notif_badge_severity_info.
//
// Network contract (base-path aware via meta[name="htmx-base-path"]):
//   PUT {base}/api/v1/settings -- {"<key>": "true"|"false"}
// The csrf_token cookie is sent as X-CSRF-Token, read via the canonical
// window.swCsrfToken() helper from preferences.js (loaded in layout.templ
// before this module) rather than an inline cookie-parse regex.
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

  function updateSetting(key, checked) {
    var bp = (document.querySelector('meta[name="htmx-base-path"]') || { content: '' }).content;
    var csrfToken = (typeof window.swCsrfToken === 'function') ? window.swCsrfToken() : '';
    var body = {};
    body[key] = checked ? "true" : "false";
    fetch(bp + '/api/v1/settings', {
      method: 'PUT',
      headers: {
        'Content-Type': 'application/json',
        'X-CSRF-Token': csrfToken
      },
      body: JSON.stringify(body)
    });
  }

  // Inline-handler global: the card's onclick attributes call this by name, so
  // it must live on window (see export-surface note in header).
  window.updateSetting = updateSetting;

  window.swNotifBadges = {
    updateSetting: updateSetting
  };
})();
