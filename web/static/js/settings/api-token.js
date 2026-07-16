// Settings: extracted from the inline apiTokenScript() (M55 #1808).
// Behavior-preserving lift out of web/templates/settings.templ; the JS is
// verbatim except for this IIFE wrapper, the load-once guard, the window
// re-export of createAPIToken (called by name from the create-token form), and
// the CSRF read routed through the canonical window.swCsrfToken() helper
// (preferences.js) instead of an inline cookie-parse regex.
//
// DOM contract: bound via onsubmit="createAPIToken(event)" on #create-token-form;
//   reads name + scope_read/scope_write/scope_webhook/scope_admin checkboxes;
//   on success fills #token-plaintext (a readonly <input>, #2526) and unhides
//   #token-created-result.
// Network: POST {base}/api/v1/auth/tokens with {name, scopes}; sends csrf_token
//   as X-CSRF-Token. Errors surface via alert() (verbatim original behavior).
//
// Export surface: window.swApiToken doubles as the load-once guard.
// createAPIToken is assigned to window because the form calls it by bare name.
(function () {
  'use strict';

  if (window.swApiToken) return;

  function createAPIToken(event) {
    event.preventDefault();
    var form = event.target;
    var name = form.elements['name'].value;
    var scopes = [];
    if (form.elements['scope_read'].checked) scopes.push('read');
    if (form.elements['scope_write'].checked) scopes.push('write');
    if (form.elements['scope_webhook'].checked) scopes.push('webhook');
    if (form.elements['scope_admin'].checked) scopes.push('admin');
    if (scopes.length === 0) {
      alert('Select at least one scope');
      return;
    }
    var bp = (document.querySelector('meta[name="htmx-base-path"]') || {content: ''}).content;
    var csrfToken;
    if (typeof window.swCsrfToken === 'function') {
      csrfToken = window.swCsrfToken();
    } else {
      console.error("swCsrfToken unavailable - preferences.js may have failed to load; state-changing requests will 403");
      csrfToken = '';
    }
    fetch(bp + '/api/v1/auth/tokens', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-CSRF-Token': csrfToken
      },
      body: JSON.stringify({ name: name, scopes: scopes.join(',') })
    })
    .then(function(resp) {
      if (!resp.ok) {
        return resp.json().then(function(body) {
          throw new Error(body.error || 'Failed to create token');
        }, function() {
          throw new Error('Failed to create token');
        });
      }
      return resp.json();
    })
    .then(function(data) {
      document.getElementById('token-plaintext').value = data.token;
      document.getElementById('token-created-result').classList.remove('hidden');
      form.reset();
      form.elements['scope_read'].checked = true;
    })
    .catch(function(err) {
      alert(err.message);
    });
  }

  window.createAPIToken = createAPIToken;

  window.swApiToken = { create: createAPIToken };
})();
