// Settings: extracted from the inline logViewerScript() (M55 #1808).
// Behavior-preserving lift out of web/templates/settings.templ; the JS
// is verbatim except for this IIFE wrapper, the load-once guard, the
// window re-exports below, and CSRF reads routed through the canonical
// window.swCsrfToken() helper (preferences.js) instead of an inline
// cookie-parse regex.
//
// DOM contract (ids bound in settings.templ): log-file-picker-row, log-file-select, log-pause-btn, log-search, log-viewer
// Network: /api/v1/logs, /api/v1/logs/files
//
// Export surface: window.swLogViewer doubles as the load-once guard;
// the following are re-exported to window because markup event
// handlers or sibling modules call them by name: clearLogs, downloadLogs, loadLogFiles, selectLogFile, toggleLogLevel, toggleLogPause, updateLogFilters, logViewerState.
(function () {
  'use strict';

  if (window.swLogViewer) return;

      // logViewerState tracks filter selections, pause status, and the active file.
      // file: '' means the live ring buffer; any other value is a historical filename.
      var logViewerState = {
        level: 'info',
        search: '',
        paused: false,
        file: ''
      };

      // Resolve base path for sub-path deployments (e.g. reverse proxy prefix).
      var logBasePath = '';
      var logMeta = document.querySelector('meta[name="htmx-base-path"]');
      if (logMeta && logMeta.content) {
        logBasePath = logMeta.content;
      }

      // toggleLogLevel handles level-filter button clicks.
      // The selected level becomes the minimum displayed level (e.g. selecting
      // "warn" shows warn + error, selecting "debug" shows everything).
      function toggleLogLevel(btn) {
        var level = btn.getAttribute('data-log-level');
        logViewerState.level = level;

        // Update button visuals: buttons at or above the selected level
        // get a ring highlight; buttons below it get dimmed.
        // aria-pressed indicates single selection (only the clicked button).
        var severity = { trace: -1, debug: 0, info: 1, warn: 2, error: 3 };
        var selected = (level in severity) ? severity[level] : 1;
        var buttons = document.querySelectorAll('.log-level-btn');
        for (var i = 0; i < buttons.length; i++) {
          var btnLevel = buttons[i].getAttribute('data-log-level');
          var btnSev = severity[btnLevel] || 0;
          if (btnSev >= selected) {
            buttons[i].classList.remove('opacity-50');
            buttons[i].classList.add('ring-1');
            var ringColor = levelRingClass(btnLevel);
            buttons[i].className = buttons[i].className.replace(/ring-\S+\/50/g, '');
            buttons[i].classList.add(ringColor);
          } else {
            buttons[i].classList.add('opacity-50');
            buttons[i].classList.remove('ring-1');
          }
          // aria-pressed reflects which button was clicked, not the visual range.
          buttons[i].setAttribute('aria-pressed', btnLevel === level ? 'true' : 'false');
        }
        updateLogFilters();
      }

      // levelRingClass returns the Tailwind ring color class for a log level.
      function levelRingClass(level) {
        switch (level) {
          case 'trace': return 'ring-purple-500/50';
          case 'debug': return 'ring-gray-500/50';
          case 'info':  return 'ring-blue-500/50';
          case 'warn':  return 'ring-amber-500/50';
          case 'error': return 'ring-red-500/50';
          default:      return 'ring-gray-500/50';
        }
      }

      // updateLogFilters rebuilds the hx-get URL and re-registers HTMX
      // polling so the periodic trigger uses the updated URL.
      function updateLogFilters() {
        var search = document.getElementById('log-search');
        logViewerState.search = search ? search.value : '';
        var viewer = document.getElementById('log-viewer');
        if (!viewer) return;
        var url = logBasePath + '/api/v1/logs?limit=200&level=' + encodeURIComponent(logViewerState.level);
        if (logViewerState.search) {
          url += '&search=' + encodeURIComponent(logViewerState.search);
        }
        if (logViewerState.file) {
          url += '&file=' + encodeURIComponent(logViewerState.file);
        }
        viewer.setAttribute('hx-get', url);
        // Re-process the element so HTMX picks up the new hx-get URL
        // for subsequent periodic polls ("every 2s" trigger).
        if (typeof htmx !== 'undefined') {
          htmx.process(viewer);
        }
        // Trigger an immediate refresh so the user sees results now.
        if (!logViewerState.paused && typeof htmx !== 'undefined') {
          htmx.trigger(document.body, 'logRefresh');
        }
      }

      // loadLogFiles fetches the available log files and populates the file picker.
      // The picker row is shown only when file logging is configured (non-empty list).
      function loadLogFiles() {
        fetch(logBasePath + '/api/v1/logs/files')
          .then(function(r) { if (!r.ok) throw new Error('failed'); return r.json(); })
          .then(function(files) {
            var row = document.getElementById('log-file-picker-row');
            var sel = document.getElementById('log-file-select');
            if (!row || !sel) return;
            if (!files || files.length === 0) {
              row.classList.add('hidden');
              return;
            }
            var current = logViewerState.file;
            // Rebuild options using DOM methods (no innerHTML with dynamic content).
            while (sel.options.length > 0) { sel.remove(0); }
            var live = document.createElement('option');
            live.value = '';
            live.textContent = 'Live (current)';
            sel.appendChild(live);
            files.forEach(function(f) {
              if (f.is_current) return; // already represented by "Live (current)"
              var opt = document.createElement('option');
              opt.value = f.name;
              var mb = (f.size / (1024 * 1024)).toFixed(1);
              opt.textContent = f.name + ' - ' + mb + ' MB';
              sel.appendChild(opt);
            });
            // Restore previous selection or fall back to live.
            sel.value = current;
            if (sel.value !== current) { sel.value = ''; }
            row.classList.remove('hidden');
            // Also update the cleanup button visibility.
            if (typeof updateCleanupRow === 'function') updateCleanupRow();
          })
          .catch(function() {
            var row = document.getElementById('log-file-picker-row');
            if (row) row.classList.add('hidden');
          });
      }

      // selectLogFile switches the viewer between live ring buffer and a
      // historical log file. Pass an empty string to return to live view.
      function selectLogFile(filename) {
        logViewerState.file = filename;
        var viewer = document.getElementById('log-viewer');
        var pauseBtn = document.getElementById('log-pause-btn');
        var clearBtn = document.querySelector('[onclick="clearLogs()"]');
        if (filename) {
          // Historical file: disable polling and action buttons that only
          // apply to the live ring buffer.
          if (viewer) {
            viewer.setAttribute('hx-trigger', 'logRefresh from:body');
            if (typeof htmx !== 'undefined') htmx.process(viewer);
          }
          if (pauseBtn) pauseBtn.disabled = true;
          if (clearBtn) clearBtn.disabled = true;
        } else {
          // Live view: restore polling (unless user had manually paused).
          if (viewer && !logViewerState.paused) {
            viewer.setAttribute('hx-trigger', 'load, every 2s, logRefresh from:body');
            if (typeof htmx !== 'undefined') htmx.process(viewer);
          }
          if (pauseBtn) pauseBtn.disabled = false;
          if (clearBtn) clearBtn.disabled = false;
        }
        updateLogFilters();
      }

      // toggleLogPause pauses or resumes the HTMX polling.
      function toggleLogPause() {
        logViewerState.paused = !logViewerState.paused;
        var btn = document.getElementById('log-pause-btn');
        var viewer = document.getElementById('log-viewer');
        if (logViewerState.paused) {
          if (btn) {
            btn.textContent = 'Resume';
            btn.setAttribute('aria-pressed', 'true');
            btn.classList.add('bg-amber-600', 'text-white', 'border-amber-600');
            btn.classList.remove('dark:hover:bg-gray-700', 'hover:bg-gray-100');
          }
          // Remove polling trigger to stop updates; keep manual refresh.
          if (viewer) viewer.setAttribute('hx-trigger', 'logRefresh from:body');
          if (typeof htmx !== 'undefined' && viewer) htmx.process(viewer);
        } else {
          if (btn) {
            btn.textContent = 'Pause';
            btn.setAttribute('aria-pressed', 'false');
            btn.classList.remove('bg-amber-600', 'text-white', 'border-amber-600');
            btn.classList.add('dark:hover:bg-gray-700', 'hover:bg-gray-100');
          }
          // Restore polling trigger.
          if (viewer) viewer.setAttribute('hx-trigger', 'load, every 2s, logRefresh from:body');
          if (typeof htmx !== 'undefined' && viewer) htmx.process(viewer);
        }
      }

      // clearLogs sends a DELETE to clear the ring buffer and refreshes the viewer.
      function clearLogs() {
        var csrfToken = (typeof window.swCsrfToken === 'function') ? window.swCsrfToken() : '';
        fetch(logBasePath + '/api/v1/logs', {
          method: 'DELETE',
          headers: { 'X-CSRF-Token': csrfToken, 'HX-Request': 'true' }
        }).then(function(r) {
          if (!r.ok) throw new Error('clear failed');
          return r.text();
        }).then(function(html) {
          var viewer = document.getElementById('log-viewer');
          if (viewer) viewer.innerHTML = html;
        }).catch(function() {
          if (typeof showToast === 'function') {
            showToast('Failed to clear logs.');
          }
        });
      }

      // downloadLogs fetches the current filtered entries as JSON and triggers
      // a browser download as a .json file.
      function downloadLogs() {
        var url = logBasePath + '/api/v1/logs?limit=500&level=' + encodeURIComponent(logViewerState.level);
        if (logViewerState.search) {
          url += '&search=' + encodeURIComponent(logViewerState.search);
        }
        fetch(url, {
          headers: { 'Accept': 'application/json' }
        }).then(function(r) {
          if (!r.ok) throw new Error('fetch failed');
          return r.json();
        }).then(function(data) {
          var blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' });
          var a = document.createElement('a');
          a.href = URL.createObjectURL(blob);
          a.download = 'stillwater-logs-' + new Date().toISOString().slice(0, 19).replace(/:/g, '-') + '.json';
          document.body.appendChild(a);
          a.click();
          document.body.removeChild(a);
          URL.revokeObjectURL(a.href);
        }).catch(function() {
          if (typeof showToast === 'function') {
            showToast('Failed to download logs.');
          }
        });
      }

      // Load the file picker and cleanup row when the Logs tab is active on page load.
      (function() {
        var panel = document.querySelector('[data-tab-panel="logs"]');
        if (panel && !panel.classList.contains('hidden')) {
          loadLogFiles();
          if (typeof updateCleanupRow === 'function') updateCleanupRow();
        }
      })();

      // Capture scroll position BEFORE the HTMX swap replaces content,
      // so we know whether the user was near the bottom.
      document.addEventListener('htmx:beforeSwap', function(evt) {
        if (evt.detail && evt.detail.target && evt.detail.target.id === 'log-viewer') {
          var v = evt.detail.target;
          v.dataset.wasNearBottom = (v.scrollHeight - v.scrollTop - v.clientHeight < 50) ? 'true' : 'false';
        }
      });

      // Auto-scroll: after new content settles, scroll to bottom only if the
      // user was near the bottom before the swap.
      document.addEventListener('htmx:afterSettle', function(evt) {
        if (evt.detail && evt.detail.target && evt.detail.target.id === 'log-viewer') {
          if (evt.detail.target.dataset.wasNearBottom === 'true') {
            evt.detail.target.scrollTop = evt.detail.target.scrollHeight;
          }
        }
      });

  // Re-exports: markup on* handlers and sibling settings modules call
  // these by bare name, so they must live on window.
  window.clearLogs = clearLogs;
  window.downloadLogs = downloadLogs;
  window.loadLogFiles = loadLogFiles;
  window.selectLogFile = selectLogFile;
  window.toggleLogLevel = toggleLogLevel;
  window.toggleLogPause = toggleLogPause;
  window.updateLogFilters = updateLogFilters;
  window.logViewerState = logViewerState;

  window.swLogViewer = { clearLogs: clearLogs, downloadLogs: downloadLogs, loadLogFiles: loadLogFiles, selectLogFile: selectLogFile, toggleLogLevel: toggleLogLevel, toggleLogPause: toggleLogPause, updateLogFilters: updateLogFilters, logViewerState: logViewerState };
})();
