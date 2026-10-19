(function () {
  function csrfToken() {
    var match = document.cookie.match(/(?:^|;\s*)enclava_csrf=([^;]*)/);
    return match ? decodeURIComponent(match[1]) : '';
  }
  document.addEventListener('submit', function (event) {
    var form = event.target;
    if (!form || form.id !== 'provider-endpoint-form') return;
    event.preventDefault();
    var data = new FormData(form);
    var capabilities = String(data.get('capabilities') || '').split(',').map(function (v) { return v.trim(); }).filter(Boolean);
    var payload = {
      id: String(data.get('id') || '').trim(), protocol: data.get('protocol'),
      base_origin: String(data.get('base_origin') || '').trim(), capabilities: capabilities,
      credential_ref: String(data.get('credential_ref') || '').trim(),
      authorized_private: form.elements.authorized_private.checked
    };
    var message = document.getElementById('provider-endpoint-message');
    var headers = { 'Content-Type': 'application/json' };
    if (csrfToken()) headers['X-CSRF-Token'] = csrfToken();
    fetch('/api/v1/providers', { method: 'POST', credentials: 'same-origin', headers: headers, body: JSON.stringify(payload) })
      .then(function (response) {
        return response.json().catch(function () { return {}; }).then(function (body) {
          if (!response.ok) throw new Error(body.error && body.error.message || 'Could not save endpoint.');
          return body;
        });
      })
      .then(function () { message.textContent = 'Endpoint saved. Reload to view current status.'; form.reset(); })
      .catch(function (error) { message.textContent = error.message || 'Could not save endpoint.'; });
  });
})();
