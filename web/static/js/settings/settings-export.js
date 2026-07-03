// Settings: extracted from the inline settingsExportScript() (M55 #1808).
// Behavior-preserving lift out of web/templates/settings.templ; the JS
// is verbatim except for this IIFE wrapper, the load-once guard, the
// window re-exports below, and CSRF reads routed through the canonical
// window.swCsrfToken() helper (preferences.js) instead of an inline
// cookie-parse regex.
//
// DOM contract (ids bound in settings.templ): export-btn, export-result, export-spinner
// Network: /api/v1/settings/export
//
// Export surface: window.swSettingsExport doubles as the load-once guard;
// the following are re-exported to window because markup event
// handlers or sibling modules call them by name: exportSettings.
(function () {
  'use strict';

  if (window.swSettingsExport) return;

      function exportSettings(form) {
        var pp = form.querySelector('[name=export_passphrase]').value;
        if (!pp) return;

        var btn = document.getElementById('export-btn');
        var spinner = document.getElementById('export-spinner');
        if (btn) btn.disabled = true;
        if (spinner) spinner.classList.remove('hidden');

        var bp = (document.querySelector('meta[name="htmx-base-path"]') || {content: ''}).content;
        var csrfToken;
        if (typeof window.swCsrfToken === 'function') {
          csrfToken = window.swCsrfToken();
        } else {
          console.error("swCsrfToken unavailable - preferences.js may have failed to load; state-changing requests will 403");
          csrfToken = '';
        }

        fetch(bp + '/api/v1/settings/export', {
          method: 'POST',
          headers: {
            'Content-Type': 'application/x-www-form-urlencoded',
            'X-CSRF-Token': csrfToken
          },
          body: 'passphrase=' + encodeURIComponent(pp)
        }).then(function(r) {
          if (!r.ok) throw new Error('export failed');
          var cd = r.headers.get('Content-Disposition') || '';
          var match = cd.match(/filename="(.+?)"/);
          var filename = match ? match[1] : 'stillwater-settings.json';
          return r.blob().then(function(blob) { return { blob: blob, filename: filename }; });
        }).then(function(result) {
          var url = URL.createObjectURL(result.blob);
          var a = document.createElement('a');
          a.href = url;
          a.download = result.filename;
          document.body.appendChild(a);
          a.click();
          document.body.removeChild(a);
          URL.revokeObjectURL(url);
          form.reset();
          // Read the summary from the downloaded envelope and render a count
          // readout symmetrical to the import success message.
          result.blob.text().then(function(text) {
            try {
              var env = JSON.parse(text);
              if (env && env.summary) {
                var s = env.summary;
                var out = document.getElementById('export-result');
                if (out) {
                  out.innerHTML = '<div class="text-sm text-green-600 dark:text-green-400">'
                    + 'Export complete: ' + s.settings + ' settings, '
                    + s.connections + ' connections, '
                    + s.platform_profiles + ' profiles, '
                    + s.webhooks + ' webhooks, '
                    + s.provider_keys + ' provider keys, '
                    + s.priorities + ' priorities, '
                    + s.rules + ' rules, '
                    + s.scraper_configs + ' scraper configs, '
                    + s.user_preferences + ' preferences.'
                    + '</div>';
                }
              }
            } catch (e) { /* non-JSON or missing summary: stay silent */ }
          });
        }).catch(function(err) {
          var out = document.getElementById('export-result');
          if (out) out.innerHTML = '<div class="text-sm text-red-600 dark:text-red-400">Export failed. Please try again.</div>';
          else alert('Export failed. Please try again.');
        }).finally(function() {
          if (btn) btn.disabled = false;
          if (spinner) spinner.classList.add('hidden');
        });
      }

  // Re-exports: markup on* handlers and sibling settings modules call
  // these by bare name, so they must live on window.
  window.exportSettings = exportSettings;

  window.swSettingsExport = { exportSettings: exportSettings };
})();
