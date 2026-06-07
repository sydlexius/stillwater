// artwork-modal.js - orchestration for the next/ in-page "Manage artwork" modal
// (M55 #1336, 4B). The heavy image-editing behavior (crop, sort, drag-drop,
// upload, fetch-URL, compare) lives in the reused ArtworkManageEditor fragment
// that this modal lazy-loads per kind; this module only drives the modal shell:
// open/close + focus management, the kind switcher, lazy body loading, the
// Revert-to-original affordance (consumes #1837's revert endpoint + the
// /info backup_exists flag), and the in-modal conflict-gate banner for
// structured 409 responses.
//
// All wiring is document-level delegation so it survives htmx fragment swaps.
(function () {
  "use strict";

  var KIND_TO_TYPE = {
    primary: "thumb",
    logo: "logo",
    banner: "banner",
    backdrops: "fanart",
  };

  var _opener = null; // element focused before the modal opened (focus restore)
  var _activeKind = "primary";

  function modal() {
    return document.getElementById("artwork-modal");
  }

  function basePath() {
    var meta = document.querySelector('meta[name="htmx-base-path"]');
    return meta ? meta.content : "";
  }

  // Delegates to the canonical reader (preferences.js) instead of re-inventing
  // the cookie regex, matching the other first-party modules.
  function csrfToken() {
    return typeof window.swCsrfToken === "function" ? window.swCsrfToken() : "";
  }

  function artistID() {
    var m = modal();
    return m ? m.dataset.artistId : "";
  }

  function hideGateBanner() {
    var b = document.getElementById("artwork-gate-banner");
    if (b) b.classList.add("hidden");
  }

  // showGateBanner renders the structured 409 cause (conflict gate) inline rather
  // than letting the write fail silently or reloading the page.
  function showGateBanner(reason) {
    var b = document.getElementById("artwork-gate-banner");
    var r = document.getElementById("artwork-gate-reason");
    if (!b) return;
    if (r) r.textContent = reason || "";
    b.classList.remove("hidden");
  }

  // loadBody lazily fetches the reused editor fragment for the active kind.
  // A scoped htmx:responseError listener (below) renders an inline error state
  // on non-2xx so a 404 (deleted artist), 500, or session-expiry redirect does
  // not leave the modal body silently empty.
  function loadBody() {
    var id = artistID();
    if (!id || typeof window.htmx === "undefined") return;
    var url =
      "/next/artists/" +
      id +
      "/artwork-modal?kind=" +
      encodeURIComponent(_activeKind);
    window.htmx.ajax("GET", url, { target: "#artwork-modal-body", swap: "innerHTML" });
  }

  // refreshRevert toggles the Revert affordance based on whether a one-deep
  // backup exists for the active kind. Fanart (multi-slot) never reports a
  // single-slot backup, so Revert stays hidden there.
  //
  // A request failure (401/403/500/network) is NOT treated as "no backup": we
  // log a warning and leave the prior Revert state so the affordance survives
  // a transient error. Only a clean 200 + backup_exists=false legitimately
  // hides Revert. This mirrors doRevert's own guard (see its comment below).
  function refreshRevert() {
    var row = document.getElementById("artwork-revert-row");
    if (!row) return;
    var type = KIND_TO_TYPE[_activeKind] || "thumb";
    if (type === "fanart") {
      row.classList.add("hidden");
      return;
    }
    var id = artistID();
    if (!id) return;
    fetch(
      basePath() + "/api/v1/artists/" + id + "/images/" + type + "/info",
      { credentials: "same-origin", headers: { "HX-Request": "false" } },
    )
      .then(function (r) {
        if (!r.ok) {
          // Request failed: log and leave the prior Revert state for retry.
          console.warn("artwork revert info failed: HTTP " + r.status);
          return undefined; // signals the next handler to skip state change
        }
        return r.json();
      })
      .then(function (info) {
        if (info === undefined) return; // request failed: leave prior state
        if (info && info.backup_exists) {
          row.classList.remove("hidden");
        } else {
          row.classList.add("hidden");
        }
      })
      .catch(function (err) {
        // Network error: log but leave prior Revert state so the user can retry.
        console.warn("artwork revert info network error: " + (err && err.message));
      });
  }

  function setActiveKind(kind) {
    if (!KIND_TO_TYPE[kind]) kind = "primary";
    _activeKind = kind;
    var tabs = document.querySelectorAll("[data-sw-artwork-kind-tab]");
    for (var i = 0; i < tabs.length; i++) {
      var on = tabs[i].dataset.artworkKind === kind;
      tabs[i].setAttribute("aria-pressed", on ? "true" : "false");
    }
    hideGateBanner();
    loadBody();
    refreshRevert();
  }

  function openModal(kind) {
    var m = modal();
    if (!m) return;
    _opener = document.activeElement;
    m.classList.remove("hidden");
    m.classList.add("flex");
    document.removeEventListener("keydown", onKeydown);
    document.addEventListener("keydown", onKeydown);
    setActiveKind(kind || "primary");
    m.focus();
  }

  function closeModal() {
    var m = modal();
    if (!m) return;
    m.classList.add("hidden");
    m.classList.remove("flex");
    document.removeEventListener("keydown", onKeydown);
    if (_opener && typeof _opener.focus === "function") _opener.focus();
    _opener = null;
  }

  function onKeydown(e) {
    if (e.key === "Escape") {
      e.preventDefault();
      closeModal();
      return;
    }
    // Trap Tab/Shift+Tab inside the dialog so keyboard focus cannot escape to
    // the page behind it while the modal is open (WCAG 2.1 SC 2.4.3).
    if (e.key !== "Tab") return;
    var m = modal();
    if (!m || m.classList.contains("hidden")) return;
    var focusables = m.querySelectorAll(
      'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
    );
    // Only consider currently-visible controls (the lazy body and hidden
    // sub-modals contribute none until shown).
    var visible = [];
    for (var i = 0; i < focusables.length; i++) {
      var el = focusables[i];
      if (el.offsetParent !== null || el === document.activeElement) visible.push(el);
    }
    if (visible.length === 0) {
      e.preventDefault();
      m.focus();
      return;
    }
    var first = visible[0];
    var last = visible[visible.length - 1];
    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus();
    }
  }

  // doRevert POSTs the #1837 revert endpoint for the active single-slot kind.
  // 200 -> reload the editor body and re-check Revert; 409 -> show the gate
  // banner (do not reload); 404 -> the backup vanished, just hide Revert.
  function doRevert() {
    var id = artistID();
    var type = KIND_TO_TYPE[_activeKind] || "thumb";
    if (!id || type === "fanart") return;
    // Guard an empty CSRF token (missing/expired cookie): sending it would draw
    // a server 403 that the catch-all below would treat as "backup gone" and
    // silently hide Revert. Bail without sending so the affordance survives a
    // reload-and-retry. Mirrors the guard in fanart-manage.js.
    var token = csrfToken();
    if (!token) {
      console.warn("artwork revert skipped: empty CSRF token (reload the page)");
      return;
    }
    fetch(basePath() + "/api/v1/artists/" + id + "/images/" + type + "/revert", {
      method: "POST",
      headers: { "X-CSRF-Token": token },
      credentials: "same-origin",
    })
      .then(function (r) {
        if (r.ok) {
          hideGateBanner();
          loadBody();
          refreshRevert();
          return;
        }
        if (r.status === 409) {
          return r
            .json()
            .then(function (b) {
              showGateBanner(b && b.reason);
            })
            .catch(function () {
              showGateBanner("");
            });
        }
        if (r.status === 404) {
          // The backup genuinely vanished: hide Revert and move on.
          var row = document.getElementById("artwork-revert-row");
          if (row) row.classList.add("hidden");
          return;
        }
        // Any other status is a transient/unexpected failure: log it (so it is
        // not silent) but keep the affordance so the user can retry.
        console.warn("artwork revert failed: HTTP " + r.status);
      })
      .catch(function (err) {
        // Network error: keep the affordance for retry; log rather than swallow.
        console.warn("artwork revert network error: " + (err && err.message));
      });
  }

  // ---- Delegation ------------------------------------------------------------
  document.addEventListener("click", function (e) {
    var openBtn = e.target.closest("[data-sw-artwork-open]");
    if (openBtn) {
      e.preventDefault();
      openModal(openBtn.dataset.artworkKind || "primary");
      return;
    }
    if (e.target.closest("[data-sw-artwork-close]")) {
      e.preventDefault();
      closeModal();
      return;
    }
    var tab = e.target.closest("[data-sw-artwork-kind-tab]");
    if (tab) {
      e.preventDefault();
      setActiveKind(tab.dataset.artworkKind);
      return;
    }
    if (e.target.closest("#artwork-revert-btn")) {
      e.preventDefault();
      doRevert();
      return;
    }
    // Click on the dark backdrop (the modal element itself, not its surface)
    // closes the modal.
    if (e.target === modal()) {
      closeModal();
    }
  });

  // Structured-409 surfacing: any write inside the modal that htmx fires and the
  // gate blocks returns 409 with {reason}. Render the cause in the banner rather
  // than letting htmx swallow it silently. Scoped to the modal subtree.
  document.addEventListener("htmx:responseError", function (e) {
    var xhr = e.detail && e.detail.xhr;
    if (!xhr || xhr.status !== 409) return;
    var m = modal();
    var tgt = e.detail.target || e.target;
    if (!m || m.classList.contains("hidden") || !m.contains(tgt)) return;
    var reason = "";
    try {
      var body = JSON.parse(xhr.responseText);
      reason = body.reason || "";
    } catch (_) {
      /* non-JSON 409: show the generic paused copy */
    }
    showGateBanner(reason);
  });

  // Body-load error surfacing (H2): if the lazy GET for the editor fragment
  // fails (404 = artist deleted in another tab, 500, session-expiry redirect),
  // htmx skips the swap and the body stays empty/stale with no feedback.
  // Render an inline error state instead. 409 is excluded: the gate-banner
  // listener above already handles that case for writes; the body GET never
  // returns 409.
  document.addEventListener("htmx:responseError", function (e) {
    var xhr = e.detail && e.detail.xhr;
    if (!xhr || xhr.status === 409) return;
    var b = document.getElementById("artwork-modal-body");
    if (!b) return;
    var tgt = e.detail.target || e.target;
    // Only react when the error target IS the body element itself (the lazy
    // body-load GET). Errors from descendant targets (image search, logo-trim,
    // image DELETE, image fetch) must NOT overwrite the whole editor body and
    // destroy the user's in-progress editing session.
    if (tgt !== b) return;
    b.innerHTML =
      '<p class="text-sm text-red-500 py-6 text-center" role="alert">' +
      "Failed to load the editor (HTTP " +
      xhr.status +
      "). Reload the page and try again." +
      "</p>";
  });
})();
