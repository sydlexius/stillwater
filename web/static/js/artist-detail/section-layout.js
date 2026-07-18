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

  // cachedArray reads a stored-preference array out of window.swPreferences'
  // sessionStorage cache (preferences.js). Returns [] when the cache, the key,
  // or swPreferences itself is unavailable, never throws.
  function cachedArray(key) {
    var cache = window.swPreferences && typeof window.swPreferences.getCache === "function"
      ? window.swPreferences.getCache()
      : null;
    var val = cache && cache[key];
    return Array.isArray(val) ? val : [];
  }

  // mergeAbsent appends any id from storedIDs that is not already in liveIDs,
  // preserving storedIDs' relative order, and returns the merged list. liveIDs
  // keeps its own order untouched.
  //
  // Without this, a save built from live-DOM ids only (#2108) silently drops
  // the order/collapsed state of any section not currently rendered on this
  // page load -- e.g. the debug section, which only renders when the
  // show_platform_debug preference is on. A save triggered while debug is
  // absent would overwrite artist_detail_section_order /
  // artist_detail_collapsed_sections with a live-DOM snapshot that has no
  // "debug" entry at all, permanently losing that section's preference.
  function mergeAbsent(liveIDs, storedIDs) {
    var seen = {};
    liveIDs.forEach(function (id) {
      seen[id] = true;
    });
    var merged = liveIDs.slice();
    storedIDs.forEach(function (id) {
      if (!seen[id]) {
        merged.push(id);
        seen[id] = true;
      }
    });
    return merged;
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
    // Merge in any stored id absent from the live DOM (#2108) so a save never
    // discards an unrendered section's preference.
    var order = mergeAbsent(currentOrder(), cachedArray(ORDER_KEY));
    var collapsed = mergeAbsent(currentCollapsed(), cachedArray(COLLAPSED_KEY));
    fetch(bp + "/api/v1/preferences", {
      method: "PATCH",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json", "X-CSRF-Token": csrfToken() },
      body: JSON.stringify(
        (function () {
          var body = {};
          body[ORDER_KEY] = order;
          body[COLLAPSED_KEY] = collapsed;
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

  // applyLayout live-applies a stored order + collapsed-set to the current
  // page's rendered sections, without a network round-trip or reload (#2110).
  // The on-page drag/collapse controls already update the live DOM directly
  // (Sortable.create moves nodes; toggleCollapse flips aria-expanded/hidden);
  // this is the same mechanism exposed so the preferences drawer's layout
  // controls (prefs-drawer.js) can apply a change to an already-open
  // artist-detail page instead of only persisting it for the next full load.
  //
  // Gated to when the section container is actually present: a no-op when the
  // drawer is opened on a page other than artist-detail.
  function applyLayout(order, collapsed) {
    var root = container();
    if (!root) return; // not on the artist-detail page

    order = Array.isArray(order) ? order : [];
    collapsed = Array.isArray(collapsed) ? collapsed : [];

    // Reorder: move each known section, in requested order, to the end of the
    // container in turn (appendChild on an existing child relocates it -- the
    // correct re-sort primitive for iterating a desired order in place, same
    // pattern prefs-drawer.js's resetLayout already uses on its own list).
    // Any live section whose id is absent from `order` (e.g. the drawer never
    // manages "debug") keeps its current relative position, appended after
    // the ordered ones.
    var byID = {};
    sections().forEach(function (s) {
      byID[s.getAttribute("data-sw-section")] = s;
    });
    var placed = {};
    order.forEach(function (id) {
      var s = byID[id];
      if (s) {
        root.appendChild(s);
        placed[id] = true;
      }
    });
    sections().forEach(function (s) {
      var id = s.getAttribute("data-sw-section");
      if (!placed[id]) {
        root.appendChild(s);
      }
    });

    // Collapse state: sync each live section's disclosure button + body to
    // match the requested collapsed set. Skips sections with no toggle button
    // (none expected, but avoids a null-deref if the markup ever varies).
    sections().forEach(function (s) {
      var id = s.getAttribute("data-sw-section");
      var btn = s.querySelector("[data-sw-section-toggle]");
      if (!btn) return;
      var shouldCollapse = collapsed.indexOf(id) !== -1;
      var isCollapsed = btn.getAttribute("aria-expanded") === "false";
      if (shouldCollapse === isCollapsed) return; // already in sync
      btn.setAttribute("aria-expanded", String(!shouldCollapse));
      var bodyID = btn.getAttribute("aria-controls");
      var body = bodyID ? document.getElementById(bodyID) : null;
      if (body) {
        if (shouldCollapse) {
          body.setAttribute("hidden", "");
        } else {
          body.removeAttribute("hidden");
        }
      } else if (window.console) {
        console.error("swArtistSectionLayout: applyLayout toggle target not found for aria-controls=" + bodyID);
      }
    });
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
      var root = container();
      if (!root || !root.contains(btn)) return; // only our sections
      ev.preventDefault();
      toggleCollapse(btn);
    });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }

  window.swArtistSectionLayout = { init: init, applyLayout: applyLayout, __initialized: true };
})();
