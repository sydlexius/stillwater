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
    notification_enabled: 'true'
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

    // Theme also toggles the "dark" class for Tailwind dark-mode support.
    if (key === 'theme') {
      if (value === 'dark') {
        root.classList.add('dark');
      } else {
        root.classList.remove('dark');
      }
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
  }

  // --- Session redirect helper ---

  // Redirect to login when a 401 is received from a direct fetch call.
  // The session.js handler only covers HTMX requests; this covers fetch().
  function handleSessionExpiry() {
    var loginPath = bp + '/login';
    var path = window.location.pathname;
    if (path === loginPath || path === loginPath + '/') {
      return;
    }
    var returnURL = path + window.location.search;
    window.location.href = loginPath + '?return=' + encodeURIComponent(returnURL);
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
    // Optimistic: apply immediately so the UI feels instant.
    applySingle(key, value);

    // Update the local cache optimistically.
    var cached = readCache() || {};
    cached[key] = value;
    writeCache(cached);

    return fetch(API_BASE + '/' + encodeURIComponent(key), {
      method: 'PUT',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ value: value })
    })
      .then(function (resp) {
        if (!resp.ok) {
          if (resp.status === 401) {
            handleSessionExpiry();
          }
          console.warn('swPreferences: server rejected "' + key + '" (HTTP ' + resp.status + ')');
          return value;
        }
        return resp.json().then(function (data) {
          // The API may normalize the value; update cache with what the
          // server actually stored.
          if (data && data.value !== undefined) {
            cached[key] = data.value;
            writeCache(cached);
            applySingle(key, data.value);
            return data.value;
          }
          return value;
        });
      })
      .catch(function (err) {
        console.warn('swPreferences: failed to save preference "' + key + '"', err);
        return value;
      });
  }

  // Return the cached preferences object, or null if nothing is cached.
  function getCache() {
    return readCache();
  }

  // --- Expose public API ---

  window.swPreferences = {
    load: load,
    set: set,
    applyAll: applyAll,
    getCache: getCache
  };

  // --- Auto-initialize on page load ---
  // Apply cached preferences synchronously (this script runs in <head> or
  // early in <body>) so the first paint already has the right theme/layout.
  var cached = readCache();
  if (cached) {
    applyAll(cached);
  } else {
    applyAll(DEFAULTS);
  }
})();
