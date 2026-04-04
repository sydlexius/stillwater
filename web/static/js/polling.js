// pollAsyncStatus polls a URL at a fixed interval until a terminal condition
// is reached. All async-operation polling in the UI should use this helper to
// avoid duplicating the setInterval/fetch/maxAttempts boilerplate.
//
// Parameters:
//   url       - Status endpoint to fetch on each tick.
//   callbacks - Object with event handlers:
//       onData(data)      - Called with parsed JSON each tick. Return true to stop polling.
//       onTimeout()       - Called when maxAttempts is exceeded (only when maxAttempts > 0).
//       onHTTPError(status) - Called when the response is not ok (receives HTTP status code).
//       onNetworkError()  - Called on fetch/network failure.
//   options   - Optional overrides:
//       intervalMs   (default 2000)
//       maxAttempts  (default 0; 0 means no timeout -- poll until done or error)
//       headers      (default {})
//       credentials  (default 'same-origin')
//
// Returns an object with a stop() method to cancel polling externally.
function pollAsyncStatus(url, callbacks, options) {
  options = options || {};
  var intervalMs = options.intervalMs || 2000;
  var maxAttempts = (options.maxAttempts !== undefined) ? options.maxAttempts : 0;
  var headers = options.headers || {};
  var credentials = options.credentials || 'same-origin';
  var attempts = 0;
  var stopped = false;
  var timer = null;

  function stop() {
    stopped = true;
    if (timer) clearTimeout(timer);
  }

  function tick() {
    if (stopped) return;
    if (maxAttempts > 0 && ++attempts > maxAttempts) {
      stop();
      if (callbacks.onTimeout) callbacks.onTimeout();
      return;
    }
    fetch(url, { headers: headers, credentials: credentials })
      .then(function (r) {
        if (stopped) return null;
        if (!r.ok) {
          stop();
          if (callbacks.onHTTPError) callbacks.onHTTPError(r.status);
          return null;
        }
        return r.json().catch(function () {
          // Server returned 200 but non-JSON body (e.g. proxy error page).
          stop();
          if (callbacks.onHTTPError) callbacks.onHTTPError(r.status);
          return null;
        });
      })
      .then(function (data) {
        if (stopped || !data) return;
        if (callbacks.onData && callbacks.onData(data)) {
          stop();
          return;
        }
        if (!stopped) timer = setTimeout(tick, intervalMs);
      })
      .catch(function (err) {
        if (stopped) return;
        stop();
        if (callbacks.onNetworkError) callbacks.onNetworkError(err);
      });
  }

  timer = setTimeout(tick, intervalMs);

  return {
    stop: function () {
      stop();
    }
  };
}
