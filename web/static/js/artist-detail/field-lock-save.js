// field-lock-save.js - save-then-lock guard for the next/ artist-detail edit
// surface (M55 #1336, 4B).
//
// The per-field lock toggle (FieldLockToggle in web/templates/artist_field.templ)
// fires its POST/DELETE to /field-locks and then reloads the page. In next/ edit
// mode the lock toggle lives in the value cell next to an unsaved <input>/<select>/
// <textarea name="value">, so a user who types a value and THEN clicks the lock
// would lose the typed text to the reload (the input was never saved).
//
// This module closes that gap: when the toggle is clicked while its field has a
// DIRTY edit input, it first PATCHes the typed value (the same endpoint the edit
// form posts to), and only on success re-dispatches the click so htmx performs
// the lock + reload against the now-persisted value. A failed save aborts the
// lock so the page never reloads over unsaved input.
//
// Scoped to .sw-next-artist-detail: only that channel reveals the edit-mode lock
// toggle, and a clean input or a display-mode toggle (no [name="value"]) is left
// entirely to htmx (early return), so this is a no-op everywhere else.
(function () {
  "use strict";

  // csrfToken delegates to the canonical reader (preferences.js) rather than
  // re-inventing the cookie regex, mirroring the other first-party modules.
  function csrfToken() {
    return typeof window.swCsrfToken === "function" ? window.swCsrfToken() : "";
  }

  function basePath() {
    var meta = document.querySelector('meta[name="htmx-base-path"]');
    return meta ? meta.content : "";
  }

  // isDirty reports whether an edit control holds a value the user changed from
  // the server-rendered default (so we only save when there is something to lose).
  function isDirty(input) {
    if (input.tagName === "SELECT") {
      for (var i = 0; i < input.options.length; i++) {
        if (input.options[i].selected !== input.options[i].defaultSelected) {
          return true;
        }
      }
      return false;
    }
    return input.value !== input.defaultValue;
  }

  // saveThenLock PATCHes the field's typed value, then re-clicks the toggle so
  // htmx applies the lock against the persisted value. On failure it alerts and
  // does NOT proceed, so the unsaved input survives (no reload).
  function saveThenLock(form, input, toggle) {
    var url = basePath() + form.getAttribute("hx-patch");
    var token = csrfToken();
    if (!token) {
      alert(
        toggle.getAttribute("data-error") ||
          "Session expired. Please reload the page and try again.",
      );
      return;
    }
    var body = new URLSearchParams();
    body.set("value", input.value);
    fetch(url, {
      method: "PATCH",
      headers: {
        "Content-Type": "application/x-www-form-urlencoded",
        "X-CSRF-Token": token,
      },
      body: body.toString(),
      credentials: "same-origin",
    })
      .then(function (r) {
        if (r.ok) {
          // Let the original htmx lock flow run now that the value is saved.
          toggle.dataset.swLockProceed = "1";
          toggle.click();
        } else {
          alert(
            toggle.getAttribute("data-error") ||
              "Saving the field before locking failed; your change was not saved.",
          );
        }
      })
      .catch(function () {
        alert(
          toggle.getAttribute("data-error") ||
            "Network error saving the field before locking; your change was not saved.",
        );
      });
  }

  // Capture-phase so this runs BEFORE htmx's own click handler on the toggle:
  // stopPropagation() then keeps the event from reaching htmx until the value is
  // saved (we re-dispatch the click afterwards via the swLockProceed flag).
  document.addEventListener(
    "click",
    function (e) {
      var toggle = e.target.closest(".field-lock-toggle");
      if (!toggle) return;
      // Second pass after a successful save: clear the flag and let htmx run.
      if (toggle.dataset.swLockProceed === "1") {
        delete toggle.dataset.swLockProceed;
        return;
      }
      // Only the next/ channel reveals the edit-mode toggle; leave the stable
      // channel (and any future host) untouched.
      if (!toggle.closest(".sw-next-artist-detail")) return;
      var container = toggle.closest('[id^="field-"]');
      if (!container) return;
      var form = container.querySelector("form[hx-patch]");
      if (!form) return; // display mode: no edit form, nothing to lose.
      var input = form.querySelector('[name="value"]');
      if (!input || !isDirty(input)) return; // clean input: htmx lock is safe.
      e.preventDefault();
      e.stopPropagation();
      saveThenLock(form, input, toggle);
    },
    true,
  );
})();
