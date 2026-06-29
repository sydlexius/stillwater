// Settings: extracted from the inline settingsConnectionScript() (M55 #1808).
// Behavior-preserving lift out of web/templates/settings.templ; the JS is
// verbatim except for this IIFE wrapper, the load-once guard, the window
// re-export of settingsDeleteConnection_click (called by name from the
// connection card's delete control), and the CSRF read routed through the
// canonical window.swCsrfToken() helper (preferences.js) instead of an inline
// cookie-parse regex.
//
// DOM contract: builds a confirm dialog via the global showConfirmDialog();
// the rendered HTML uses radio inputs named "conn-delete-choice".
// Network: GET/DELETE {base}/api/v1/connections/{id} (DELETE takes optional
//   ?deleteLibraries / ?deleteArtists query params). State-changing DELETE
//   sends the csrf_token cookie as X-CSRF-Token.
//
// Export surface: window.swConnections doubles as the load-once guard.
// settingsDeleteConnection_click is assigned to window because the delete
// control calls it by bare name.
(function () {
  'use strict';

  if (window.swConnections) return;

  var bp = (document.querySelector('meta[name="htmx-base-path"]') || {content: ''}).content;

  function settingsDeleteConnection_click(id) {
    var csrfToken = (typeof window.swCsrfToken === 'function') ? window.swCsrfToken() : '';

    function doDelete(params) {
      var url = bp + "/api/v1/connections/" + id;
      if (params) { url += "?" + params; }
      fetch(url, {
        method: "DELETE",
        headers: {"X-CSRF-Token": csrfToken}
      }).then(function(res) {
        // Refresh just the Connections section instead of a full-page reload
        // (M55 #1339) so the next/ scroll position + ambient backdrop survive.
        if (res.ok) {
          if (typeof window.swRefreshSettingsSection === 'function') {
            window.swRefreshSettingsSection('connections');
          } else {
            console.error('connections.js: swRefreshSettingsSection unavailable; section will not refresh after delete');
            window.location.reload();
          }
          return;
        }
        // The error body may be JSON, plain text, or empty; res.json() rejects
        // on non-JSON, so read text first and parse opportunistically rather
        // than leaving the user with no feedback.
        res.text().then(function(text) {
          var msg = "Failed to delete connection";
          if (text) {
            try {
              var data = JSON.parse(text);
              if (data && data.error) msg = data.error;
            } catch (e) { /* non-JSON body: keep the generic message */ }
          }
          alert(msg);
        }, function() {
          alert("Failed to delete connection");
        });
      }).catch(function() {
        alert("Failed to delete connection");
      });
    }

    fetch(bp + "/api/v1/connections/" + id, {
      headers: {"X-CSRF-Token": csrfToken}
    }).then(function(res) {
      if (!res.ok) {
        showConfirmDialog("Delete this connection?", null, function() { doDelete(""); });
        return;
      }
      return res.json();
    }).then(function(data) {
      if (!data) return;
      // Coerce the counts to integers before they are interpolated into the
      // showConfirmDialog HTML string (rendered with {html: true}); this
      // neutralizes any HTML/script that a non-numeric response field could
      // otherwise inject into the DOM. NaN falls back to 0.
      var libCount = parseInt(data.library_count, 10) || 0;
      var artistCount = parseInt(data.artist_count, 10) || 0;

      if (libCount === 0 && artistCount === 0) {
        showConfirmDialog("Delete this connection?", null, function() { doDelete(""); });
        return;
      }

      var libNoun = libCount === 1 ? "library" : "libraries";
      var artistNoun = artistCount === 1 ? "artist" : "artists";
      var msg = '<p class="mb-3">This connection has <strong>' + libCount + '</strong> ' + libNoun;
      if (artistCount > 0) {
        msg += ' and <strong>' + artistCount + '</strong> ' + artistNoun;
      }
      msg += '.</p>';
      var allLabel = artistCount > 0
          ? ' Delete connection, ' + libNoun + ', and ' + artistNoun
          : ' Delete connection and ' + libNoun;
      var libsLabel = artistCount > 0
          ? ' Delete connection and ' + libNoun + ' (keep ' + artistNoun + ')'
          : ' Delete connection and ' + libNoun;
      var connLabel = artistCount > 0
          ? ' Delete connection only (keep ' + libNoun + ' and ' + artistNoun + ')'
          : ' Delete connection only (keep ' + libNoun + ')';

      msg += '<label class="flex items-center gap-2 mb-1 cursor-pointer">'
        + '<input type="radio" name="conn-delete-choice" value="all" checked class="text-blue-600"/>'
        + allLabel
        + '</label>';
      if (artistCount > 0) {
        msg += '<label class="flex items-center gap-2 mb-1 cursor-pointer">'
          + '<input type="radio" name="conn-delete-choice" value="libs" class="text-blue-600"/>'
          + libsLabel
          + '</label>';
      }
      msg += '<label class="flex items-center gap-2 cursor-pointer">'
        + '<input type="radio" name="conn-delete-choice" value="conn" class="text-blue-600"/>'
        + connLabel
        + '</label>';

      showConfirmDialog(msg, null, function() {
        var checked = document.querySelector('input[name="conn-delete-choice"]:checked');
        var val = checked ? checked.value : "conn";
        if (val === "all") {
          doDelete("deleteLibraries=true&deleteArtists=true");
        } else if (val === "libs") {
          doDelete("deleteLibraries=true");
        } else {
          doDelete("");
        }
      }, {html: true});
    }).catch(function() {
      showConfirmDialog("Delete this connection?", null, function() { doDelete(""); });
    });
  }

  window.settingsDeleteConnection_click = settingsDeleteConnection_click;

  window.swConnections = { deleteConnection: settingsDeleteConnection_click };
})();
