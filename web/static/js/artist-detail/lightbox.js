// lightbox.js - parameterized full-size image lightbox for the artist-detail
// surfaces (stable Images tab + next/ Artwork section/modal).
//
// Extracted verbatim (M55 #1336, 4B) from the inline script in
// web/templates/artist_images_tab.templ so both UI channels share one copy. The
// lightbox overlay DOM (id="sw-lightbox", img id="sw-lightbox-img") still lives
// in templ; this module only drives open/close, focus restore, and the
// Escape + Tab focus-trap.
//
// Markup and the openImageLightbox templ script call openLightbox/closeLightbox
// by bare name from inline onclick handlers, so this IIFE exposes them on
// window (the F4a lesson: bare-name onclick callers need the global). No Go
// values are interpolated here; the element ids are constants.
(function () {
  "use strict";

  // _lightboxOpener holds the element that had focus when the lightbox was
  // opened; closeLightbox restores focus to it (same pattern as layout.templ
  // _modalOpener / hideModal).
  var _lightboxOpener = null;

  function openLightbox(src, alt) {
    var lb = document.getElementById("sw-lightbox");
    var img = document.getElementById("sw-lightbox-img");
    if (!lb || !img) {
      console.warn("[stillwater] openLightbox: lightbox DOM elements not found");
      return;
    }
    _lightboxOpener = document.activeElement;
    img.src = src;
    img.alt = alt || "";
    lb.classList.remove("hidden");
    lb.classList.add("flex");
    // Remove before adding to prevent duplicate listeners when the lightbox is
    // reopened without closing first.
    document.removeEventListener("keydown", _lightboxKeydown);
    document.addEventListener("keydown", _lightboxKeydown);
    // Move focus into the dialog so keyboard users land inside it.
    var closeBtn = lb.querySelector("[data-lightbox-close]");
    if (closeBtn) {
      closeBtn.focus();
    } else {
      lb.focus();
    }
  }

  function closeLightbox() {
    var lb = document.getElementById("sw-lightbox");
    if (!lb) return;
    lb.classList.add("hidden");
    lb.classList.remove("flex");
    var img = document.getElementById("sw-lightbox-img");
    if (img) img.src = "";
    document.removeEventListener("keydown", _lightboxKeydown);
    // Restore focus to the element that opened the lightbox.
    if (_lightboxOpener && typeof _lightboxOpener.focus === "function") {
      _lightboxOpener.focus();
    }
    _lightboxOpener = null;
  }

  function _lightboxKeydown(e) {
    if (e.key === "Escape") {
      e.preventDefault();
      closeLightbox();
      return;
    }
    // Trap Tab/Shift+Tab inside the dialog so focus cannot escape while the
    // modal is open (WCAG 2.1 SC 2.4.3).
    if (e.key !== "Tab") return;
    var lb = document.getElementById("sw-lightbox");
    if (!lb || lb.classList.contains("hidden")) return;
    var focusables = lb.querySelectorAll(
      'button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
    );
    if (focusables.length === 0) {
      e.preventDefault();
      lb.focus();
      return;
    }
    var first = focusables[0];
    var last = focusables[focusables.length - 1];
    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus();
    }
  }

  // Expose the two entry points; bare-name inline onclick handlers and the
  // openImageLightbox templ script call these globally.
  window.openLightbox = openLightbox;
  window.closeLightbox = closeLightbox;
})();
