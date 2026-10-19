 (function () {
  function csrfToken() {
    var match = document.cookie.match(/(?:^|;\s*)enclava_csrf=([^;]*)/);
    return match ? decodeURIComponent(match[1]) : '';
  }
  function api(path, options) {
    options = options || {};
    options.credentials = 'same-origin';
    options.headers = options.headers || {};
    if (csrfToken()) options.headers['X-CSRF-Token'] = csrfToken();
    return fetch(path, options).then(function (response) {
      return response.json().catch(function () { return {}; }).then(function (body) {
        if (!response.ok) throw new Error(body.error && body.error.message || 'Request failed.');
        return body;
      });
    });
  }
  document.addEventListener('click', function (event) {
    var disable = event.target.closest('.provider-disable-endpoint');
    if (disable) {
      event.preventDefault();
      if (!window.confirm('Disable endpoint ' + disable.dataset.id + '? It will stop receiving inference requests.')) return;
      var status = document.getElementById('provider-endpoint-message');
      api('/api/v1/providers/' + encodeURIComponent(disable.dataset.id), { method: 'DELETE' })
        .then(function () { window.location.reload(); })
        .catch(function (error) { status.textContent = error.message || 'Could not disable endpoint.'; });
      return;
    }
    var button = event.target.closest('.provider-edit-endpoint');
    var form = document.getElementById('provider-endpoint-form');
    if (!form) return;
    if (button) {
      event.preventDefault();
      form.dataset.endpointID = button.dataset.id;
      form.elements.id.value = button.dataset.id;
      form.elements.id.readOnly = true;
      form.elements.protocol.value = button.dataset.protocol;
      form.elements.assurance_mode.value = button.dataset.assuranceMode || '';
      form.elements.base_origin.value = button.dataset.origin;
      form.elements.capabilities.value = (button.dataset.capabilities || '').split(',').filter(Boolean).join(',');
      form.elements.credential_ref.value = button.dataset.credentialRef || '';
      form.elements.authorized_private.checked = button.dataset.private === 'true';
      form.querySelector('button[type="submit"]').textContent = 'Save endpoint changes';
      document.getElementById('provider-endpoint-cancel').hidden = false;
      form.scrollIntoView({ behavior: 'smooth', block: 'center' });
      return;
    }
    if (event.target.id === 'provider-endpoint-cancel') {
      form.reset();
      delete form.dataset.endpointID;
      form.elements.id.readOnly = false;
      form.querySelector('button[type="submit"]').textContent = 'Add endpoint';
      event.target.hidden = true;
    }
  });
  document.addEventListener('submit', function (event) {
    var form = event.target;
    if (!form) return;
    if (form.id === 'provider-endpoint-form') {
      event.preventDefault();
      var data = new FormData(form);
      var capabilities = String(data.get('capabilities') || '').split(',').map(function (v) { return v.trim(); }).filter(Boolean);
      var payload = {
        id: String(data.get('id') || '').trim(), protocol: data.get('protocol'),
        assurance_mode: String(data.get('assurance_mode') || ''),
        base_origin: String(data.get('base_origin') || '').trim(), capabilities: capabilities,
        credential_ref: String(data.get('credential_ref') || '').trim(),
        authorized_private: form.elements.authorized_private.checked
      };
      var message = document.getElementById('provider-endpoint-message');
      var editing = form.dataset.endpointID;
      var path = editing ? '/api/v1/providers/' + encodeURIComponent(editing) : '/api/v1/providers';
      api(path, { method: editing ? 'PUT' : 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload) })
        .then(function () { window.location.reload(); })
        .catch(function (error) { message.textContent = error.message || 'Could not save endpoint.'; });
      return;
    }
    if (form.id !== 'provider-policy-form') return;
    event.preventDefault();
    var data = new FormData(form);
    var denied = data.getAll('deny');
    var policy = {
      subject_kind: String(data.get('subject_kind') || ''),
      subject_id: String(data.get('subject_id') || '').trim(),
      required_assurance: String(data.get('required_assurance') || ''),
      denied_capabilities: denied
    };
    var message = document.getElementById('provider-policy-message');
    api('/api/v1/processing-restrictions', {
      method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(policy)
    }).then(function () { message.textContent = 'Restrictions saved and applied by inference policy.'; })
      .catch(function (error) { message.textContent = error.message || 'Could not save restrictions.'; });
  });
  document.addEventListener('change', function (event) {
    var form = event.target && event.target.form;
    if (!form || form.id !== 'provider-policy-form') return;
    if (event.target.name === 'subject_kind') {
      form.elements.subject_id.value = event.target.value === 'tenant' ? form.dataset.tenantId : '';
      form.elements.required_assurance.value = '';
      Array.prototype.forEach.call(form.querySelectorAll('input[name=\"deny\"]'), function (input) { input.checked = false; });
    }
    if (event.target.name !== 'subject_kind' && event.target.name !== 'subject_id') return;
    var kind = form.elements.subject_kind.value;
    var id = String(form.elements.subject_id.value || '').trim();
    if (!id) return;
    var query = '?subject_kind=' + encodeURIComponent(kind) + '&subject_id=' + encodeURIComponent(id);
    api('/api/v1/processing-restrictions' + query).then(function (body) {
      var row = body.restriction || {};
      var capabilities = row.DeniedCapabilities || row.denied_capabilities || [];
      Array.prototype.forEach.call(form.querySelectorAll('input[name=\"deny\"]'), function (input) {
        input.checked = capabilities.indexOf(input.value) !== -1;
      });
      form.elements.required_assurance.value = row.RequiredAssurance || row.required_assurance || '';
    }).catch(function (error) {
      document.getElementById('provider-policy-message').textContent = error.message || 'Could not load restrictions for this subject.';
    });
  });
  var policyForm = document.getElementById('provider-policy-form');
  if (policyForm) {
    policyForm.elements.subject_id.dispatchEvent(new Event('change', { bubbles: true }));
  }
})();
