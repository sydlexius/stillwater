// Settings: extracted from the inline settingsLibraryScript() (M55 #1808).
// Behavior-preserving lift out of web/templates/settings.templ; the JS
// is verbatim except for this IIFE wrapper, the load-once guard, the
// window re-exports below, and CSRF reads routed through the canonical
// window.swCsrfToken() helper (preferences.js) instead of an inline
// cookie-parse regex.
//
// DOM contract (ids bound in settings.templ): settings-add-library-btn, settings-library-form, settings-library-list
// Network: /api/v1/connections/, /api/v1/libraries, /api/v1/libraries/
//
// Export surface: window.swSettingsLibrary doubles as the load-once guard;
// the following are re-exported to window because markup event
// handlers or sibling modules call them by name: onSettingsLibrarySaved, runLibraryOp, settingsDeleteLibrary_click, updateLibraryFSMode, updateLibraryLockNFO, updateLibraryPollInterval.
(function () {
  'use strict';

  if (window.swSettingsLibrary) return;

      function onSettingsLibrarySaved() {
        var form = document.getElementById("settings-library-form");
        if (form) { form.reset(); form.classList.add("hidden"); }
        var btn = document.getElementById("settings-add-library-btn");
        if (btn) btn.classList.remove("hidden");
        refreshSettingsLibraryList();
      }

      var bp = (document.querySelector('meta[name="htmx-base-path"]') || {content: ''}).content;

      function refreshSettingsLibraryList() {
        var csrfToken;
        if (typeof window.swCsrfToken === 'function') {
          csrfToken = window.swCsrfToken();
        } else {
          console.error("swCsrfToken unavailable - preferences.js may have failed to load; state-changing requests will 403");
          csrfToken = '';
        }
        var sourceLogos = {emby: bp + "/static/img/logos/emby-128.png", jellyfin: bp + "/static/img/logos/jellyfin.svg", lidarr: bp + "/static/img/logos/lidarr.svg"};
        var sourceNames = {emby: "Emby", jellyfin: "Jellyfin", lidarr: "Lidarr"};
        fetch(bp + "/api/v1/libraries", {
          headers: {"X-CSRF-Token": csrfToken}
        }).then(function(res) { return res.json(); })
        .then(function(libs) {
          var list = document.getElementById("settings-library-list");
          // Read the i18n-translated badge text from the container's
          // data attribute. Templ does NOT interpolate { } inside
          // <script> blocks, so we cannot use { t(ctx, ...) } here --
          // that produces literal text in the rendered DOM.
          var connectionLabel = (list && list.getAttribute("data-connection-label")) || "Connection";
          // Defensive HTML-escape: the source is a trusted t(ctx, ...)
          // string rendered into a data attribute, but it is concatenated
          // into innerHTML below so escape it to neutralize any future
          // locale value that contains HTML-special characters.
          var _escDiv = document.createElement("div");
          _escDiv.textContent = connectionLabel;
          var safeConnectionLabel = _escDiv.innerHTML;
          // Same escape applied to the Lock NFOs label and tooltip,
          // which are also concatenated into innerHTML below.
          var lockNfoLabel = (list && list.dataset.lockNfoLabel) || "Lock NFOs";
          // lockNfoTitle backs the popover body. If the i18n key is
          // missing or empty for any reason, fall back to the label
          // rather than letting the popover render with only a
          // "Read more" link and no body text.
          var lockNfoTitle = (list && list.dataset.lockNfoTitle) || lockNfoLabel;
          var helpReadMore = (list && list.dataset.helpReadMore) || "Read more";
          // Single-source the docs anchor: the templ render reads it
          // from the matching ContextHelp call site; here we read it
          // from the data attribute the templ also populates so the
          // two render paths can never drift on rename.
          var lockNfoDocAnchor = (list && list.dataset.lockNfoDocAnchor) || "settings-libraries-libraries-lock-nfo-label";
          _escDiv.textContent = lockNfoLabel;
          var safeLockNfoLabel = _escDiv.innerHTML;
          _escDiv.textContent = lockNfoTitle;
          var safeLockNfoTitle = _escDiv.innerHTML;
          _escDiv.textContent = helpReadMore;
          var safeHelpReadMore = _escDiv.innerHTML;
          var fsModeTitle = (list && list.dataset.fsModeTitle) || "Filesystem monitoring mode";
          _escDiv.textContent = fsModeTitle;
          var safeFsModeTitle = _escDiv.innerHTML;
          var pollIntervalTitle = (list && list.dataset.pollIntervalTitle) || "Poll interval";
          _escDiv.textContent = pollIntervalTitle;
          var safePollIntervalTitle = _escDiv.innerHTML;
          var resyncLabel = (list && list.dataset.resync) || "Re-sync Artists";
          _escDiv.textContent = resyncLabel;
          var safeResyncLabel = _escDiv.innerHTML;
          var scanLabel = (list && list.dataset.scan) || "Scan Library";
          _escDiv.textContent = scanLabel;
          var safeScanLabel = _escDiv.innerHTML;
          if (!libs || libs.length === 0) {
            var emptyText = (list && list.dataset.empty) || "No libraries configured.";
            _escDiv.textContent = emptyText;
            list.innerHTML = '<p id="settings-no-libraries" class="text-sm text-gray-400 dark:text-gray-500 italic">' + _escDiv.innerHTML + '</p>';
            return;
          }
          var html = "";
          libs.forEach(function(lib) {
            var div = document.createElement("div");
            div.textContent = lib.name;
            var safeName = div.innerHTML;
            div.textContent = lib.path;
            var safePath = div.innerHTML;
            div.textContent = lib.type;
            var safeType = div.innerHTML;
            var pathLine = lib.path ? '<div class="text-xs text-gray-500 dark:text-gray-400">' + safePath + '</div>' : '';
            var connectionBadge = lib.connection_id ? '<span class="inline-flex items-center rounded-full bg-gray-100 dark:bg-gray-700 px-2 py-0.5 text-xs text-gray-600 dark:text-gray-300">' + safeConnectionLabel + '</span>' : '';
            var sourceBadge = '';
            if (lib.source && lib.source !== 'manual' && sourceNames[lib.source]) {
              sourceBadge = '<span class="inline-flex items-center gap-1 rounded-full bg-gray-100 dark:bg-gray-700 px-2 py-0.5 text-xs text-gray-600 dark:text-gray-300">'
                + '<img src="' + sourceLogos[lib.source] + '" class="h-3.5 w-3.5" alt=""/>'
                + sourceNames[lib.source]
                + '</span>';
            }
            var spinnerSvg = '<svg class="hidden animate-spin h-3 w-3" viewBox="0 0 24 24" fill="none"><circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle><path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z"></path></svg>';
            var modeSelect = '';
            if (lib.path) {
              var fsMode = lib.fs_watch || 0;
              var supportsNotify = lib.fs_notify_supported;
              var selClass = 'text-xs rounded border-gray-300 dark:border-gray-600 dark:bg-gray-700 dark:text-gray-200 focus:ring-blue-500 px-1.5 py-1';
              modeSelect = '<select class="' + selClass + '" title="' + safeFsModeTitle + '" aria-label="' + safeFsModeTitle + '" onchange="updateLibraryFSMode(&apos;' + lib.id + '&apos;, parseInt(this.value, 10))">'
                + '<option value="0"' + (fsMode === 0 ? ' selected' : '') + '>Off</option>';
              if (supportsNotify) {
                modeSelect += '<option value="1"' + (fsMode === 1 ? ' selected' : '') + '>Watch</option>';
              }
              modeSelect += '<option value="2"' + (fsMode === 2 ? ' selected' : '') + '>Poll</option>';
              if (supportsNotify) {
                modeSelect += '<option value="3"' + (fsMode === 3 ? ' selected' : '') + '>Watch + Poll</option>';
              }
              modeSelect += '</select>';
              if (fsMode === 2 || fsMode === 3) {
                var pi = lib.fs_poll_interval || 60;
                modeSelect += ' <select class="' + selClass + '" title="' + safePollIntervalTitle + '" aria-label="' + safePollIntervalTitle + '" onchange="updateLibraryPollInterval(&apos;' + lib.id + '&apos;, parseInt(this.value, 10))">'
                  + '<option value="60"' + (pi === 60 ? ' selected' : '') + '>1m</option>'
                  + '<option value="300"' + (pi === 300 ? ' selected' : '') + '>5m</option>'
                  + '<option value="900"' + (pi === 900 ? ' selected' : '') + '>15m</option>'
                  + '<option value="1800"' + (pi === 1800 ? ' selected' : '') + '>30m</option>'
                  + '</select>';
              }
            }
            // Lock NFOs toggle. Server-rendered rows include this via
            // settingsLibraryRow; replicate it here so client-side
            // refreshes (refreshSettingsLibraryList) preserve the
            // control. The icon span is decorative (aria-hidden,
            // no tabindex/aria-label) so it does not pollute the
            // checkbox's accessible name; the long help text is
            // supplied via aria-describedby on the input,
            // referencing an sr-only sibling that lives OUTSIDE
            // the <label> for the same reason. Keep this layout
            // in sync with the templ-rendered settingsLibraryRow.
            // onchange passes `this` so updateLibraryLockNFO can
            // revert the checkbox if the PUT fails.
            var lockNfo = '';
            if (lib.path) {
              var helpId = 'lock-nfo-help-' + lib.id;
              var descId = 'lock-nfo-desc-' + lib.id;
              var checked = lib.nfo_lock_data ? ' checked' : '';
              // Mirror the templ ContextHelp render for the static row at
              // settings.templ:3383. The JS path is exercised only for
              // libraries added without a page refresh; on refresh the
              // templ render takes over and these strings disappear.
              // The sr-only span at descId is the always-present
              // description target for the checkbox -- the popover
              // owned by the help button is collapsed until clicked,
              // so the checkbox needs its own stable description.
              lockNfo = '<span class="inline-flex items-center gap-1">'
                + '<label class="inline-flex items-center gap-1 text-xs text-gray-700 dark:text-gray-300 cursor-pointer">'
                + '<input type="checkbox" class="rounded border-gray-300 dark:border-gray-600 dark:bg-gray-700 focus:ring-blue-500"' + checked + ' aria-describedby="' + descId + '" onchange="updateLibraryLockNFO(&apos;' + lib.id + '&apos;, this, this.checked)"/>'
                + '<span>' + safeLockNfoLabel + '</span>'
                + '</label>'
                + '<span id="' + descId + '" class="sr-only">' + safeLockNfoTitle + '</span>'
                + '<span class="sw-context-help" id="' + helpId + '">'
                + '<button type="button" class="sw-context-help-btn" aria-label="' + safeLockNfoLabel + '" aria-expanded="false" aria-controls="' + helpId + '-popover" onclick="swContextHelpToggle(this)" onkeydown="if(event.key===&apos;Escape&apos;)swContextHelpClose(this)">?</button>'
                + '<span id="' + helpId + '-popover" role="tooltip" class="sw-context-help-popover" aria-hidden="true">' + safeLockNfoTitle
                + '<a href="https://sydlexius.github.io/stillwater/reference/settings-by-tab/#' + encodeURIComponent(lockNfoDocAnchor) + '" target="_blank" rel="noopener" class="sw-context-help-link">' + safeHelpReadMore + '</a>'
                + '</span>'
                + '</span>'
                + '</span>';
            }
            var populateBtn = '';
            if (lib.connection_id) {
              populateBtn = '<button type="button" id="populate-btn-' + lib.id + '" class="text-xs px-2 py-1 rounded bg-blue-600 text-white hover:bg-blue-700 transition-colors inline-flex items-center gap-1" onclick="runLibraryOp(&apos;' + lib.connection_id + '&apos;, &apos;' + lib.id + '&apos;, &apos;populate&apos;)">'
                + spinnerSvg.replace('class="hidden', 'id="populate-spinner-' + lib.id + '" class="hidden')
                + '<span id="populate-label-' + lib.id + '">' + safeResyncLabel + '</span></button>';
            }
            var scanBtn = '';
            if (lib.connection_id) {
              scanBtn = '<button type="button" id="scan-btn-' + lib.id + '" class="text-xs px-2 py-1 rounded bg-gray-600 text-white hover:bg-gray-700 transition-colors inline-flex items-center gap-1" onclick="runLibraryOp(&apos;' + lib.connection_id + '&apos;, &apos;' + lib.id + '&apos;, &apos;scan&apos;)">'
                + spinnerSvg.replace('class="hidden', 'id="scan-spinner-' + lib.id + '" class="hidden')
                + '<span id="scan-label-' + lib.id + '">' + safeScanLabel + '</span></button>';
            }
            html += '<div class="flex items-center justify-between rounded-lg border border-gray-200 dark:border-gray-700 px-4 py-3" id="settings-lib-' + lib.id + '">'
              + '<div>'
              + '<div class="font-medium text-sm text-gray-900 dark:text-gray-100">' + safeName + '</div>'
              + pathLine
              + '<div class="flex items-center gap-1.5 mt-1">'
              + '<span class="inline-flex items-center rounded-full bg-gray-100 dark:bg-gray-700 px-2 py-0.5 text-xs text-gray-600 dark:text-gray-300">' + safeType + '</span>'
              + connectionBadge
              + sourceBadge
              + '</div>'
              + '</div>'
              + '<div class="flex items-center gap-2">'
              + modeSelect
              + lockNfo
              + populateBtn
              + scanBtn
              + '<button type="button" class="text-xs text-red-600 dark:text-red-400 hover:text-red-800 dark:hover:text-red-300" onclick="settingsDeleteLibrary_click(&apos;' + lib.id + '&apos;)">'
              + 'Remove'
              + '</button>'
              + '</div>'
              + '</div>';
          });
          list.innerHTML = html;
        });
      }

      function runLibraryOp(connID, libID, operation) {
        var csrfToken;
        if (typeof window.swCsrfToken === 'function') {
          csrfToken = window.swCsrfToken();
        } else {
          console.error("swCsrfToken unavailable - preferences.js may have failed to load; state-changing requests will 403");
          csrfToken = '';
        }
        var btn = document.getElementById(operation + '-btn-' + libID);
        var spinner = document.getElementById(operation + '-spinner-' + libID);
        var label = document.getElementById(operation + '-label-' + libID);
        var endpoint = bp + "/api/v1/connections/" + connID + "/libraries/" + libID + "/" + operation;

        fetch(endpoint, {
          method: "POST",
          headers: {"X-CSRF-Token": csrfToken}
        }).then(function(res) {
          if (res.status === 409) {
            showToast("Operation already running for this library");
            return;
          }
          if (res.status === 202) {
            if (btn) btn.disabled = true;
            if (spinner) spinner.classList.remove('hidden');
            if (label) label.textContent = operation === 'populate' ? 'Populating...' : 'Scanning...';
            pollLibraryOp(libID, operation, btn, spinner, label);
            return;
          }
          if (!res.ok) {
            res.text().then(function(body) {
              var message = "Operation failed (HTTP " + res.status + ")";
              if (body) {
                try {
                  var data = JSON.parse(body);
                  if (data && data.error) {
                    message = data.error;
                  }
                } catch (e) {
                  // Body is not JSON; keep default message.
                }
              }
              showToast(message);
            }).catch(function() {
              showToast("Operation failed (HTTP " + res.status + ")");
            });
            return;
          }
          res.json().then(function(data) {
            showToast((data && data.error) || "Operation failed");
          }).catch(function() {
            showToast("Operation failed");
          });
        }).catch(function() {
          showToast("Network error");
        });
      }

      function pollLibraryOp(libID, operation, btn, spinner, label) {
        // Status polling is a GET (pollAsyncStatus sends no CSRF token), so this
        // function needs no swCsrfToken; the previously-computed-but-unused token
        // block was removed to avoid a false-alarm console.error (#2109 review).
        function resetUI() {
          if (btn) btn.disabled = false;
          if (spinner) spinner.classList.add('hidden');
          if (label) label.textContent = operation === 'populate' ? 'Re-sync Artists' : 'Scan Library';
        }
        pollAsyncStatus(bp + "/api/v1/libraries/" + libID + "/operation/status", {
          onData: function(data) {
            if (data.status === 'completed') {
              resetUI();
              showSuccessToast(data.message || 'Operation completed');
              if (operation === 'populate') refreshSettingsLibraryList();
              return true;
            }
            if (data.status === 'failed') {
              resetUI();
              showToast(data.message || 'Operation failed');
              return true;
            }
            if (data.status === 'idle') {
              resetUI();
              showToast("Operation state lost (server may have restarted)");
              return true;
            }
            return false;
          },
          onHTTPError: function(status) {
            resetUI();
            showToast("Status check failed (HTTP " + status + ")");
          },
          onNetworkError: function() {
            resetUI();
            showToast("Network error while checking operation status");
          }
        }, {
          headers: {"X-CSRF-Token": csrfToken}
        });
      }

      function updateLibraryFSMode(id, mode) {
        var csrfToken;
        if (typeof window.swCsrfToken === 'function') {
          csrfToken = window.swCsrfToken();
        } else {
          console.error("swCsrfToken unavailable - preferences.js may have failed to load; state-changing requests will 403");
          csrfToken = '';
        }
        fetch(bp + "/api/v1/libraries/" + id, {
          method: "PUT",
          headers: {"Content-Type": "application/json", "X-CSRF-Token": csrfToken},
          body: JSON.stringify({fs_watch: mode})
        }).then(function(res) {
          if (res.ok) {
            var labels = {0: "Off", 1: "Watch", 2: "Poll", 3: "Watch + Poll"};
            showSuccessToast("Monitoring set to " + (labels[mode] || "Off"));
            refreshSettingsLibraryList();
          } else {
            showToast("Failed to update library");
          }
        }).catch(function() {
          showToast("Network error");
        });
      }

      function updateLibraryPollInterval(id, interval) {
        var csrfToken;
        if (typeof window.swCsrfToken === 'function') {
          csrfToken = window.swCsrfToken();
        } else {
          console.error("swCsrfToken unavailable - preferences.js may have failed to load; state-changing requests will 403");
          csrfToken = '';
        }
        fetch(bp + "/api/v1/libraries/" + id, {
          method: "PUT",
          headers: {"Content-Type": "application/json", "X-CSRF-Token": csrfToken},
          body: JSON.stringify({fs_poll_interval: interval})
        }).then(function(res) {
          if (res.ok) {
            showSuccessToast("Poll interval updated");
          } else {
            showToast("Failed to update poll interval");
          }
        }).catch(function() {
          showToast("Network error");
        });
      }

      function updateLibraryLockNFO(id, input, enabled) {
        // Read translated toast text from the library list container's
        // dataset (set by templ at render time). The fallback strings are
        // only used if the container is missing or the data attribute is
        // absent -- both indicate a template/render bug, not a user-
        // observable English fallback.
        var list = document.getElementById("settings-library-list");
        var msgEnabled = (list && list.dataset.lockNfoEnabled) || "NFO locking enabled for library";
        var msgDisabled = (list && list.dataset.lockNfoDisabled) || "NFO locking disabled for library";
        var msgFailed = (list && list.dataset.lockNfoFailed) || "Failed to update NFO locking";
        var msgNetErr = (list && list.dataset.netError) || "Network error";
        // Capture the previous state before the optimistic UI update
        // is allowed to stand. The browser already flipped `input.checked`
        // on the user's click; if the PUT fails we restore it so the UI
        // never lies about whether future writeback will stamp lockdata.
        var previous = !enabled;
        // Disable the input while the PUT is in flight so a rapid
        // on->off->on sequence cannot race -- without this, an older
        // request can resolve last and persist a stale value, leaving
        // the server's nfo_lock_data out of sync with what the UI
        // shows. Re-enabled in the .finally() handler below regardless
        // of outcome so the control is never permanently stuck.
        if (input) input.disabled = true;
        var csrfToken;
        if (typeof window.swCsrfToken === 'function') {
          csrfToken = window.swCsrfToken();
        } else {
          console.error("swCsrfToken unavailable - preferences.js may have failed to load; state-changing requests will 403");
          csrfToken = '';
        }
        fetch(bp + "/api/v1/libraries/" + id, {
          method: "PUT",
          headers: {"Content-Type": "application/json", "X-CSRF-Token": csrfToken},
          body: JSON.stringify({nfo_lock_data: enabled})
        }).then(function(res) {
          if (res.ok) {
            showSuccessToast(enabled ? msgEnabled : msgDisabled);
          } else {
            if (input) input.checked = previous;
            showToast(msgFailed);
          }
        }).catch(function() {
          if (input) input.checked = previous;
          showToast(msgNetErr);
        }).finally(function() {
          if (input) input.disabled = false;
        });
      }

      function settingsDeleteLibrary_click(id) {
        var csrfToken;
        if (typeof window.swCsrfToken === 'function') {
          csrfToken = window.swCsrfToken();
        } else {
          console.error("swCsrfToken unavailable - preferences.js may have failed to load; state-changing requests will 403");
          csrfToken = '';
        }
        function doDelete(deleteArtists) {
          var url = bp + "/api/v1/libraries/" + id;
          if (deleteArtists) { url += "?deleteArtists=true"; }
          fetch(url, {
            method: "DELETE",
            headers: {"X-CSRF-Token": csrfToken}
          }).then(function(res) {
            if (res.ok) {
              refreshSettingsLibraryList();
            } else {
              res.json().then(function(data) { alert(data.error || "Failed to delete library"); });
            }
          }).catch(function() {
            alert("Failed to delete library");
          });
        }

        fetch(bp + "/api/v1/libraries/" + id, {
          headers: {"X-CSRF-Token": csrfToken}
        }).then(function(res) {
          if (!res.ok) {
            showConfirmDialog("Remove this library?", null, function() { doDelete(false); });
            return;
          }
          return res.json();
        })
        .then(function(data) {
          if (!data) return;
          var count = data.artist_count || 0;
          if (count === 0) {
            showConfirmDialog("Remove this library?", null, function() { doDelete(false); });
            return;
          }
          var noun = count === 1 ? "artist" : "artists";
          var msg = '<p class="mb-3">This library has <strong>' + count + '</strong> ' + noun + '.</p>'
            + '<label class="flex items-center gap-2 mb-1 cursor-pointer">'
            + '<input type="radio" name="lib-delete-choice" value="delete" checked class="text-blue-600"/> Delete ' + noun
            + '</label>'
            + '<label class="flex items-center gap-2 cursor-pointer">'
            + '<input type="radio" name="lib-delete-choice" value="keep" class="text-blue-600"/> Keep ' + noun + ' (unassign from library)'
            + '</label>';
          showConfirmDialog(msg, null, function() {
            var checked = document.querySelector('input[name="lib-delete-choice"]:checked');
            doDelete(checked && checked.value === 'delete');
          }, {html: true});
        })
        .catch(function() {
          showConfirmDialog("Remove this library?", null, function() { doDelete(false); });
        });
      }


  // Re-exports: markup on* handlers and sibling settings modules call
  // these by bare name, so they must live on window.
  window.onSettingsLibrarySaved = onSettingsLibrarySaved;
  window.runLibraryOp = runLibraryOp;
  window.settingsDeleteLibrary_click = settingsDeleteLibrary_click;
  window.updateLibraryFSMode = updateLibraryFSMode;
  window.updateLibraryLockNFO = updateLibraryLockNFO;
  window.updateLibraryPollInterval = updateLibraryPollInterval;

  window.swSettingsLibrary = { onSettingsLibrarySaved: onSettingsLibrarySaved, runLibraryOp: runLibraryOp, settingsDeleteLibrary_click: settingsDeleteLibrary_click, updateLibraryFSMode: updateLibraryFSMode, updateLibraryLockNFO: updateLibraryLockNFO, updateLibraryPollInterval: updateLibraryPollInterval };
})();
