// Settings: extracted from the inline loggingScript() (M55 #1808).
// Behavior-preserving lift out of web/templates/settings.templ; the JS
// is verbatim except for this IIFE wrapper, the load-once guard, the
// window re-exports below, and CSRF reads routed through the canonical
// window.swCsrfToken() helper (preferences.js) instead of an inline
// cookie-parse regex.
//
// DOM contract: binds the file-logging settings fields in settings.templ
// (file-logging-settings toggle, the log-file-* path/format/rotation inputs,
// the ephemeral-level revert row, and the log-cleanup-row/-status controls).
// Network: /api/v1/logs/files
//
// Export surface: window.swLogging doubles as the load-once guard;
// the following are re-exported to window because markup event
// handlers or sibling modules call them by name: cleanupLogFiles, loadLoggingConfig, syncLogFilePath, toggleFileLogging, updateCleanupRow, updateEphemeralRow.
(function () {
  'use strict';

  if (window.swLogging) return;

      // Resolve base path for sub-path deployments.
      var logSettingsBasePath = '';
      var logSettingsMeta = document.querySelector('meta[name="htmx-base-path"]');
      if (logSettingsMeta && logSettingsMeta.content) {
        logSettingsBasePath = logSettingsMeta.content;
      }

      // Tracks the DB-persisted level so the ephemeral row can show what it will revert to.
      var persistedLogLevel = 'info';

      // loadLoggingConfig populates all logging settings fields from an API response.
      function loadLoggingConfig(d) {
        var levelEl = document.getElementById('log-level');
        var formatEl = document.getElementById('log-format');
        var pathHidden = document.getElementById('log-file-path');
        var pathDisplay = document.getElementById('log-file-path-display');
        var maxSize = document.getElementById('log-file-max-size');
        var maxFiles = document.getElementById('log-file-max-files');
        var maxAge = document.getElementById('log-file-max-age');
        if (levelEl) levelEl.value = d.level || 'info';
        if (formatEl) formatEl.value = d.format || 'json';
        var fp = d.file_path || '';
        if (pathHidden) pathHidden.value = fp;
        if (pathDisplay) pathDisplay.value = fp || '/config/logs/stillwater.log';
        if (maxSize) maxSize.value = d.file_max_size_mb || 10;
        if (maxFiles) maxFiles.value = d.file_max_files || 5;
        if (maxAge) maxAge.value = d.file_max_age_days || 30;
        // Sync toggle and visibility to the current file path state.
        setFileLoggingToggle(fp !== '');
        updateCleanupRow();
        // Track the persisted level for the ephemeral revert label.
        persistedLogLevel = d.level || 'info';
        updateEphemeralRow();
      }

      // setFileLoggingToggle updates the toggle button and shows/hides the file settings section.
      function setFileLoggingToggle(enabled) {
        var btn = document.getElementById('log-to-file-btn');
        var knob = btn ? btn.querySelector('span') : null;
        var settings = document.getElementById('file-logging-settings');
        if (btn) {
          btn.setAttribute('aria-checked', enabled ? 'true' : 'false');
          if (enabled) {
            btn.classList.remove('bg-gray-200', 'dark:bg-gray-600');
            btn.classList.add('bg-blue-600');
          } else {
            btn.classList.remove('bg-blue-600');
            btn.classList.add('bg-gray-200', 'dark:bg-gray-600');
          }
        }
        if (knob) {
          if (enabled) {
            knob.classList.remove('translate-x-0');
            knob.classList.add('translate-x-5');
          } else {
            knob.classList.remove('translate-x-5');
            knob.classList.add('translate-x-0');
          }
        }
        if (settings) {
          if (enabled) settings.classList.remove('hidden');
          else settings.classList.add('hidden');
        }
      }

      // toggleFileLogging handles the Log to file toggle click.
      function toggleFileLogging() {
        var btn = document.getElementById('log-to-file-btn');
        var pathHidden = document.getElementById('log-file-path');
        var pathDisplay = document.getElementById('log-file-path-display');
        var enabled = btn && btn.getAttribute('aria-checked') !== 'true';
        setFileLoggingToggle(enabled);
        if (enabled) {
          var val = pathDisplay ? pathDisplay.value.trim() : '';
          if (!val) val = '/config/logs/stillwater.log';
          if (pathDisplay) pathDisplay.value = val;
          if (pathHidden) pathHidden.value = val;
        } else {
          if (pathHidden) pathHidden.value = '';
        }
      }

      // syncLogFilePath keeps the hidden file_path input in sync with the visible display input.
      function syncLogFilePath(value) {
        var pathHidden = document.getElementById('log-file-path');
        if (pathHidden) pathHidden.value = value;
      }

      // updateCleanupRow shows or hides the cleanup button based on whether rotated files exist.
      function updateCleanupRow() {
        fetch(logSettingsBasePath + '/api/v1/logs/files')
          .then(function(r) { if (!r.ok) throw new Error('failed'); return r.json(); })
          .then(function(files) {
            var row = document.getElementById('log-cleanup-row');
            if (!row) return;
            var hasRotated = files && files.some(function(f) { return !f.is_current; });
            if (hasRotated) row.classList.remove('hidden');
            else row.classList.add('hidden');
          })
          .catch(function() {});
      }

      // updateEphemeralRow shows the "Revert on restart" checkbox when the selected
      // level differs from the persisted level (useful for temporary trace/debug).
      function updateEphemeralRow() {
        var levelEl = document.getElementById('log-level');
        var row = document.getElementById('ephemeral-level-row');
        var revertLabel = document.getElementById('ephemeral-revert-level');
        var checkbox = document.getElementById('ephemeral-level');
        if (!levelEl || !row) return;
        var selected = levelEl.value;
        if (selected !== persistedLogLevel) {
          if (revertLabel) revertLabel.textContent = persistedLogLevel;
          row.classList.remove('hidden');
        } else {
          row.classList.add('hidden');
          if (checkbox) checkbox.checked = false;
        }
      }

      // cleanupLogFiles deletes all rotated log files.
      function cleanupLogFiles() {
        var csrfToken = (typeof window.swCsrfToken === 'function') ? window.swCsrfToken() : '';
        var status = document.getElementById('log-cleanup-status');
        fetch(logSettingsBasePath + '/api/v1/logs/files', {
          method: 'DELETE',
          headers: { 'X-CSRF-Token': csrfToken }
        }).then(function(r) {
          if (!r.ok) throw new Error('cleanup failed');
          return r.json();
        }).then(function(data) {
          if (status) {
            var mb = (data.bytes_freed / (1024 * 1024)).toFixed(1);
            status.textContent = 'Removed ' + data.deleted + ' file' + (data.deleted !== 1 ? 's' : '') + ' (' + mb + ' MB freed)';
            setTimeout(function() { if (status) status.textContent = ''; }, 4000);
          }
          if (typeof loadLogFiles === 'function') loadLogFiles();
          updateCleanupRow();
        }).catch(function() {
          if (status) status.textContent = 'Failed to clean up logs';
        });
      }

  // Re-exports: markup on* handlers and sibling settings modules call
  // these by bare name, so they must live on window.
  window.cleanupLogFiles = cleanupLogFiles;
  window.loadLoggingConfig = loadLoggingConfig;
  window.syncLogFilePath = syncLogFilePath;
  window.toggleFileLogging = toggleFileLogging;
  window.updateCleanupRow = updateCleanupRow;
  window.updateEphemeralRow = updateEphemeralRow;

  window.swLogging = { cleanupLogFiles: cleanupLogFiles, loadLoggingConfig: loadLoggingConfig, syncLogFilePath: syncLogFilePath, toggleFileLogging: toggleFileLogging, updateCleanupRow: updateCleanupRow, updateEphemeralRow: updateEphemeralRow };
})();
