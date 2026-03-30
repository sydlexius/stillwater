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
  }

  function refreshNotificationBadge() {
    // Find the notification badge element and trigger an HTMX refresh
    // if it exists. The badge uses hx-get for polling; we trigger a
    // manual load to get an immediate update.
    var badge = document.getElementById("notification-badge");
    if (badge && typeof htmx !== "undefined") {
      htmx.trigger(badge, "sse-refresh");
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
