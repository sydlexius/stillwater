// Artist detail: ambient fanart slideshow backdrop (M55 #1336, 4A).
// Extracted verbatim from the inline <script> that lived in
// web/templates/artist_detail.templ. Cross-fades the fixed-position fanart
// backdrop (two stacked <img> layers) when the artist has more than one fanart.
//
// DOM contract:
//   #fanart-slideshow  -- mount; carries data-fanart-count and
//     data-fanart-path-template (a base-path-relative URL with a {index}
//     placeholder, so the path is never hardcoded in JS).
//   #fanart-a / #fanart-b -- the two cross-fading image layers.
// Network: GET {base}{path-template with {index} filled} (base via
// meta[name="htmx-base-path"]). Pauses on document.hidden, resumes on visible.
//
// Export surface: window.swArtistFanartSlideshow doubles as the load-once guard.
(function () {
  'use strict';
  if (window.swArtistFanartSlideshow) return;

  function init() {
    var ss = document.getElementById('fanart-slideshow');
    if (!ss) return;
    var count = parseInt(ss.dataset.fanartCount, 10);
    // DOM-provided, base-path-relative path template (never hardcode /api/v1/...
    // in JS): "/api/v1/artists/<id>/images/fanart/{index}/file". We prepend only
    // the htmx-base-path and substitute the frame index here.
    var pathTemplate = ss.dataset.fanartPathTemplate;
    if (!(count >= 2) || !pathTemplate) return;
    var imgA = document.getElementById('fanart-a');
    var imgB = document.getElementById('fanart-b');
    if (!imgA || !imgB) return;
    var current = 0;
    var showingA = true;
    var intervalMs = 8000;
    var timer = null;
    var bpEl = document.querySelector('meta[name="htmx-base-path"]');
    var bp = bpEl ? bpEl.content : '';

    function cycle() {
      current = (current + 1) % count;
      var url = bp + pathTemplate.replace('{index}', String(current));
      if (showingA) {
        imgB.onload = function () { imgA.style.opacity = '0'; imgB.style.opacity = '1'; showingA = false; };
        imgB.onerror = function () { /* skip failed image on next cycle */ };
        imgB.src = url;
      } else {
        imgA.onload = function () { imgB.style.opacity = '0'; imgA.style.opacity = '1'; showingA = true; };
        imgA.onerror = function () { /* skip failed image on next cycle */ };
        imgA.src = url;
      }
    }

    function start() { if (!timer) timer = setInterval(cycle, intervalMs); }
    function stop() { if (timer) { clearInterval(timer); timer = null; } }

    document.addEventListener('visibilitychange', function () {
      if (document.hidden) stop(); else start();
    });
    start();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }

  window.swArtistFanartSlideshow = { init: init };
})();
