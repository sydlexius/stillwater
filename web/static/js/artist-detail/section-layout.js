// section-layout.js - collapsible + drag-reorderable sections on the next/
// artist-detail page (M55 #2065).
//
// Two interactions, both persisted per-user via PATCH /api/v1/preferences:
//   - Drag reorder: SortableJS mounts on [data-sw-sortable-section] and drags
//     its direct-child <section data-sw-section> cards by the left
//     .sw-section-drag-handle grip (the hero/sticky header stay outside the
//     container, so they are never reorderable). onEnd persists the new order
//     under artist_detail_section_order.
//   - Collapse toggle: the disclosure button in each section head
//     ([data-sw-section-toggle]) hides/shows the section body and persists the
//     collapsed set under artist_detail_collapsed_sections.
//
// Both the order and collapsed keys are sent on every save so the two stay
// consistent regardless of which interaction triggered it. State is read from
// the live DOM (section order + each toggle's aria-expanded), never threaded
// through JS variables, so it survives htmx fragment swaps of section bodies.
//
// CSRF + base path are read the same way as the other artist-detail modules
// (window.swCsrfToken + the htmx-base-path meta), NOT window.swPreferences
// (which is the preference get/set API, not a CSRF source).
(function () {
  "use strict";

  // Idempotency guard: skip re-initialization if already loaded.
  if (window.swArtistSectionLayout && window.swArtistSectionLayout.__initialized) return;

  var ORDER_KEY = "artist_detail_section_order";
  var COLLAPSED_KEY = "artist_detail_collapsed_sections";

  function csrfToken() {
    if (typeof window.swCsrfToken !== "function") {
      console.error("swCsrfToken unavailable - preferences.js may have failed to load; preference saves will 403");
      return "";
    }
    return window.swCsrfToken();
  }

  function basePath() {
    var meta = document.querySelector('meta[name="htmx-base-path"]');
    return meta ? meta.content : "";
  }

  function container() {
    return document.querySelector("[data-sw-sortable-section]");
  }

  // sections returns the live ordered list of section elements (direct children
  // carrying data-sw-section).
  function sections() {
    var root = container();
    if (!root) return [];
    return Array.prototype.slice.call(root.querySelectorAll(":scope > [data-sw-section]"));
  }

  // currentOrder is the section ids in DOM order.
  function currentOrder() {
    return sections().map(function (s) {
      return s.getAttribute("data-sw-section");
    });
  }

  // currentCollapsed is the ids of sections whose disclosure button is collapsed
  // (aria-expanded="false").
  function currentCollapsed() {
    return sections()
      .filter(function (s) {
        var btn = s.querySelector("[data-sw-section-toggle]");
        return btn && btn.getAttribute("aria-expanded") === "false";
      })
      .map(function (s) {
        return s.getAttribute("data-sw-section");
      });
  }

  function save() {
    var bp = basePath();
    fetch(bp + "/api/v1/preferences", {
      method: "PATCH",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json", "X-CSRF-Token": csrfToken() },
      body: JSON.stringify(
        (function () {
          var body = {};
          body[ORDER_KEY] = currentOrder();
          body[COLLAPSED_KEY] = currentCollapsed();
          return body;
        })()
      ),
    })
      .then(function (r) {
        if (!r.ok && window.console) {
          console.warn("swArtistSectionLayout: failed to save section layout (HTTP " + r.status + ")");
        }
      })
      .catch(function (e) {
        if (window.console) {
          console.warn("swArtistSectionLayout: network error saving section layout", e);
        }
      });
  }

  // toggleCollapse flips a single section's collapsed state: show/hide its body
  // (via aria-controls) and update the disclosure button's aria-expanded. The
  // chevron rotation is driven by CSS off aria-expanded, so no class flip here.
  function toggleCollapse(btn) {
    var bodyID = btn.getAttribute("aria-controls");
    var body = bodyID ? document.getElementById(bodyID) : null;
    var expanded = btn.getAttribute("aria-expanded") !== "false";
    var nextExpanded = !expanded;
    btn.setAttribute("aria-expanded", String(nextExpanded));
    if (body) {
      if (nextExpanded) {
        body.removeAttribute("hidden");
      } else {
        body.setAttribute("hidden", "");
      }
    } else if (window.console) {
      // Loud failure: the toggle is wired but its body target is missing, which
      // means the aria-controls id drifted from the body div id.
      console.error("swArtistSectionLayout: toggle target not found for aria-controls=" + bodyID);
    }
    save();
  }

  function initSortable() {
    var root = container();
    if (!root) return;
    if (typeof Sortable === "undefined") {
      // Loud failure: Sortable is a hard dependency loaded before this module on
      // next/ pages. If it is missing, reorder silently would not work.
      if (window.console) {
        console.error("swArtistSectionLayout: SortableJS not loaded; section reorder disabled");
      }
      return;
    }
    Sortable.create(root, {
      handle: ".sw-section-drag-handle",
      animation: 150,
      ghostClass: "sw-section-ghost",
      chosenClass: "sw-section-chosen",
      onEnd: save,
    });
  }

  function init() {
    if (!container()) return; // not on the artist-detail page

    initSortable();

    // Delegated collapse-toggle handler: survives htmx swaps of section bodies.
    document.addEventListener("click", function (ev) {
      var btn = ev.target.closest ? ev.target.closest("[data-sw-section-toggle]") : null;
      if (!btn) return;
      if (!container().contains(btn)) return; // only our sections
      ev.preventDefault();
      toggleCollapse(btn);
    });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }

  window.swArtistSectionLayout = { init: init, __initialized: true };
})();
