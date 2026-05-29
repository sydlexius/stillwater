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

  // maxSeenId is the highest SSE event id we have already surfaced. On
  // reconnect the server replays buffered events (Last-Event-ID), some of
  // which we already toasted; event ids are monotonic, so we only toast an
  // id greater than this mark. Events missed while offline still carry ids
  // above the mark, so their toasts fire exactly once.
  var maxSeenId = 0;

  // wasDisconnected gates the reconnect-rehydrate path so the initial
  // page-load `connected` event does NOT fire status fetches (the page
  // already rendered with the latest in-flight state from server-side
  // templates). scheduleReconnect sets it; the next connected handler
  // consumes and clears it. Per the ProgressPill stale/reconnect design
  // (#1641), only true reconnects trigger /status rehydrate calls so we
  // do not double-load on every page navigation.
  var wasDisconnected = false;

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
  var structuredEvents = [
    "operation.progress",
    "connection.push_failed",
    // M55 next-channel events. Cross-tab / dashboard signals that render via
    // their own consumers (no generic toast). Each must be listed here or
    // EventSource silently drops the frame and its sse:<name> CustomEvent
    // never fires.
    "settings.changed",
    "activity.recent",
    "dashboard.action-resolved"
  ];

  function connect() {
    if (source) {
      try { source.close(); } catch (e) { /* ignore */ }
    }

    source = new EventSource(streamURL);

    source.addEventListener("connected", function (evt) {
      // Connection established -- reset retry delay.
      retryDelay = 1000;
      // Reconnect rehydrate (#1641): the EventSource just came back from
      // a disconnect, so the ProgressPill UI may be showing stale state
      // for any in-flight long-running op that ticked while we were
      // offline. Query the status endpoints and replay the snapshots
      // through window.swProgressPill so the pill auto-recovers without
      // a manual page reload.
      if (wasDisconnected) {
        wasDisconnected = false;
        rehydrateInflightOps();
      }
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
      // Any non-OPEN state means the SSE stream is degraded -- either fully
      // CLOSED (e.g. auth failure) or in CONNECTING while the browser's
      // native EventSource attempts auto-reconnect after a transient drop.
      // Flag wasDisconnected for both so the next `connected` event fires
      // rehydrateInflightOps; without this, native auto-reconnects would
      // silently skip the rehydrate and the pill could stay stale-display
      // after a real reconnect (#1641 CR follow-up).
      if (source.readyState !== EventSource.OPEN) {
        wasDisconnected = true;
      }
      // Only schedule manual reconnect when CLOSED -- CONNECTING means the
      // browser is already retrying on its own, so a parallel
      // scheduleReconnect would race and produce double connections.
      if (source.readyState === EventSource.CLOSED) {
        scheduleReconnect();
      }
    };
  }

  // noteSeenID advances the toast high-water mark from an SSE frame's id and
  // reports whether the frame is a replay duplicate (id at or below the mark).
  // Frames without a numeric id (the connected handshake) never dedupe.
  function noteSeenID(evt) {
    var evtId = evt && evt.lastEventId ? parseInt(evt.lastEventId, 10) : 0;
    if (!evtId || isNaN(evtId)) {
      return false;
    }
    if (evtId <= maxSeenId) {
      return true;
    }
    maxSeenId = evtId;
    return false;
  }

  function handleEvent(eventType, evt) {
    var data;
    try {
      data = JSON.parse(evt.data);
    } catch (e) {
      return;
    }

    // Suppress toasts for events already surfaced before a reconnect: replayed
    // frames carry ids at or below the high-water mark. We still advance the
    // mark and refresh derived UI below so missed-while-offline events (ids
    // above the mark) toast exactly once.
    var isReplayDuplicate = noteSeenID(evt);

    // Show toast notification.
    var toastType = toastEvents[eventType];
    var message = data.message || data.title || "Event received";

    if (!isReplayDuplicate) {
      if (toastType === "success" && typeof window.showSuccessToast === "function") {
        window.showSuccessToast(message);
      } else if (toastType === "warning" && typeof window.showWarningToast === "function") {
        window.showWarningToast(message);
      } else if (typeof window.showToast === "function") {
        window.showToast(message);
      }
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

    // Keep the toast high-water mark in step with the shared id sequence so a
    // later toast event is not mistaken for a replay duplicate.
    noteSeenID(evt);

    // dashboard.action-resolved is the cross-tab counterpart of the
    // "dashboard:action-resolved" HTMX trigger the resolving tab sets on its
    // response. Re-dispatch that same body event here so the action-queue and
    // notification badge refresh in other open tabs via the existing HTMX
    // wiring (which listens for "dashboard:action-resolved from:body").
    if (eventType === "dashboard.action-resolved") {
      document.body.dispatchEvent(new CustomEvent("dashboard:action-resolved", {bubbles: true}));
      refreshNotificationBadge();
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
    // Flag this as a reconnect (not the initial connect) so the next
    // `connected` event fires the ProgressPill rehydrate fetches.
    // Cleared after rehydrate runs to avoid re-firing on subsequent
    // healthy reconnect-events that never disconnected.
    wasDisconnected = true;
    retryTimer = setTimeout(function () {
      retryTimer = null;
      connect();
    }, retryDelay);
    // Exponential backoff with cap.
    retryDelay = Math.min(retryDelay * 2, maxRetryDelay);
  }

  // rehydrateInflightOps queries the per-op status endpoints in parallel
  // and forwards any non-idle snapshot to the ProgressPill. Called only
  // after a true reconnect (not the initial page load) so refreshes during
  // a healthy session do not double-fire.
  //
  // Bulk-actions covers run-rules / re-identify / scan / fetch-images /
  // bulk-lock / bulk-unlock (all serialize through the bulk_action
  // singleton). Populate covers per-library populate jobs and may return
  // multiple in-flight snapshots.
  //
  // Failures are swallowed (warn-logged only): a missing status endpoint
  // on a stripped build, an in-flight backend deploy, or a momentary 5xx
  // must not crash the rehydrate path. The next SSE progress tick will
  // refresh the pill on its own.
  function rehydrateInflightOps() {
    if (typeof window.swProgressPill !== "object" || typeof window.swProgressPill.push !== "function") {
      return;
    }
    var base = bp;
    fetchAndForward(base + "/api/v1/artists/bulk-actions/status", function (snap) {
      // The bulk-actions handler returns {"status":"idle"} when nothing is
      // running; forward only non-idle snapshots so the pill does not
      // briefly render an empty placeholder. The status payload uses the
      // BulkActionProgress.snapshot() shape ({action, status, processed,
      // total, ...}) which differs from the ProgressPill event envelope
      // ({op_id, label, processed, total, status, cancel_url}); translate
      // the running case here so the pill key matches the original SSE
      // emission (op_id="bulk_action") and a future progress tick updates
      // the same pill rather than stacking a second one.
      if (!snap || !snap.status || snap.status === "idle") return;
      var status = snap.status;
      if (status === "running") {
        window.swProgressPill.push({
          op_id: "bulk_action",
          label: snap.action || "",
          processed: snap.processed || 0,
          total: snap.total || 0,
          status: "running",
          cancel_url: base + "/api/v1/artists/bulk-actions/cancel"
        });
      } else if (status === "completed" || status === "failed" || status === "canceled") {
        window.swProgressPill.push({
          op_id: "bulk_action",
          label: snap.action || "",
          processed: snap.processed || 0,
          total: snap.total || 0,
          status: status
        });
      }
    });
    fetchAndForward(base + "/api/v1/connections/populate/in-flight", function (resp) {
      // The populate aggregate returns {"operations":[...]} so we can
      // forward multiple per-library snapshots in one round trip. Each
      // entry is the same shape publishOpProgress emits on the SSE bus.
      if (!resp || !resp.operations || !resp.operations.length) return;
      for (var i = 0; i < resp.operations.length; i++) {
        var op = resp.operations[i];
        if (op && op.op_id) {
          window.swProgressPill.push(op);
        }
      }
    });
  }

  function fetchAndForward(url, onSnapshot) {
    fetch(url, {credentials: "same-origin", headers: {"Accept": "application/json"}}).then(function (resp) {
      if (!resp || !resp.ok) return null;
      return resp.json();
    }).then(function (data) {
      if (!data) return;
      try {
        onSnapshot(data);
      } catch (e) {
        if (window.console && console.warn) {
          console.warn("ProgressPill rehydrate forward failed:", e);
        }
      }
    }).catch(function (e) {
      if (window.console && console.warn) {
        console.warn("ProgressPill rehydrate fetch failed:", url, e);
      }
    });
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
