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
    var body = JSON.stringify({ value: JSON.stringify(views) });
    fetch(prefURL(), {
      method: 'PUT',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
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

  // refreshChipsRow triggers an HTMX reload of the artist-content region so
  // the server re-renders the saved view chips based on the updated preference.
  // The chips row is inside #artist-content (rendered as part of the table or
  // grid) so a content swap picks it up without a full page navigation.
  function refreshChipsRow() {
    var target = document.querySelector(CONTENT_TARGET);
    if (target && window.htmx) {
      htmx.trigger(target, 'sw:filter-applied');
    }
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
        refreshChipsRow();
        if (window.showToast) {
          showToast('View "' + name + '" saved.', 'success');
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
          if (window.showToast) showToast('Could not delete view.', 'error');
          return;
        }
        refreshChipsRow();
        if (window.showToast) {
          showToast('View "' + name + '" deleted.', 'info');
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
