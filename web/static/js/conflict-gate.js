// conflict-gate.js -- syncs the body data attributes that drive the
// "visibly disabled" style on write buttons throughout the app.
//
// The banner component (conflict_banner.templ) renders a hidden span
// <span id="sw-conflict-state" data-image-gated="..." data-nfo-gated="...">
// on every swap. We mirror those flags onto <body data-sw-image-gated>
// and <body data-sw-nfo-gated> so CSS selectors in styles.css can target
// any element decorated with `data-sw-requires-image-write` or
// `data-sw-requires-nfo-write`. The CSS approach keeps the grey-out
// logic declarative; pages just tag their buttons once.
(function () {
  "use strict";

  // Refresh the conflict banner and every visible per-connection
  // detected-* panel. We route through htmx.ajax instead of hx-trigger
  // because HTMX special-cases the "sse:" event-name prefix and silently
  // drops `hx-trigger="sse:conflict.changed"` unless the htmx-sse extension
  // is loaded -- which Stillwater does not bundle, so listener-based SSE
  // updates never fired. Using htmx.ajax here works regardless of which
  // HTMX extensions are present and lets the button-emitted CustomEvent
  // and the sse.js server-push path share a single trigger surface.
  function refreshConflictSurfaces() {
    if (typeof htmx === "undefined") {
      return;
    }
    var banner = document.getElementById("conflict-banner");
    if (banner) {
      htmx.ajax("GET", "/api/v1/config/conflict-banner", {
        target: "#conflict-banner",
        swap: "innerHTML",
      });
    }
    // Re-fetch every settings "Detected on this server" panel currently in
    // the DOM. These carry their own hx-get so we can just trigger a
    // refresh rather than know their URLs.
    var panels = document.querySelectorAll('[id^="detected-"]');
    for (var i = 0; i < panels.length; i++) {
      var el = panels[i];
      var url = el.getAttribute("hx-get");
      if (!url) continue;
      htmx.ajax("GET", url, { target: "#" + el.id, swap: "innerHTML" });
    }
  }

  // Listen on body for the SSE-sourced event (sse.js dispatches these on
  // body with bubbles: true). We also hook HTMX's own `htmx:afterRequest`
  // to catch successful POSTs against the stillwater-managed endpoint --
  // that way the button only needs hx-post/hx-swap=none, no hx-on glue,
  // and we do not depend on HTMX's attribute-string JS parsing which was
  // observed to silently drop hx-on expressions under some DOM swap
  // orderings.
  function attachListeners() {
    if (!document.body) return;
    document.body.addEventListener("sse:conflict.changed", refreshConflictSurfaces);
    document.body.addEventListener("htmx:afterRequest", function (evt) {
      var detail = evt && evt.detail;
      if (!detail || !detail.successful) return;
      var url =
        (detail.requestConfig && detail.requestConfig.path) ||
        (detail.pathInfo && detail.pathInfo.requestPath) ||
        (detail.xhr && detail.xhr.responseURL) ||
        "";
      if (typeof url !== "string") return;
      if (url.indexOf("/stillwater-managed") !== -1) {
        // Refresh banner + detected panels immediately. On the settings
        // page the iOS toggle and card copy are rendered server-side from
        // the DB row we just mutated, and there is no per-toggle render
        // endpoint; do a soft reload so the toggle button reflects the
        // new state consistently with the banner. Other pages only show
        // the banner and detected panels, which refreshConflictSurfaces
        // already handles.
        var path = window.location.pathname || "";
        if (path.indexOf("/settings") !== -1) {
          window.location.reload();
          return;
        }
        refreshConflictSurfaces();
      }
    });
  }
  if (document.body) {
    attachListeners();
  } else {
    document.addEventListener("DOMContentLoaded", attachListeners);
  }

  function syncFromBanner() {
    var marker = document.getElementById("sw-conflict-state");
    var body = document.body;
    if (!body) {
      return;
    }
    if (!marker) {
      body.removeAttribute("data-sw-image-gated");
      body.removeAttribute("data-sw-nfo-gated");
      return;
    }
    if (marker.getAttribute("data-image-gated") === "true") {
      body.setAttribute("data-sw-image-gated", "true");
    } else {
      body.removeAttribute("data-sw-image-gated");
    }
    if (marker.getAttribute("data-nfo-gated") === "true") {
      body.setAttribute("data-sw-nfo-gated", "true");
    } else {
      body.removeAttribute("data-sw-nfo-gated");
    }
  }

  // Exposed so the banner's hx-on::after-swap hook can invoke it without
  // relying on DOMContentLoaded timing.
  window.swSyncConflictGateFromBanner = syncFromBanner;

  // swStillwaterManagedAfterRequest runs after the "Let Stillwater manage"
  // toggle POSTs (hx-swap="none"), syncing the toggle button's frozen
  // aria-checked / Tailwind class / hx-vals state from the server's
  // authoritative feature_manage_server_files response, then refreshing the
  // per-connection conflict-detail fragment and firing sse:conflict.changed
  // for the banner. On failure it writes a localized inline error.
  //
  // It lives in conflict-gate.js (loaded in the layout on every page) rather
  // than a settings-only module because the toggle is triggered from BOTH the
  // settings card AND the global ConflictBanner CTAs -- the handler must exist
  // wherever either renders. The triggering element is passed as `triggerEl`
  // and the HTMX event as `event` (bound via
  // hx-on:htmx:after-request="swStillwaterManagedAfterRequest(this, event)"),
  // so the event is threaded explicitly instead of read from the deprecated
  // global window.event. The connID is read from the trigger's data-conn-id;
  // the settings switch (#stillwater-managed-<connID>) is then looked up to
  // sync its state and may be absent when the CTA fires from a banner on
  // another page, which the null guards tolerate.
  //
  // Button class strings come from the trigger/toggle's data-sw-{btn,knob}-{on,off}
  // attributes (populated by the ruleToggle*Classes helpers at render time) so
  // this never hardcodes Tailwind utility names.
  window.swStillwaterManagedAfterRequest = function (triggerEl, event) {
    var connID = triggerEl && triggerEl.dataset ? triggerEl.dataset.connId : "";
    var btn = document.getElementById("stillwater-managed-" + connID);
    if (event.detail.successful) {
      try {
        var resp = JSON.parse(event.detail.xhr.responseText || "{}");
        if (btn && typeof resp.feature_manage_server_files === "boolean") {
          var enabled = resp.feature_manage_server_files;
          var knob = btn.querySelector("span");
          btn.setAttribute("aria-checked", String(enabled));
          btn.setAttribute("class", enabled ? btn.dataset.swBtnOn : btn.dataset.swBtnOff);
          btn.setAttribute("hx-vals", JSON.stringify({ enabled: !enabled }));
          if (knob) {
            knob.setAttribute("class", enabled ? btn.dataset.swKnobOn : btn.dataset.swKnobOff);
          }
        }
      } catch (_) {
        // Non-JSON response; leave button state and rely on a subsequent page
        // load to re-sync. The fetch below still refreshes the detail fragment
        // so the user sees the new server-side state.
      }
      if (typeof htmx !== "undefined") {
        htmx.ajax("GET", "/api/v1/connections/" + connID + "/conflict-detail",
          { target: "#detected-" + connID, swap: "innerHTML" });
      }
      document.body.dispatchEvent(new CustomEvent("sse:conflict.changed"));
    } else {
      var el = document.getElementById("detected-" + connID);
      if (el) {
        // Prefer the render-time translated copy pinned on the toggle's
        // data-sw-error attribute. The English literal is a last-resort
        // fallback for the pathological case where the toggle was removed
        // from the DOM before the request settled.
        el.textContent = (btn && btn.dataset.swError) || "Could not update this server-managed setting. Try again or reload the page.";
      }
    }
  };

  // Run once on page load in case the banner lands before our listener
  // wires up (HTMX's hx-on fires after the node is in the DOM).
  document.addEventListener("DOMContentLoaded", syncFromBanner);
  document.addEventListener("htmx:afterSwap", function (evt) {
    if (evt && evt.detail && evt.detail.target && evt.detail.target.id === "conflict-banner") {
      syncFromBanner();
    }
  });
})();
