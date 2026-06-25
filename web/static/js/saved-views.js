// Saved filter views controller for the next/ artists page (M55 #1777).
// Provides apply, save, and delete operations against the saved_views user
// preference, then re-renders the chips row without a full page reload.
//
// Public API (exposed as window.swSavedViews):
//   applySavedView(params)    -- apply a saved view by updating the URL and
//                                triggering the HTMX artist-content reload
//   openSaveViewModal()       -- show the modal to name + save the current view
//   closeSaveViewModal()      -- hide the save-view modal
//   deleteSavedView(name)     -- remove a named view and refresh the chips row
(function () {
  'use strict';

  var PREF_KEY = 'saved_views';
  var PREF_URL = '/api/v1/preferences/' + PREF_KEY;
  var CONTENT_TARGET = '#artist-content';

  // basePath reads the deployment base path from the <meta> tag injected by
  // the layout, matching the pattern used by ArtistsPageScripts.
  function basePath() {
    var meta = document.querySelector('meta[name="htmx-base-path"]');
    return meta ? meta.content : '';
  }

  // prefURL returns the absolute URL for the saved_views preference endpoint,
  // accounting for the deployment base path.
  function prefURL() {
    return basePath() + PREF_URL;
  }

  // fetchSavedViews fetches the current saved views from the server and calls
  // callback(views) on success or callback(null) on error. views is an array
  // of {name, params, created_at} objects.
  function fetchSavedViews(callback) {
    fetch(prefURL(), { credentials: 'same-origin' })
      .then(function (resp) {
        if (!resp.ok) {
          console.error('[saved-views] GET preference failed', resp.status);
          callback(null);
          return;
        }
        return resp.json();
      })
      .then(function (data) {
        if (!data) return;
        var raw = data.value || '[]';
        try {
          callback(JSON.parse(raw));
        } catch (e) {
          console.error('[saved-views] malformed saved views JSON', e);
          callback([]);
        }
      })
      .catch(function (err) {
        console.error('[saved-views] fetch error', err);
        callback(null);
      });
  }

  // putSavedViews replaces the entire saved_views preference with views (array).
  // Calls callback(ok) when the write settles.
  function putSavedViews(views, callback) {
    var csrf = typeof window.swCsrfToken === 'function' ? window.swCsrfToken() : '';
    var body = JSON.stringify({ value: JSON.stringify(views) });
    fetch(prefURL(), {
      method: 'PUT',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
      body: body,
    })
      .then(function (resp) {
        if (!resp.ok) {
          console.error('[saved-views] PUT preference failed', resp.status);
          callback(false);
          return;
        }
        callback(true);
      })
      .catch(function (err) {
        console.error('[saved-views] PUT error', err);
        callback(false);
      });
  }

  // refreshChipsRow triggers an HTMX reload of #artist-content. Used by
  // applySavedView to reload the filtered artist list after a view is applied.
  // Save/delete operations call renderChipsRow directly because #saved-views-row
  // lives OUTSIDE #artist-content and is not reached by a content swap.
  function refreshChipsRow() {
    var target = document.querySelector(CONTENT_TARGET);
    if (target && window.htmx) {
      htmx.trigger(target, 'sw:filter-applied');
    }
  }

  // renderChipsRow rebuilds #saved-views-row client-side from a views array
  // ({name, params, created_at}[]). Shows the row when views is non-empty,
  // hides it when empty. Called after a successful save or delete so the chip
  // appears / disappears without waiting for a full page reload.
  function renderChipsRow(views) {
    var row = document.getElementById('saved-views-row');
    if (!row) return;

    // Remove existing chip spans (keep the static "Saved:" label span).
    var chips = row.querySelectorAll('span.group');
    for (var i = 0; i < chips.length; i++) {
      row.removeChild(chips[i]);
    }

    if (!views || views.length === 0) {
      row.classList.add('hidden');
      return;
    }
    row.classList.remove('hidden');

    var svgNS = 'http://www.w3.org/2000/svg';
    var xPath = 'M6.28 5.22a.75.75 0 0 0-1.06 1.06L8.94 10l-3.72 3.72a.75.75 0 1 0 1.06 1.06L10 11.06l3.72 3.72a.75.75 0 1 0 1.06-1.06L11.06 10l3.72-3.72a.75.75 0 0 0-1.06-1.06L10 8.94 6.28 5.22Z';

    views.forEach(function(sv) {
      // Closure captures sv so event handlers don't need inline JS strings.
      var group = document.createElement('span');
      group.className = 'group relative inline-flex items-center';

      var applyBtn = document.createElement('button');
      applyBtn.type = 'button';
      applyBtn.title = 'Apply view: ' + sv.name;
      applyBtn.setAttribute('aria-label', 'Apply saved view: ' + sv.name);
      applyBtn.className = 'inline-flex items-center gap-1 rounded-l-full rounded-r-none border border-[var(--swd-line)] bg-white/5 px-3 py-1 text-xs text-[var(--swd-ink-2)] hover:bg-white/10 transition-colors';
      applyBtn.textContent = sv.name;
      applyBtn.onclick = function() {
        if (window.swSavedViews) window.swSavedViews.applySavedView(sv.params);
      };

      var delBtn = document.createElement('button');
      delBtn.type = 'button';
      delBtn.title = 'Delete view: ' + sv.name;
      delBtn.setAttribute('aria-label', 'Delete saved view: ' + sv.name);
      delBtn.className = 'inline-flex items-center rounded-l-none rounded-r-full border border-l-0 border-[var(--swd-line)] bg-white/5 px-1.5 py-1 text-xs text-gray-400 hover:bg-red-500/20 hover:text-red-400 transition-colors opacity-0 group-hover:opacity-100 focus:opacity-100';
      delBtn.onclick = function() {
        if (window.swSavedViews) window.swSavedViews.deleteSavedView(sv.name);
      };

      var svg = document.createElementNS(svgNS, 'svg');
      svg.setAttribute('class', 'h-3 w-3');
      svg.setAttribute('viewBox', '0 0 20 20');
      svg.setAttribute('fill', 'currentColor');
      svg.setAttribute('aria-hidden', 'true');
      var path = document.createElementNS(svgNS, 'path');
      path.setAttribute('d', xPath);
      svg.appendChild(path);
      delBtn.appendChild(svg);

      group.appendChild(applyBtn);
      group.appendChild(delBtn);
      row.appendChild(group);
    });
  }

  // applySavedView parses params (a query string such as
  // "filter_type=%2Bperson&sort=name") and applies it as the active URL state,
  // then triggers the HTMX content refresh. The flyout's initFromURL is called
  // on the next page render so the flyout chips stay in sync.
  function applySavedView(params) {
    var url = new URL(window.location.href);

    // Clear all existing filter and sort params managed by the artists page
    // so the saved view replaces (not appends to) the current state.
    var savedParams = new URLSearchParams(params || '');

    // Remove existing filter_* params, sort, order, search, and ids so the
    // saved view starts from a clean slate. Leave page and page_size.
    var keysToRemove = [];
    url.searchParams.forEach(function (_, k) {
      if (
        k.indexOf('filter_') === 0 ||
        k === 'sort' ||
        k === 'order' ||
        k === 'search' ||
        k === 'ids'
      ) {
        keysToRemove.push(k);
      }
    });
    keysToRemove.forEach(function (k) { url.searchParams.delete(k); });

    // Apply the saved params.
    savedParams.forEach(function (v, k) {
      url.searchParams.append(k, v);
    });

    // Reset to page 1 when applying a saved view so stale pagination does not
    // produce an empty result set.
    url.searchParams.set('page', '1');

    // Sync the search input value so the text box reflects the saved search.
    var searchInput = document.getElementById('artist-search');
    if (searchInput) {
      searchInput.value = url.searchParams.get('search') || '';
    }

    history.pushState(null, '', url.toString());
    refreshChipsRow();
  }

  // openSaveViewModal opens the save-view modal and pre-populates it with the
  // current URL filter state. The modal input is focused for immediate typing.
  function openSaveViewModal() {
    var modal = document.getElementById('save-view-modal');
    var input = document.getElementById('save-view-name-input');
    var err = document.getElementById('save-view-error');
    if (!modal) {
      console.error('[saved-views] #save-view-modal not found in DOM');
      return;
    }
    if (err) err.textContent = '';
    if (input) {
      input.value = '';
      modal.classList.remove('hidden');
      // Focus after the browser's show transition settles.
      setTimeout(function () { input.focus(); }, 50);
    } else {
      modal.classList.remove('hidden');
    }
  }

  // closeSaveViewModal hides the save-view modal without saving.
  function closeSaveViewModal() {
    var modal = document.getElementById('save-view-modal');
    if (modal) modal.classList.add('hidden');
  }

  // saveCurrentView reads the current URL state, appends a new view with the
  // given name, PUTs the updated array, and refreshes the chips row. On error
  // an inline message is shown in the modal.
  function saveCurrentView(name) {
    if (!name || name.length > 50) {
      showSaveError('Name must be 1-50 characters.');
      return;
    }

    // Capture the current filter state as a query string snapshot. Exclude
    // page and page_size from the snapshot -- they are position parameters, not
    // filter state.
    var url = new URL(window.location.href);
    var snapshot = new URLSearchParams();
    url.searchParams.forEach(function (v, k) {
      if (k !== 'page' && k !== 'page_size' && k !== 'view') {
        snapshot.append(k, v);
      }
    });

    fetchSavedViews(function (views) {
      if (views === null) {
        showSaveError('Could not load saved views. Try again.');
        return;
      }

      // Reject duplicate names.
      for (var i = 0; i < views.length; i++) {
        if (views[i].name === name) {
          showSaveError('A view named "' + name + '" already exists.');
          return;
        }
      }

      if (views.length >= 20) {
        showSaveError('You have reached the maximum of 20 saved views.');
        return;
      }

      var newView = {
        name: name,
        params: snapshot.toString(),
        created_at: new Date().toISOString(),
      };
      views.push(newView);

      putSavedViews(views, function (ok) {
        if (!ok) {
          showSaveError('Could not save view. Try again.');
          return;
        }
        closeSaveViewModal();
        renderChipsRow(views);
        if (window.showSuccessToast) {
          showSuccessToast('View "' + name + '" saved.');
        }
      });
    });
  }

  // deleteSavedView removes the named view from the preference and refreshes
  // the chips row. Shows a toast on success.
  function deleteSavedView(name) {
    fetchSavedViews(function (views) {
      if (views === null) return;
      var updated = views.filter(function (v) { return v.name !== name; });
      putSavedViews(updated, function (ok) {
        if (!ok) {
          if (window.showToast) showToast('Could not delete view.');
          return;
        }
        renderChipsRow(updated);
        if (window.showSuccessToast) {
          showSuccessToast('View "' + name + '" deleted.');
        }
      });
    });
  }

  // showSaveError displays an inline error message inside the save-view modal.
  function showSaveError(msg) {
    var err = document.getElementById('save-view-error');
    if (err) err.textContent = msg;
  }

  // Wire the save-view modal's form submission.
  document.addEventListener('DOMContentLoaded', function () {
    var modal = document.getElementById('save-view-modal');
    if (!modal) return;

    var form = modal.querySelector('form');
    if (form) {
      form.addEventListener('submit', function (e) {
        e.preventDefault();
        var input = document.getElementById('save-view-name-input');
        saveCurrentView(input ? input.value.trim() : '');
      });
    }

    // Escape key closes the modal.
    modal.addEventListener('keydown', function (e) {
      if (e.key === 'Escape') closeSaveViewModal();
    });

    // Clicking the backdrop closes the modal.
    var backdrop = modal.querySelector('[data-save-view-backdrop]');
    if (backdrop) {
      backdrop.addEventListener('click', closeSaveViewModal);
    }
  });

  // Expose public API.
  window.swSavedViews = {
    applySavedView: applySavedView,
    openSaveViewModal: openSaveViewModal,
    closeSaveViewModal: closeSaveViewModal,
    saveCurrentView: saveCurrentView,
    deleteSavedView: deleteSavedView,
  };
})();
