// Client-side preference loader with server sync.
// Loads user preferences from the API, caches in sessionStorage for instant
// render on subsequent navigations, and applies them as data attributes on
// <html> so CSS can respond without a round-trip.
//
// Public API (exposed as window.swPreferences):
//   load()            -- fetch from API, update cache, apply to DOM
//   set(key, value)   -- PUT single preference, update cache, apply to DOM
//   applyAll(prefs)   -- apply a full preferences object to the DOM
//   getCache()        -- return cached preferences from sessionStorage (or null)
(function () {
  'use strict';

  var STORAGE_KEY = 'sw-preferences';

  // Read the base path from the meta tag so sub-path deployments work.
  var bpEl = document.querySelector('meta[name="htmx-base-path"]');
  var bp = bpEl ? bpEl.content : '';
  var API_BASE = bp + '/api/v1/preferences';

  // Default preferences used when both cache and API are unavailable.
  var DEFAULTS = {
    theme: 'dark',
    glass_intensity: 'medium',
    sidebar_state: 'full',
    content_width: 'narrow',
    font_family: 'inter',
    font_size: 'medium',
    letter_spacing: 'normal',
    thumbnail_size: 'medium',
    reduced_motion: 'system',
    lite_mode: 'off',
    language: 'en',
    notification_enabled: 'true',
    bg_opacity: '65'
  };

  // Mapping from preference key to the data attribute name set on <html>.
  // Keys not listed here are stored in the cache but not applied to the DOM.
  var ATTR_MAP = {
    theme: 'data-theme',
    glass_intensity: 'data-glass',
    sidebar_state: 'data-sidebar',
    content_width: 'data-width',
    font_family: 'data-font-family',
    font_size: 'data-font-size',
    letter_spacing: 'data-letter-spacing',
    thumbnail_size: 'data-thumbnail-size',
    reduced_motion: 'data-motion',
    lite_mode: 'data-lite'
  };

  // --- sessionStorage helpers ---

  function readCache() {
    try {
      var raw = sessionStorage.getItem(STORAGE_KEY);
      if (raw) return JSON.parse(raw);
    } catch (e) {
      console.warn('swPreferences: failed to read cache', e);
    }
    return null;
  }

  function writeCache(prefs) {
    try {
      sessionStorage.setItem(STORAGE_KEY, JSON.stringify(prefs));
    } catch (e) {
      // Storage full or unavailable -- non-fatal.
    }
  }

  // --- CSRF token helper ---

  function csrfToken() {
    var match = document.cookie.match(/(?:^|;\s*)csrf_token=([^;]*)/);
    return match ? match[1] : '';
  }

  // --- Lite mode auto-detection ---

  function detectLiteMode() {
    var dominated = false;
    if (typeof navigator !== 'undefined') {
      if (navigator.hardwareConcurrency && navigator.hardwareConcurrency < 4) {
        dominated = true;
      }
      // navigator.deviceMemory is only available in some browsers (Chrome).
      if (navigator.deviceMemory && navigator.deviceMemory < 4) {
        dominated = true;
      }
    }
    return dominated;
  }

  // --- DOM application ---

  // Apply a single key/value preference to the document element.
  function applySingle(key, value) {
    var root = document.documentElement;
    var attr = ATTR_MAP[key];

    // Special handling: lite_mode "auto" resolves based on device capability.
    if (key === 'lite_mode' && value === 'auto') {
      value = detectLiteMode() ? 'on' : 'off';
    }

    if (attr) {
      root.setAttribute(attr, value);
    }

    // bg_opacity updates the --sw-glass-bg CSS custom property directly.
    if (key === 'bg_opacity') {
      var pct = parseInt(value, 10) / 100;
      if (isNaN(pct)) pct = 0.65;
      var isDark = root.classList.contains('dark');
      if (isDark) {
        root.style.setProperty('--sw-glass-bg', 'rgba(30, 41, 59, ' + pct + ')');
      } else {
        root.style.setProperty('--sw-glass-bg', 'rgba(255, 255, 255, ' + pct + ')');
      }
    }

    // Theme also toggles the "dark" class for Tailwind dark-mode support.
    // "system" follows the OS preference via matchMedia.
    if (key === 'theme') {
      var isDark = value === 'dark' ||
        (value === 'system' && window.matchMedia('(prefers-color-scheme: dark)').matches);
      if (isDark) {
        root.classList.add('dark');
      } else {
        root.classList.remove('dark');
      }
      // Recompute theme-dependent background color after theme change.
      var cached = readCache() || {};
      var opacityVal = cached.bg_opacity || DEFAULTS.bg_opacity || '65';
      applySingle('bg_opacity', opacityVal);
    }
  }

  // Apply all preferences to the DOM.
  function applyAll(prefs) {
    if (!prefs) return;
    var keys = Object.keys(ATTR_MAP);
    for (var i = 0; i < keys.length; i++) {
      var key = keys[i];
      if (prefs.hasOwnProperty(key)) {
        applySingle(key, prefs[key]);
      }
    }
    // bg_opacity is not in ATTR_MAP (it sets a CSS custom property, not a
    // data attribute), so apply it explicitly after the ATTR_MAP loop.
    if (prefs.hasOwnProperty('bg_opacity')) {
      applySingle('bg_opacity', prefs.bg_opacity);
    }
  }

  // --- Session redirect helper ---

  // Redirect to login when a 401 is received from a direct fetch call.
  // The session.js handler only covers HTMX requests; this covers fetch().
  // The login page is at the root (GET {basePath}/). No loop guard needed
  // because preferences.js is only loaded by Layout (authenticated pages),
  // not the login template.
  function handleSessionExpiry() {
    var loginUrl = bp + '/';
    // Clear cached preferences so the next user does not see stale settings.
    clearCache();
    // Strip base path prefix so the server does not double-prefix on redirect.
    var relPath = window.location.pathname;
    if (bp && (relPath === bp || relPath.indexOf(bp + '/') === 0)) {
      relPath = relPath.substring(bp.length) || '/';
    }
    var returnURL = relPath + window.location.search + window.location.hash;
    window.location.href = loginUrl + '?return=' + encodeURIComponent(returnURL);
  }

  // --- API communication ---

  // Fetch all preferences from the server, update cache, and apply.
  // Returns a Promise that resolves with the preferences object.
  function load() {
    // Step 1: Apply cached preferences immediately (no flash of defaults).
    var cached = readCache();
    if (cached) {
      applyAll(cached);
    } else {
      // No cache at all -- apply compiled defaults so the page is not unstyled.
      applyAll(DEFAULTS);
    }

    // Step 2: Fetch fresh data from the API.
    return fetch(API_BASE, { credentials: 'same-origin' })
      .then(function (resp) {
        if (!resp.ok) {
          if (resp.status === 401) {
            handleSessionExpiry();
          }
          console.warn('swPreferences: API returned ' + resp.status + ' on load');
          return cached || DEFAULTS;
        }
        return resp.json();
      })
      .then(function (prefs) {
        writeCache(prefs);
        applyAll(prefs);
        document.dispatchEvent(new CustomEvent('sw:preferences-applied'));
        return prefs;
      })
      .catch(function (err) {
        console.warn('swPreferences: failed to load preferences from API', err);
        return cached || DEFAULTS;
      });
  }

  // Persist a single preference change to the server, update cache, and apply.
  // Returns a Promise that resolves with the updated preference value.
  function set(key, value) {
    // Save previous value for rollback if the server rejects.
    var cached = readCache() || {};
    var previousValue = cached[key] || DEFAULTS[key];

    // Optimistic: apply immediately so the UI feels instant.
    applySingle(key, value);
    cached[key] = value;
    writeCache(cached);

    return fetch(API_BASE + '/' + encodeURIComponent(key), {
      method: 'PUT',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrfToken() },
      body: JSON.stringify({ value: value })
    })
      .then(function (resp) {
        if (!resp.ok) {
          if (resp.status === 401) {
            handleSessionExpiry();
          }
          console.warn('swPreferences: server rejected "' + key + '" (HTTP ' + resp.status + '), reverting');
          // Revert to previous value so client stays consistent with server.
          cached[key] = previousValue;
          writeCache(cached);
          applySingle(key, previousValue);
          document.dispatchEvent(new CustomEvent('sw:preferences-applied'));
          return previousValue;
        }
        return resp.json().then(function (data) {
          // The API may normalize the value; update cache with what the
          // server actually stored.
          if (data && data.value !== undefined) {
            cached[key] = data.value;
            writeCache(cached);
            applySingle(key, data.value);
            document.dispatchEvent(new CustomEvent('sw:preferences-applied'));
            return data.value;
          }
          document.dispatchEvent(new CustomEvent('sw:preferences-applied'));
          return value;
        });
      })
      .catch(function (err) {
        console.warn('swPreferences: failed to save preference "' + key + '", reverting', err);
        cached[key] = previousValue;
        writeCache(cached);
        applySingle(key, previousValue);
        document.dispatchEvent(new CustomEvent('sw:preferences-applied'));
        return previousValue;
      });
  }

  // Return the cached preferences object, or null if nothing is cached.
  function getCache() {
    return readCache();
  }

  // Clear the preference cache. Called on logout to prevent the next user
  // from briefly seeing the previous user's appearance settings.
  function clearCache() {
    try {
      sessionStorage.removeItem(STORAGE_KEY);
    } catch (e) {
      // Storage unavailable -- non-fatal.
    }
  }

  // --- Expose public API ---

  window.swPreferences = {
    load: load,
    set: set,
    applyAll: applyAll,
    getCache: getCache,
    clearCache: clearCache
  };

  // --- Auto-initialize on page load ---
  // Step 1 (synchronous): Apply cached preferences immediately so the first
  // paint has the right theme/layout without waiting for the API.
  var initCached = readCache();
  if (initCached) {
    applyAll(initCached);
  } else {
    applyAll(DEFAULTS);
  }

  // Step 2 (async): Fetch fresh preferences from the API once the DOM is ready.
  // This syncs server-stored preferences into the cache and applies any changes.
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', function () { load(); });
  } else {
    load();
  }
})();
