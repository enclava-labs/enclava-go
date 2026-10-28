/* usage.js — Usage & billing page module (plan 09-08, OA-29). Writes go
 * through window.Enclava.api only (D-17): budget create is a POST with the
 * full 08-07 contract, a limit edit is a PATCH carrying {limit_value} and
 * nothing else — no drawer action can ever reset liability. Success re-renders
 * the budgets fragment through Enclava.refresh(); failures surface in the
 * drawer. No inline handlers, no hx-on (D-20). */
(function () {
  function showFormError(form, message) {
    const el = form.querySelector('.field-error');
    if (el) {
      el.textContent = message;
      el.hidden = false;
    } else {
      window.Enclava.toast('error', message);
    }
  }

  // Date inputs carry YYYY-MM-DD; the budget API needs RFC3339 timestamps.
  function isoFromDay(value, fallbackNow) {
    if (value) {
      const d = new Date(value + 'T00:00:00Z');
      if (!isNaN(d.getTime())) return d.toISOString();
    }
    return fallbackNow ? new Date().toISOString() : '';
  }

  document.addEventListener('submit', (e) => {
    const form = e.target.closest('form[data-usage-form]');
    if (!form) return;
    e.preventDefault();
    const error = form.querySelector('.field-error');
    if (error) error.hidden = true;

    const fd = new FormData(form);
    const budgetId = form.dataset.budgetId || '';
    let request;
    if (budgetId) {
      request = window.Enclava.api('PATCH',
        '/api/v1/admin/budgets/' + encodeURIComponent(budgetId),
        { limit_value: Number(fd.get('limit_value')) });
    } else {
      const subjectType = String(fd.get('subject_type') || 'tenant');
      const body = {
        name: String(fd.get('name') || '').trim(),
        subject_type: subjectType,
        kind: String(fd.get('kind') || ''),
        limit_value: Number(fd.get('limit_value')),
        currency: String(fd.get('currency') || '').trim().toUpperCase(),
        period: String(fd.get('period') || ''),
        starts_at: isoFromDay(fd.get('starts_at'), true),
      };
      if (subjectType !== 'tenant') {
        const picker = form.querySelector('[data-subject-for="' + subjectType + '"] select');
        body.subject_id = picker ? String(picker.value || '') : '';
      }
      const endsAt = isoFromDay(fd.get('ends_at'), false);
      if (endsAt) body.ends_at = endsAt;
      request = window.Enclava.api('POST', '/api/v1/admin/budgets', body);
    }
    request.then(() => {
      window.Enclava.closeDrawer();
      window.Enclava.toast('success', budgetId ? 'Budget limit updated' : 'Budget created');
      window.Enclava.refresh();
    }).catch((err) => {
      const action = window.Enclava.errorAction(err);
      showFormError(form, action.text || (err && err.message) || 'Request failed');
      window.Enclava.toast('error', action.text || (err && err.message) || 'Request failed',
        { action: action });
    });
  });

  // The subject type select shows exactly one bounded picker; the server
  // re-validates every subject binding regardless of what the form sends.
  document.addEventListener('change', (e) => {
    if (!(e.target instanceof Element) || e.target.id !== 'budget-subject-type') return;
    const type = e.target.value;
    document.querySelectorAll('#drawer [data-subject-for]').forEach((wrap) => {
      wrap.hidden = wrap.dataset.subjectFor !== type;
    });
  });
})();
