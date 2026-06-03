// Settings: extracted from the inline ruleScheduleScript() (M55 #1808).
// Behavior-preserving lift out of web/templates/settings.templ; the JS is
// verbatim except for this IIFE wrapper, the load-once guard, the window
// re-export of updateRuleSchedule (called by name from the schedule <select>),
// and the CSRF read routed through the canonical window.swCsrfToken() helper
// (preferences.js) instead of an inline cookie-parse regex.
//
// DOM contract: bound via onchange="updateRuleSchedule(this)" on the
//   rule-schedule interval <select>.
// Network: PUT {base}/api/v1/settings with
//   {"rule_schedule.interval_minutes": value}; sends csrf_token as
//   X-CSRF-Token. Save error-handling hardened per the #1808 acceptance
//   criteria: the <select> is disabled during the PUT and, on a non-2xx/network
//   failure, restored to the last-confirmed value (server baseline read from the
//   option's `selected` attribute) with a showToast() message, so the dropdown
//   never shows a value the server did not persist.
//
// Export surface: window.swRuleSchedule doubles as the load-once guard.
// updateRuleSchedule is assigned to window because the <select> calls it by
// bare name.
(function () {
  'use strict';

  if (window.swRuleSchedule) return;

  // Last value the server confirmed, so a failed save can restore the dropdown.
  // null until the first successful save; the server-rendered baseline is read
  // from the option carrying the `selected` attribute (option.defaultSelected),
  // which survives later value changes.
  var lastConfirmed = null;

  function persistedValue(selectEl) {
    if (lastConfirmed !== null) return lastConfirmed;
    var opts = selectEl.options;
    for (var i = 0; i < opts.length; i++) {
      if (opts[i].defaultSelected) return opts[i].value;
    }
    return selectEl.value;
  }

  function updateRuleSchedule(selectEl) {
    var value = selectEl.value;
    var priorValue = persistedValue(selectEl);
    var bp = (document.querySelector('meta[name="htmx-base-path"]') || {content: ''}).content;
    var csrfToken = (typeof window.swCsrfToken === 'function') ? window.swCsrfToken() : '';
    selectEl.disabled = true;
    fetch(bp + '/api/v1/settings', {
      method: 'PUT',
      headers: {
        'Content-Type': 'application/json',
        'X-CSRF-Token': csrfToken
      },
      body: JSON.stringify({"rule_schedule.interval_minutes": value})
    }).then(function(r) {
      if (!r.ok) {
        selectEl.value = priorValue;
        if (typeof showToast === 'function') {
          showToast('Failed to update rule schedule.');
        }
        return;
      }
      lastConfirmed = value;
    }).catch(function() {
      selectEl.value = priorValue;
      if (typeof showToast === 'function') {
        showToast('Network error updating rule schedule.');
      }
    }).then(function() {
      selectEl.disabled = false;
    });
  }

  window.updateRuleSchedule = updateRuleSchedule;

  window.swRuleSchedule = { update: updateRuleSchedule };
})();
