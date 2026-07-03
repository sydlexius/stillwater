// Settings: Image Cache controls (M55 #1806 spike).
// Extracted verbatim from the inline cacheScript() that used to live in
// web/templates/settings.templ. Drives the Image Cache card on the Settings
// General tab: loads cache stats, saves the max-size selection, and clears the
// cache.
//
// DOM contract (ids owned by the Image Cache card in settings.templ):
//   #cache-stats   -- status line; gets the "<n> MB used (...)" summary or an
//                     error message, with the matching text/red class swapped.
//   #cache-max-size-- <select> whose onchange="saveCacheMaxSize(this.value)"
//                     persists the chosen cache size.
//   #cache-status  -- transient "Saved" / "Cleared ..." / error feedback span.
// Clear is triggered by a button with onclick="clearCache()".
//
// Network contract (all base-path aware via meta[name="htmx-base-path"]):
//   GET    {base}/api/v1/settings/cache/stats   -- {size_bytes,file_count,artist_count}
//   PUT    {base}/api/v1/settings               -- {"cache.image.max_size_mb": value}
//   DELETE {base}/api/v1/settings/cache         -- {bytes_freed,files_deleted}
// State-changing requests send the csrf_token cookie as X-CSRF-Token, read via
// the canonical window.swCsrfToken() helper from preferences.js (loaded in
// layout.templ before this module) rather than an inline cookie-parse regex.
//
// Export surface: window.swImageCache doubles as the load-once guard. The two
// inline-handler globals (saveCacheMaxSize, clearCache) are also assigned to
// window because the card's onchange/onclick attributes call them by name --
// the same inline-handler-global pattern ContextHelp uses for
// window.swContextHelpToggle. No other names are leaked.
(function () {
  'use strict';

  // Re-init guard: the single window.swImageCache export (assigned at the
  // bottom) doubles as the "already loaded" flag.
  if (window.swImageCache) return;

  // Resolve base path for sub-path deployments (e.g. reverse proxy prefix).
  var cacheBasePath = '';
  var cacheBaseMeta = document.querySelector('meta[name="htmx-base-path"]');
  if (cacheBaseMeta && cacheBaseMeta.content) {
    cacheBasePath = cacheBaseMeta.content;
  }

  function loadCacheStats() {
    fetch(cacheBasePath + '/api/v1/settings/cache/stats')
      .then(function (r) { if (!r.ok) throw new Error('failed'); return r.json(); })
      .then(function (data) {
        var mb = (data.size_bytes / (1024 * 1024)).toFixed(1);
        var el = document.getElementById('cache-stats');
        if (el) {
          el.textContent = mb + ' MB used (' + data.file_count + ' files, ' + data.artist_count + ' artists)';
          el.className = 'text-sm text-gray-600 dark:text-gray-400';
        }
      })
      .catch(function () {
        var el = document.getElementById('cache-stats');
        if (el) {
          el.textContent = 'Failed to load cache stats';
          el.className = 'text-sm text-red-500';
        }
      });
  }

  function saveCacheMaxSize(value) {
    var csrfToken;
    if (typeof window.swCsrfToken === 'function') {
      csrfToken = window.swCsrfToken();
    } else {
      console.error("swCsrfToken unavailable - preferences.js may have failed to load; state-changing requests will 403");
      csrfToken = '';
    }
    fetch(cacheBasePath + '/api/v1/settings', {
      method: 'PUT',
      headers: {
        'Content-Type': 'application/json',
        'X-CSRF-Token': csrfToken
      },
      body: JSON.stringify({ 'cache.image.max_size_mb': value })
    }).then(function (r) {
      if (!r.ok) throw new Error('failed');
      var el = document.getElementById('cache-status');
      el.textContent = 'Saved';
      el.className = 'text-sm text-green-600 dark:text-green-400';
      setTimeout(function () { el.textContent = ''; }, 2000);
      loadCacheStats();
    }).catch(function () {
      var el = document.getElementById('cache-status');
      el.textContent = 'Failed to save';
      el.className = 'text-sm text-red-500';
    });
  }

  function clearCache() {
    if (!confirm('Clear all cached images? Pathless artists will lose their local image copies.')) return;
    var csrfToken;
    if (typeof window.swCsrfToken === 'function') {
      csrfToken = window.swCsrfToken();
    } else {
      console.error("swCsrfToken unavailable - preferences.js may have failed to load; state-changing requests will 403");
      csrfToken = '';
    }
    fetch(cacheBasePath + '/api/v1/settings/cache', {
      method: 'DELETE',
      headers: { 'X-CSRF-Token': csrfToken }
    })
      .then(function (r) { if (!r.ok) throw new Error('failed'); return r.json(); })
      .then(function (data) {
        var mb = (data.bytes_freed / (1024 * 1024)).toFixed(1);
        var el = document.getElementById('cache-status');
        el.textContent = 'Cleared ' + data.files_deleted + ' files (' + mb + ' MB)';
        el.className = 'text-sm text-green-600 dark:text-green-400';
        setTimeout(function () { el.textContent = ''; }, 3000);
        loadCacheStats();
      })
      .catch(function () {
        var el = document.getElementById('cache-status');
        el.textContent = 'Failed to clear cache';
        el.className = 'text-sm text-red-500';
      });
  }

  // Inline-handler globals: the card's onchange/onclick attributes call these
  // by name, so they must live on window (see export-surface note in header).
  window.saveCacheMaxSize = saveCacheMaxSize;
  window.clearCache = clearCache;
  // loadCacheStats is also probed as a global by settingsTabScript (it does
  // `typeof loadCacheStats === 'function'` on tab switch / popstate to refresh
  // the General tab), so it must be reachable by bare name too. Assigning it to
  // window keeps that cross-script call working exactly as it did inline.
  window.loadCacheStats = loadCacheStats;

  // Load cache stats when the General tab is visible on page load.
  (function () {
    var panel = document.querySelector('[data-tab-panel="general"]');
    if (panel && !panel.classList.contains('hidden')) {
      loadCacheStats();
    }
  })();

  window.swImageCache = {
    loadStats: loadCacheStats,
    saveMaxSize: saveCacheMaxSize,
    clear: clearCache
  };
})();
