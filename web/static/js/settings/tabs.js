// Settings: Tab switching + history sync (M55 #1808).
// Extracted verbatim from the inline settingsTabScript() that used to live in
// web/templates/settings.templ. Owns the stable settings chrome's tab nav:
// click-to-switch, ?tab= history.pushState, and popstate restore.
//
// DOM contract (settings tab bar + panels in settings.templ):
//   [data-tab="<id>"]        -- a tab button; onclick="switchSettingsTab(event, this)".
//   [data-tab-panel="<id>"]  -- the matching panel, toggled .hidden.
//   #log-viewer              -- the Logs panel poller, whose hx-trigger is
//                               paused/resumed as the Logs tab is left/entered.
//
// Cross-script contract (all probed defensively with typeof so load order does
// not matter): loadCacheStats (image-cache.js), logViewerState / loadLogFiles /
// updateCleanupRow (log-viewer.js), and the global htmx instance. These are
// reached by bare name exactly as they were when both scripts shared the inline
// global scope.
//
// Export surface: window.swSettingsTabs doubles as the load-once guard. The
// inline-handler global switchSettingsTab is also assigned to window because
// each tab button's onclick attribute calls it by name. No other names leaked.
(function () {
  'use strict';

  // Re-init guard: the single window.swSettingsTabs export (assigned at the
  // bottom) doubles as the "already loaded" flag.
  if (window.swSettingsTabs) return;

  function switchSettingsTab(event, el) {
    event.preventDefault();
    var tabId = el.dataset.tab;
    document.querySelectorAll('[data-tab-panel]').forEach(function(p) {
      p.classList.add('hidden');
    });
    var panel = null;
    document.querySelectorAll('[data-tab-panel]').forEach(function(p) {
      if (p.dataset.tabPanel === tabId) {
        panel = p;
      }
    });
    if (panel) panel.classList.remove('hidden');
    document.querySelectorAll('[data-tab]').forEach(function(t) {
      t.classList.remove('bg-white', 'dark:bg-gray-700', 'text-blue-600', 'dark:text-blue-300', 'shadow-sm');
      t.classList.add('text-gray-600', 'dark:text-gray-400', 'hover:text-gray-900', 'dark:hover:text-gray-200', 'hover:bg-white/60', 'dark:hover:bg-gray-700/60');
    });
    el.classList.remove('text-gray-600', 'dark:text-gray-400', 'hover:text-gray-900', 'dark:hover:text-gray-200', 'hover:bg-white/60', 'dark:hover:bg-gray-700/60');
    el.classList.add('bg-white', 'dark:bg-gray-700', 'text-blue-600', 'dark:text-blue-300', 'shadow-sm');
    var settingsTabMeta = document.querySelector('meta[name="htmx-base-path"]');
    var settingsTabBp = settingsTabMeta ? settingsTabMeta.content : '';
    history.pushState(null, '', settingsTabBp + '/settings?tab=' + encodeURIComponent(tabId));
    // Refresh cache stats when switching to the General tab.
    if (tabId === 'general' && typeof loadCacheStats === 'function') {
      loadCacheStats();
    }
    // Pause log polling when leaving Logs tab, resume when entering.
    var logViewer = document.getElementById('log-viewer');
    if (logViewer && typeof logViewerState !== 'undefined') {
      if (tabId === 'logs') {
        // Entering Logs tab: refresh file list, cleanup row, and resume if tab-paused.
        if (typeof loadLogFiles === 'function') loadLogFiles();
        if (typeof updateCleanupRow === 'function') updateCleanupRow();
        if (logViewer.dataset.tabPaused === 'true' && !logViewerState.file) {
          logViewer.dataset.tabPaused = 'false';
          logViewer.setAttribute('hx-trigger', 'load, every 2s, logRefresh from:body');
          if (typeof htmx !== 'undefined') htmx.process(logViewer);
        }
      } else {
        // Leaving Logs tab: pause polling unless user already paused.
        if (!logViewerState.paused) {
          logViewer.dataset.tabPaused = 'true';
          logViewer.setAttribute('hx-trigger', 'logRefresh from:body');
          if (typeof htmx !== 'undefined') htmx.process(logViewer);
        }
      }
    }
  }

  // Inline-handler global: each tab button's onclick attribute calls this by
  // name, so it must live on window (see export-surface note in header).
  window.switchSettingsTab = switchSettingsTab;

  // Restore the active tab when the user navigates back or forward.
  window.addEventListener('popstate', function() {
    var params = new URLSearchParams(window.location.search);
    var tabId = params.get('tab');
    if (!tabId) {
      var firstTab = document.querySelector('[data-tab]');
      if (firstTab) tabId = firstTab.dataset.tab;
    }
    if (!tabId) return;
    var el = document.querySelector('[data-tab="' + tabId + '"]');
    if (!el) {
      // Stale ?tab= value -- fall back to the first available tab.
      el = document.querySelector('[data-tab]');
      if (!el) return;
      tabId = el.dataset.tab;
      history.replaceState(null, '', window.location.pathname + '?tab=' + tabId);
    }
    document.querySelectorAll('[data-tab-panel]').forEach(function(p) {
      p.classList.add('hidden');
    });
    document.querySelectorAll('[data-tab-panel]').forEach(function(p) {
      if (p.dataset.tabPanel === tabId) p.classList.remove('hidden');
    });
    document.querySelectorAll('[data-tab]').forEach(function(t) {
      t.classList.remove('bg-white', 'dark:bg-gray-700', 'text-blue-600', 'dark:text-blue-300', 'shadow-sm');
      t.classList.add('text-gray-600', 'dark:text-gray-400', 'hover:text-gray-900', 'dark:hover:text-gray-200', 'hover:bg-white/60', 'dark:hover:bg-gray-700/60');
    });
    el.classList.remove('text-gray-600', 'dark:text-gray-400', 'hover:text-gray-900', 'dark:hover:text-gray-200', 'hover:bg-white/60', 'dark:hover:bg-gray-700/60');
    el.classList.add('bg-white', 'dark:bg-gray-700', 'text-blue-600', 'dark:text-blue-300', 'shadow-sm');
    // Mirror the side effects from switchSettingsTab for tab-specific state.
    if (tabId === 'general' && typeof loadCacheStats === 'function') {
      loadCacheStats();
    }
    var logViewer = document.getElementById('log-viewer');
    if (logViewer && typeof logViewerState !== 'undefined') {
      if (tabId === 'logs') {
        if (typeof loadLogFiles === 'function') loadLogFiles();
        if (typeof updateCleanupRow === 'function') updateCleanupRow();
        if (logViewer.dataset.tabPaused === 'true' && !logViewerState.file) {
          logViewer.dataset.tabPaused = 'false';
          logViewer.setAttribute('hx-trigger', 'load, every 2s, logRefresh from:body');
          if (typeof htmx !== 'undefined') htmx.process(logViewer);
        }
      } else {
        if (!logViewerState.paused) {
          logViewer.dataset.tabPaused = 'true';
          logViewer.setAttribute('hx-trigger', 'logRefresh from:body');
          if (typeof htmx !== 'undefined') htmx.process(logViewer);
        }
      }
    }
  });

  window.swSettingsTabs = {
    switchTab: switchSettingsTab
  };
})();
