// Settings: extracted from the inline ruleScheduleScript() (M55 #1808).
// Behavior-preserving lift out of web/templates/settings.templ; the JS is
// verbatim except for this IIFE wrapper, the load-once guard, the window
// re-export of updateRuleSchedule (called by name from the schedule <select>),
// and the CSRF read routed through the canonical window.swCsrfToken() helper
// (preferences.js) instead of an inline cookie-parse regex.
//
// DOM contract: bound via onchange="updateRuleSchedule(this.value)" on the
//   rule-schedule interval <select>.
// Network: PUT {base}/api/v1/settings with
//   {"rule_schedule.interval_minutes": value}; sends csrf_token as
//   X-CSRF-Token. Save error-handling hardened per the #1808 acceptance
//   criteria: a non-2xx/network failure surfaces a showToast() message so the
//   save no longer fails silently. The interval value itself is not rolled back
//   because the onchange handler receives only the value, not the <select>
//   element; rollback would require a markup change tracked separately.
//
// Export surface: window.swRuleSchedule doubles as the load-once guard.
// updateRuleSchedule is assigned to window because the <select> calls it by
// bare name.
(function () {
  'use strict';

  if (window.swRuleSchedule) return;

  function updateRuleSchedule(value) {
    var bp = (document.querySelector('meta[name="htmx-base-path"]') || {content: ''}).content;
    var csrfToken = (typeof window.swCsrfToken === 'function') ? window.swCsrfToken() : '';
    fetch(bp + '/api/v1/settings', {
      method: 'PUT',
      headers: {
        'Content-Type': 'application/json',
        'X-CSRF-Token': csrfToken
      },
      body: JSON.stringify({"rule_schedule.interval_minutes": value})
    }).then(function(r) {
      if (!r.ok && typeof showToast === 'function') {
        showToast('Failed to update rule schedule.');
      }
    }).catch(function() {
      if (typeof showToast === 'function') {
        showToast('Network error updating rule schedule.');
      }
    });
  }

  window.updateRuleSchedule = updateRuleSchedule;

  window.swRuleSchedule = { update: updateRuleSchedule };
})();
