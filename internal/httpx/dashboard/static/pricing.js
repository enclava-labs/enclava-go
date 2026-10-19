(function () {
  function csrfToken() {
    var match = document.cookie.match(/(?:^|;\s*)enclava_csrf=([^;]*)/);
    return match ? decodeURIComponent(match[1]) : '';
  }
  document.addEventListener('click', function (event) {
    var button = event.target.closest('.pricing-edit');
    if (!button) return;
    var form = document.getElementById('pricing-form');
    if (!form) return;
    ['subject_type', 'subject_id', 'capability', 'unit', 'rate_micros', 'unit_quantity', 'currency'].forEach(function (field) {
      form.elements[field].value = button.dataset[field.replace(/_([a-z])/g, function (_, letter) { return letter.toUpperCase(); })] || '';
    });
    form.querySelector('button[type="submit"]').textContent = 'Create effective price revision';
    form.scrollIntoView({ behavior: 'smooth', block: 'center' });
  });
  document.addEventListener('submit', function (event) {
    var form = event.target;
    if (!form || form.id !== 'pricing-form') return;
    event.preventDefault();
    var data = new FormData(form);
    var payload = {};
    ['subject_type', 'subject_id', 'capability', 'unit', 'currency', 'effective_from'].forEach(function (key) {
      payload[key] = String(data.get(key) || '').trim();
    });
    payload.rate_micros = Number(data.get('rate_micros'));
    payload.unit_quantity = Number(data.get('unit_quantity'));
    if (payload.effective_from) payload.effective_from = new Date(payload.effective_from).toISOString();
    else delete payload.effective_from;
    var token = csrfToken();
    fetch('/api/v1/admin/pricing', {
      method: 'POST', credentials: 'same-origin',
      headers: Object.assign({ 'Content-Type': 'application/json' }, token ? { 'X-CSRF-Token': token } : {}),
      body: JSON.stringify(payload)
    }).then(function (response) {
      return response.json().catch(function () { return {}; }).then(function (body) {
        if (!response.ok) throw new Error(body.error && body.error.message || 'Could not save price.');
        return body;
      });
    }).then(function () {
      window.location.reload();
    }).catch(function (error) {
      document.getElementById('pricing-message').textContent = error.message || 'Could not save price.';
    });
  });
})();
