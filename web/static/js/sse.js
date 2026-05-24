// sse.js -- Server-Sent Events client for real-time notifications.
//
// Connects to GET /api/v1/events/stream and listens for system events
// (scan completion, rule violations, bulk operations, etc.). Events are
// surfaced as toast notifications via the global showSuccessToast/showToast
// functions and update the notification badge count.
//
// Auto-reconnects with exponential backoff on disconnect. The connection
// is base-path aware for sub-path deployments.
//
// ES5 only -- no const/let, no arrow functions, no Set, no forEach on NodeList.

(function () {
  "use strict";

  var bpEl = document.querySelector('meta[name="htmx-base-path"]');
  var bp = bpEl ? bpEl.content : "";
  var streamURL = bp + "/api/v1/events/stream";

  var source = null;
  var retryDelay = 1000; // start at 1 second
  var maxRetryDelay = 30000; // cap at 30 seconds
  var retryTimer = null;

  // Event types that should trigger a toast notification.
  // Maps event type to toast function name.
  var toastEvents = {
    "scan.completed": "success",
    "bulk.completed": "success",
    "artist.new": "success",
    "metadata.fixed": "success",
    "rule.violation": "warning",
    "artist.updated": "success"
  };

  // Structured events do not surface a toast on their own -- they carry
  // data the page-level scripts (the layout-level ProgressPill, the
  // connection-push-failure handler below) consume via the dispatched
  // CustomEvent. EventSource silently discards frames whose `event:`
  // name has no addEventListener registration, so each new server-side
  // event type must appear here or its sse:<name> CustomEvent never
  // fires.
  var structuredEvents = ["operation.progress", "connection.push_failed"];

  function connect() {
    if (source) {
      try { source.close(); } catch (e) { /* ignore */ }
    }

    source = new EventSource(streamURL);

    source.addEventListener("connected", function (evt) {
      // Connection established -- reset retry delay.
      retryDelay = 1000;
    });

    // Listen for all mapped event types.
    var types = Object.keys(toastEvents);
    for (var i = 0; i < types.length; i++) {
      (function (eventType) {
        source.addEventListener(eventType, function (evt) {
          handleEvent(eventType, evt);
        });
      })(types[i]);
    }

    // Register structured events through a separate path that dispatches
    // the CustomEvent (so the pill JS sees it) without firing a generic
    // toast. connection.push_failed gets a dedicated toast below because
    // it is the user-visible surface from #1088; operation.progress is
    // rendered by the ProgressPill and must not also surface a toast.
    for (var j = 0; j < structuredEvents.length; j++) {
      (function (eventType) {
        source.addEventListener(eventType, function (evt) {
          handleStructuredEvent(eventType, evt);
        });
      })(structuredEvents[j]);
    }

    source.onerror = function () {
      // EventSource will auto-reconnect on its own for network errors,
      // but if the connection is fully closed (e.g. auth failure), we
      // need manual reconnection with backoff.
      if (source.readyState === EventSource.CLOSED) {
        scheduleReconnect();
      }
    };
  }

  function handleEvent(eventType, evt) {
    var data;
    try {
      data = JSON.parse(evt.data);
    } catch (e) {
      return;
    }

    // Show toast notification.
    var toastType = toastEvents[eventType];
    var message = data.message || data.title || "Event received";

    if (toastType === "success" && typeof window.showSuccessToast === "function") {
      window.showSuccessToast(message);
    } else if (toastType === "warning" && typeof window.showWarningToast === "function") {
      window.showWarningToast(message);
    } else if (typeof window.showToast === "function") {
      window.showToast(message);
    }

    // Refresh the notification badge count. The badge is loaded via HTMX
    // polling, but we can trigger an immediate refresh after an event.
    refreshNotificationBadge();

    // Dispatch a DOM event so page-specific scripts can react to SSE events
    // (e.g. the artist detail page refreshes its violations tab).
    // Fire on document.body with bubbles:true so listeners attached to
    // either body (via HTMX's `from:body`) or document (via plain
    // addEventListener) receive the event. The previous dispatch on
    // document only reached document-level listeners because CustomEvents
    // do not propagate downward, leaving body-targeted HTMX triggers
    // (including the conflict banner) silent on server push.
    document.body.dispatchEvent(new CustomEvent("sse:" + eventType, {detail: data, bubbles: true}));
  }

  // handleStructuredEvent dispatches the sse:<type> CustomEvent for events
  // that carry their own structured renderer (the ProgressPill, the
  // per-connection failure toast). It mirrors handleEvent's CustomEvent
  // shape so listeners do not have to special-case the path, but skips
  // the toastEvents lookup -- structured events render themselves.
  function handleStructuredEvent(eventType, evt) {
    var data;
    try {
      data = JSON.parse(evt.data);
    } catch (e) {
      return;
    }

    // connection.push_failed is the user-visible surface from #1088; the
    // backend has already returned success to the originating handler, so
    // an inline toast is the only signal the operator gets that a platform
    // write actually failed. The connection name + error class come from
    // the publish.busNotifier event data; an optional artist context lets
    // the message disambiguate when N platforms failed for the same item.
    if (eventType === "connection.push_failed" && typeof window.showToast === "function") {
      var conn = (data && data.connection) || "";
      var errClass = (data && data.error_class) || "push failed";
      var artist = (data && data.artist_name) || "";
      var message;
      if (conn && artist) {
        message = conn + ": " + errClass + " (artist: " + artist + ")";
      } else if (conn) {
        message = conn + ": " + errClass;
      } else {
        message = errClass;
      }
      // window.showToast is the error-level toast (red); see layout.templ
      // (enqueueToast('error', ...)) -- it is the right surface for a
      // failed platform write.
      window.showToast(message);
    }

    // Always dispatch the CustomEvent so the ProgressPill (and any
    // future structured-event consumer) sees the payload.
    document.body.dispatchEvent(new CustomEvent("sse:" + eventType, {detail: data, bubbles: true}));
  }

  function refreshNotificationBadge() {
    // Find the sidebar notification badge and trigger an immediate HTMX
    // refresh. The badge uses hx-get for polling; we call htmx.ajax
    // directly to bypass the polling interval after an SSE event.
    var badge = document.getElementById("sidebar-notif-badge");
    if (badge && typeof htmx !== "undefined") {
      var url = badge.getAttribute("hx-get");
      if (url) {
        htmx.ajax("GET", url, { target: badge });
      }
    }
  }

  function scheduleReconnect() {
    if (retryTimer) {
      clearTimeout(retryTimer);
    }
    retryTimer = setTimeout(function () {
      retryTimer = null;
      connect();
    }, retryDelay);
    // Exponential backoff with cap.
    retryDelay = Math.min(retryDelay * 2, maxRetryDelay);
  }

  // Disconnect cleanly when the page unloads.
  window.addEventListener("beforeunload", function () {
    if (retryTimer) {
      clearTimeout(retryTimer);
    }
    if (source) {
      source.close();
    }
  });

  // Expose for testing and external control.
  window.swSSE = {
    connect: connect,
    disconnect: function () {
      if (retryTimer) {
        clearTimeout(retryTimer);
        retryTimer = null;
      }
      if (source) {
        source.close();
        source = null;
      }
    },
    isConnected: function () {
      return source !== null && source.readyState === EventSource.OPEN;
    }
  };

  // Start the SSE connection.
  connect();
})();
