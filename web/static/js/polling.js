// pollAsyncStatus polls a URL at a fixed interval until a terminal condition
// is reached. All async-operation polling in the UI should use this helper to
// avoid duplicating the setInterval/fetch/maxAttempts boilerplate.
//
// Parameters:
//   url       - Status endpoint to fetch on each tick.
//   callbacks - Object with event handlers:
//       onData(data)      - Called with parsed JSON each tick. Return true to stop polling.
//       onTimeout()       - Called when maxAttempts is exceeded.
//       onHTTPError(status) - Called when the response is not ok (receives HTTP status code).
//       onNetworkError()  - Called on fetch/network failure.
//   options   - Optional overrides:
//       intervalMs   (default 2000)
//       maxAttempts  (default 150)
//       headers      (default {})
//       credentials  (default 'same-origin')
//
// Returns an object with a stop() method to cancel polling externally.
function pollAsyncStatus(url, callbacks, options) {
  options = options || {};
  var intervalMs = options.intervalMs || 2000;
  var maxAttempts = options.maxAttempts || 150;
  var headers = options.headers || {};
  var credentials = options.credentials || 'same-origin';
  var attempts = 0;

  var poll = setInterval(function () {
    if (++attempts > maxAttempts) {
      clearInterval(poll);
      if (callbacks.onTimeout) callbacks.onTimeout();
      return;
    }
    fetch(url, { headers: headers, credentials: credentials })
      .then(function (r) {
        if (!r.ok) {
          clearInterval(poll);
          if (callbacks.onHTTPError) callbacks.onHTTPError(r.status);
          return null;
        }
        return r.json().catch(function () {
          // Server returned 200 but non-JSON body (e.g. proxy error page).
          clearInterval(poll);
          if (callbacks.onHTTPError) callbacks.onHTTPError(r.status);
          return null;
        });
      })
      .then(function (data) {
        if (!data) return;
        if (callbacks.onData(data)) {
          clearInterval(poll);
        }
      })
      .catch(function (err) {
        clearInterval(poll);
        if (callbacks.onNetworkError) callbacks.onNetworkError(err);
      });
  }, intervalMs);

  return {
    stop: function () {
      clearInterval(poll);
    }
  };
}
