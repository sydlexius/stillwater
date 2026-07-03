// Settings: extracted from the inline maintenanceScript() (M55 #1808).
// Behavior-preserving lift out of web/templates/settings.templ; the JS
// is verbatim except for this IIFE wrapper, the load-once guard, the
// window re-exports below, and CSRF reads routed through the canonical
// window.swCsrfToken() helper (preferences.js) instead of an inline
// cookie-parse regex.
//
// DOM contract (ids bound in settings.templ): backup-max-age, backup-retention, backup-retention-status, maint-interval, maint-schedule-status
// Network: /api/v1/settings, /api/v1/settings/maintenance/schedule, /api/v1/settings/maintenance/status
//
// Export surface: window.swMaintenance doubles as the load-once guard;
// the following are re-exported to window because markup event
// handlers or sibling modules call them by name: saveBackupSettings, updateMaintSchedule.
(function () {
  'use strict';

  if (window.swMaintenance) return;

      var bp = (document.querySelector('meta[name="htmx-base-path"]') || {content: ''}).content;

      function updateMaintSchedule(sel) {
        var csrfToken;
        if (typeof window.swCsrfToken === 'function') {
          csrfToken = window.swCsrfToken();
        } else {
          console.error("swCsrfToken unavailable - preferences.js may have failed to load; state-changing requests will 403");
          csrfToken = '';
        }
        var status = document.getElementById('maint-schedule-status');
        fetch(bp + '/api/v1/settings/maintenance/schedule', {
          method: 'PUT',
          headers: {
            'Content-Type': 'application/json',
            'X-CSRF-Token': csrfToken,
            'HX-Request': 'true'
          },
          body: JSON.stringify({
            enabled: true,
            interval_hours: parseInt(sel.value, 10)
          })
        }).then(function(r) { return r.text(); })
          .then(function(html) {
          if (status) {
            status.innerHTML = html;
            setTimeout(function() { status.innerHTML = ''; }, 3000);
          }
        }).catch(function() {
          if (status) { status.innerHTML = '<span class="text-sm text-red-600">Failed to update.</span>'; }
        });
      }

      // Initialize dropdown from current DB value after maintenance status loads.
      document.addEventListener('htmx:afterSettle', function(evt) {
        if (evt.detail && evt.detail.target && evt.detail.target.id === 'maintenance-status') {
          fetch(bp + '/api/v1/settings/maintenance/status', {
            headers: { 'Accept': 'application/json' }
          }).then(function(r) { return r.json(); })
            .then(function(data) {
            var sel = document.getElementById('maint-interval');
            if (sel && data.schedule_interval_hours) {
              sel.value = data.schedule_interval_hours.toString();
            }
          }).catch(function() {});
        }
      });

      function saveBackupSettings() {
        var csrfToken;
        if (typeof window.swCsrfToken === 'function') {
          csrfToken = window.swCsrfToken();
        } else {
          console.error("swCsrfToken unavailable - preferences.js may have failed to load; state-changing requests will 403");
          csrfToken = '';
        }
        var retention = parseInt(document.getElementById('backup-retention').value, 10);
        var maxAge = parseInt(document.getElementById('backup-max-age').value, 10);
        var status = document.getElementById('backup-retention-status');

        if (!Number.isFinite(retention) || retention < 1) {
          showToast('Retention count must be a positive number');
          return;
        }
        if (!Number.isFinite(maxAge) || maxAge < 0) {
          showToast('Max age must be zero or a positive number');
          return;
        }

        var settings = {};
        settings['backup_retention_count'] = retention.toString();
        settings['backup_max_age_days'] = maxAge.toString();

        fetch(bp + '/api/v1/settings', {
          method: 'PUT',
          headers: {
            'Content-Type': 'application/json',
            'X-CSRF-Token': csrfToken
          },
          body: JSON.stringify(settings)
        }).then(function(r) {
          if (r.ok) {
            if (status) {
              status.classList.remove('hidden');
              setTimeout(function() { status.classList.add('hidden'); }, 3000);
            }
          } else {
            showToast('Failed to save backup settings');
          }
        }).catch(function() {
          showToast('Failed to save backup settings');
        });
      }

  // Re-exports: markup on* handlers and sibling settings modules call
  // these by bare name, so they must live on window.
  window.saveBackupSettings = saveBackupSettings;
  window.updateMaintSchedule = updateMaintSchedule;

  window.swMaintenance = { saveBackupSettings: saveBackupSettings, updateMaintSchedule: updateMaintSchedule };
})();
