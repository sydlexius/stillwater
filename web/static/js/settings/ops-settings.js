// Settings: Operational controls surfaced from env-only into the UI
// (#1746 rule-engine artist workers, #1753 scanner exclusions + backup interval).
//
// Toggles (scanner.mtime_fast_path) reuse window.updateSetting from
// notif-badges.js -- that helper is checkbox-specific (reads el.checked) and
// already handles CSRF, the optimistic rollback, and the in-flight guard.
//
// This module adds swSaveOpsSetting() for the save-button-backed text/number
// inputs (artist-worker count, scanner exclusions, backup interval). On a
// successful save it flashes the row's status pill; for the backup interval it
// also reveals the shared restart-required banner, because the backup scheduler
// is started once at boot and only picks up a new interval on the next restart.
//
// Network contract (base-path aware via meta[name="htmx-base-path"]):
//   PUT {base}/api/v1/settings -- {"<key>": "<value>"}
// The csrf_token cookie is sent as X-CSRF-Token via the canonical
// window.swCsrfToken() helper (preferences.js, loaded before this module).
//
// Export surface: window.swOpsSettings doubles as the load-once guard, and
// window.swSaveOpsSetting is exposed because the cards' onclick attributes call
// it by name (the inline-handler-global pattern used by updateSetting).
(function () {
  'use strict';

  if (window.swOpsSettings) return;

  // saveOpsSetting persists one setting key from the value of inputId.
  //   key:       the settings-table key (e.g. "rule_engine.artist_workers")
  //   inputId:   id of the <input> whose .value is sent
  //   statusId:  optional id of a "Saved" pill to flash on success
  //   restartId: optional id of a restart-required banner to reveal on success
  function saveOpsSetting(key, inputId, statusId, restartId) {
    var input = document.getElementById(inputId);
    if (!input) return;

    // Serialize: ignore re-entrant clicks while a PUT is in flight.
    if (input.dataset.inflight === '1') return;

    var bp = (document.querySelector('meta[name="htmx-base-path"]') || { content: '' }).content;
    var csrfToken = (typeof window.swCsrfToken === 'function') ? window.swCsrfToken() : '';
    var body = {};
    body[key] = String(input.value);

    input.dataset.inflight = '1';
    fetch(bp + '/api/v1/settings', {
      method: 'PUT',
      headers: {
        'Content-Type': 'application/json',
        'X-CSRF-Token': csrfToken
      },
      body: JSON.stringify(body)
    }).then(function (r) {
      if (r.ok) {
        if (statusId) {
          var pill = document.getElementById(statusId);
          if (pill) {
            pill.classList.remove('hidden');
            setTimeout(function () { pill.classList.add('hidden'); }, 2000);
          }
        }
        if (restartId) {
          var banner = document.getElementById(restartId);
          if (banner) banner.classList.remove('hidden');
        }
        return;
      }
      return r.json().then(function (j) {
        if (typeof showToast === 'function') {
          showToast((j && j.error) || 'Failed to save setting.');
        }
      }).catch(function () {
        if (typeof showToast === 'function') showToast('Failed to save setting.');
      });
    }).catch(function () {
      if (typeof showToast === 'function') showToast('Network error saving setting.');
    }).then(function () {
      delete input.dataset.inflight;
    });
  }

  // Inline-handler global: the cards' onclick attributes call this by name.
  window.swSaveOpsSetting = saveOpsSetting;

  window.swOpsSettings = {
    saveOpsSetting: saveOpsSetting
  };
})();
