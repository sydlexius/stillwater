// fanart-manage.js - parameterized fanart management for the artist-detail
// surfaces (stable Images tab / artist page, stable image-management page, and
// the next/ Artwork carousel + Manage-artwork modal).
//
// Extracted (M55 #1336, 4B) from four inline scripts so every channel shares
// one copy:
//   - setFanartPrimary (web/templates/artist_images_tab.templ)
//   - the fanart-gallery batch-delete IIFE (web/templates/artist_detail.templ)
//   - the FanartManagementGallery reorder + batch-delete IIFE
//     (web/templates/backdrop_management.templ)
//   - the fanart-bulk-save multi-select IIFE (web/templates/image_search.templ)
//
// All behavior is driven by document-level event delegation keyed off stable
// markers so it survives htmx fragment swaps without re-running an inline
// script. Containers opt in with:
//   data-sw-fanart-gallery  - a gallery wrapper holding .fanart-cb checkboxes,
//                             an inner #fanart-delete-selected button, optional
//                             .fanart-move-btn reorder buttons, and the i18n
//                             data-* strings (label/confirm/toast keys).
//   data-sw-fanart-search   - a search-results wrapper holding .fanart-select-cb
//                             checkboxes (multi-select batch download).
// Per-button hooks (data-set-primary-*, .fanart-move-btn data-*) are read off
// the element itself, mirroring the original inline code. No Go values are
// interpolated; CSRF + base path are read the same way the inline code did.
(function () {
  "use strict";

  // Delegates to the canonical reader (preferences.js) instead of re-inventing
  // the cookie regex, matching the other first-party modules.
  function csrfToken() {
    return typeof window.swCsrfToken === "function" ? window.swCsrfToken() : "";
  }

  function basePath() {
    var meta = document.querySelector('meta[name="htmx-base-path"]');
    return meta ? meta.content : "";
  }

  function pluralize(count, one, other) {
    var tpl = count === 1 ? one : other;
    // String.prototype.replace with a /g regex (ES5) rather than replaceAll
    // (ES2021), so the module stays parseable on the oldest supported WebViews.
    return tpl.replace(/\{count\}/g, String(count));
  }

  // ---- Set-as-primary (reorder so the chosen slot becomes index 0) ----------
  function setFanartPrimary(el) {
    var d = el.dataset;
    if (!confirm(d.confirm || "Set this image as the primary fanart?")) return;
    var idx = parseInt(d.setPrimaryIndex, 10);
    var count = parseInt(d.setPrimaryCount, 10);
    var id = d.setPrimaryArtist;
    if (isNaN(idx) || isNaN(count) || count <= 0) {
      alert(
        d.reorderError ||
          "Unable to reorder: invalid image data. Please reload the page.",
      );
      return;
    }
    var token = csrfToken();
    if (!token) {
      alert(d.sessionExpired || "Session expired. Please reload the page and try again.");
      return;
    }
    // Build new order: selected index first, then the rest in original order.
    var order = [idx];
    for (var i = 0; i < count; i++) {
      if (i !== idx) order.push(i);
    }
    fetch(basePath() + "/api/v1/artists/" + id + "/images/fanart/reorder", {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-CSRF-Token": token },
      body: JSON.stringify({ order: order }),
      credentials: "same-origin",
    })
      .then(function (r) {
        if (r.ok) {
          window.location.reload();
        } else {
          return r.text().then(function (body) {
            alert(
              (d.setPrimaryFailed || "Failed to set primary (HTTP %d): %s")
                .replace("%d", r.status)
                .replace("%s", body),
            );
          });
        }
      })
      .catch(function (err) {
        alert(
          (d.setPrimaryNetwork || "Network error setting primary: %s").replace(
            "%s",
            err.message,
          ),
        );
      });
  }

  // ---- Batch delete (multi-select checkboxes -> DELETE /fanart/batch) -------
  function refreshDeleteButton(gallery) {
    var deleteBtn = gallery.querySelector("#fanart-delete-selected");
    if (!deleteBtn) return;
    var checked = gallery.querySelectorAll(".fanart-cb:checked");
    if (checked.length > 0) {
      deleteBtn.classList.remove("hidden");
      deleteBtn.textContent = pluralize(
        checked.length,
        gallery.dataset.labelDeleteNOne || "Delete 1 selected",
        gallery.dataset.labelDeleteNOther || "Delete {count} selected",
      );
    } else {
      deleteBtn.classList.add("hidden");
    }
  }

  function batchDelete(gallery) {
    var checked = gallery.querySelectorAll(".fanart-cb:checked");
    if (checked.length === 0) return;
    if (
      !confirm(
        pluralize(
          checked.length,
          gallery.dataset.confirmDeleteNOne || "Delete 1 fanart image?",
          gallery.dataset.confirmDeleteNOther || "Delete {count} fanart images?",
        ),
      )
    )
      return;
    var token = csrfToken();
    if (!token) {
      alert(
        gallery.dataset.sessionExpired ||
          "Session expired. Please reload the page and try again.",
      );
      return;
    }
    var indices = [];
    for (var i = 0; i < checked.length; i++) {
      indices.push(parseInt(checked[i].value, 10));
    }
    var id = gallery.dataset.artistId;
    fetch(basePath() + "/api/v1/artists/" + id + "/images/fanart/batch", {
      method: "DELETE",
      headers: { "Content-Type": "application/json", "X-CSRF-Token": token },
      body: JSON.stringify({ indices: indices }),
      credentials: "same-origin",
    })
      .then(function (r) {
        if (r.ok) {
          window.location.reload();
          return;
        }
        // Some galleries surface a JSON {error}; others surface raw text. Try
        // JSON first, then fall back to the HTTP-status string, matching the
        // original per-gallery behavior.
        return r
          .clone()
          .json()
          .then(function (b) {
            alert(
              (gallery.dataset.toastDeleteFailed || "Failed to delete selected images: %s").replace(
                "%s",
                (b && b.error) || "unknown error",
              ),
            );
          })
          .catch(function () {
            return r.text().then(function (body) {
              var httpTpl = gallery.dataset.toastDeleteHttpFailed;
              alert(
                httpTpl
                  ? httpTpl.replace("%s", r.status)
                  : (gallery.dataset.toastDeleteFailed ||
                      "Failed to delete selected images: %s").replace("%s", body),
              );
            });
          });
      })
      .catch(function (err) {
        var netTpl =
          gallery.dataset.toastDeleteNetwork || gallery.dataset.toastNetworkError;
        alert(
          netTpl
            ? netTpl.replace("%s", err.message)
            : "Network error. Check your connection and try again.",
        );
      });
  }

  // ---- Reorder via move up/down buttons -------------------------------------
  function reorder(gallery, btn) {
    var idx = parseInt(btn.dataset.index, 10);
    var dir = btn.dataset.direction;
    var id = btn.dataset.artistId;
    var total = gallery.querySelectorAll(".fanart-cb").length;

    var order = [];
    for (var i = 0; i < total; i++) order.push(i);
    var swapWith = dir === "up" ? idx - 1 : idx + 1;
    if (swapWith < 0 || swapWith >= total) return;
    order[idx] = swapWith;
    order[swapWith] = idx;

    // Guard an empty CSRF token (missing/expired cookie) so we surface a
    // friendly reload prompt instead of letting the server reject the request
    // with a confusing 403 "invalid CSRF token". Mirrors batchDelete/setFanartPrimary.
    var token = csrfToken();
    if (!token) {
      alert(
        gallery.dataset.sessionExpired ||
          "Session expired. Please reload the page and try again.",
      );
      return;
    }

    fetch(basePath() + "/api/v1/artists/" + id + "/images/fanart/reorder", {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-CSRF-Token": token },
      body: JSON.stringify({ order: order }),
      credentials: "same-origin",
    })
      .then(function (r) {
        if (r.ok) {
          window.location.reload();
          return;
        }
        r.json()
          .then(function (b) {
            alert(
              (gallery.dataset.toastReorderFailed || "Reorder failed: %s").replace(
                "%s",
                (b && b.error) || "unknown error",
              ),
            );
          })
          .catch(function () {
            alert(
              (gallery.dataset.toastReorderHttpFailed || "Reorder failed (HTTP %s)").replace(
                "%s",
                r.status,
              ),
            );
          });
      })
      .catch(function () {
        alert(
          gallery.dataset.toastNetworkError ||
            "Network error. Check your connection and try again.",
        );
      });
  }

  // ---- Multi-select bulk download (provider/web search results) -------------
  function refreshBulkBar(container) {
    var bar = document.getElementById("fanart-bulk-bar");
    var countEl = document.getElementById("fanart-bulk-count");
    if (!bar || !countEl) return;
    var checked = container.querySelectorAll(".fanart-select-cb:checked");
    if (checked.length > 0) {
      bar.classList.remove("hidden");
      countEl.textContent = pluralize(
        checked.length,
        bar.dataset.labelSelectedOne || "1 selected",
        bar.dataset.labelSelectedOther || "{count} selected",
      );
    } else {
      bar.classList.add("hidden");
    }
  }

  function bulkSave(container) {
    var bar = document.getElementById("fanart-bulk-bar");
    var countEl = document.getElementById("fanart-bulk-count");
    var checked = container.querySelectorAll(".fanart-select-cb:checked");
    if (checked.length === 0) return;
    var urls = [];
    for (var i = 0; i < checked.length; i++) {
      urls.push(checked[i].dataset.url);
    }
    // Guard an empty CSRF token before mutating the bar UI / sending the POST,
    // so a missing cookie surfaces a reload prompt rather than a server 403.
    var token = csrfToken();
    if (!token) {
      alert(
        (bar && bar.dataset.sessionExpired) ||
          "Session expired. Please reload the page and try again.",
      );
      return;
    }
    if (bar) bar.classList.add("hidden");
    if (countEl) countEl.textContent = (bar && bar.dataset.labelSaving) || "Saving...";
    if (bar) bar.classList.remove("hidden");
    fetch(
      basePath() +
        "/api/v1/artists/" +
        container.dataset.artistId +
        "/images/fanart/fetch-batch",
      {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "X-CSRF-Token": token,
        },
        body: JSON.stringify({ urls: urls }),
        credentials: "same-origin",
      },
    )
      .then(function (r) {
        if (r.ok) {
          window.location.reload();
          return;
        }
        alert(
          (bar && bar.dataset.toastSaveFailed) ||
            "Failed to save selected images. Please try again.",
        );
        // Restore the "N selected" label so the bar is not stuck on "Saving...".
        refreshBulkBar(container);
      })
      .catch(function (err) {
        // A rejected fetch (network error) would otherwise leave the bar frozen
        // on "Saving..." with no feedback. Surface it and restore the bar.
        var netTpl = bar && bar.dataset.toastNetworkError;
        alert(
          netTpl
            ? netTpl.replace("%s", err.message)
            : "Network error. Check your connection and try again.",
        );
        refreshBulkBar(container);
      });
  }

  // ---- Delegation ------------------------------------------------------------
  document.addEventListener("change", function (e) {
    var t = e.target;
    if (t.classList && t.classList.contains("fanart-cb")) {
      var gallery = t.closest("[data-sw-fanart-gallery]");
      if (gallery) refreshDeleteButton(gallery);
    } else if (t.classList && t.classList.contains("fanart-select-cb")) {
      var search = t.closest("[data-sw-fanart-search]");
      if (search) refreshBulkBar(search);
    }
  });

  document.addEventListener("click", function (e) {
    var primaryBtn = e.target.closest("[data-set-primary-index]");
    if (primaryBtn) {
      e.preventDefault();
      setFanartPrimary(primaryBtn);
      return;
    }
    var deleteBtn = e.target.closest("#fanart-delete-selected");
    if (deleteBtn) {
      var dg = deleteBtn.closest("[data-sw-fanart-gallery]");
      if (dg) batchDelete(dg);
      return;
    }
    var moveBtn = e.target.closest(".fanart-move-btn");
    if (moveBtn) {
      var mg = moveBtn.closest("[data-sw-fanart-gallery]");
      if (mg) reorder(mg, moveBtn);
      return;
    }
    var bulkBtn = e.target.closest("#fanart-bulk-save");
    if (bulkBtn) {
      var bc = document.querySelector("[data-sw-fanart-search]");
      if (bc) bulkSave(bc);
      return;
    }
  });
})();
