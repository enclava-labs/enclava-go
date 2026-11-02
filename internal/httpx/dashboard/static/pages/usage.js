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
  // Calendar days mean local midnight in the form's IANA timezone — resolve
  // the zone's UTC offset at the candidate instant (DST-correct) via Intl.
  function zonedParts(tz, ms) {
    const parts = new Intl.DateTimeFormat('en-US', {
      timeZone: tz, hourCycle: 'h23', // never "24" for midnight
      year: 'numeric', month: '2-digit', day: '2-digit',
      hour: '2-digit', minute: '2-digit', second: '2-digit',
    }).formatToParts(new Date(ms));
    const got = {};
    for (const p of parts) got[p.type] = p.value;
    return got;
  }
  function zoneOffsetMs(tz, ms) {
    const got = zonedParts(tz, ms);
    return Date.UTC(+got.year, +got.month - 1, +got.day, +got.hour, +got.minute, +got.second) - ms;
  }
  function zonedDayKey(tz, ms) {
    const got = zonedParts(tz, ms);
    return got.year + '-' + got.month + '-' + got.day;
  }
  function zonedMidnight(day, tz) {
    const guess = Date.parse(day + 'T00:00:00Z');
    if (isNaN(guess)) return null;
    try {
      // The guess's offset may be the neighbouring day's — re-resolve once.
      const ms = guess - zoneOffsetMs(tz, guess - zoneOffsetMs(tz, guess));
      const key = zonedDayKey(tz, ms);
      if (key === day) return new Date(ms);
      // Midnight was skipped by a DST gap (America/Santiago 2025-09-07) or the
      // whole day was skipped — resolve forward to the day's first instant:
      // bound the bracketing instants hourly, then binary-search the boundary.
      let lo = ms, hi = ms;
      if (key < day) {
        for (let i = 0; i < 24 * 32 && zonedDayKey(tz, hi) < day; i++) hi += 3600000;
      } else {
        for (let i = 0; i < 24 * 32 && zonedDayKey(tz, lo) > day; i++) lo -= 3600000;
      }
      while (hi - lo > 1) {
        const mid = lo + Math.floor((hi - lo) / 2);
        if (zonedDayKey(tz, mid) < day) lo = mid; else hi = mid;
      }
      return new Date(hi);
    } catch (e) {
      // Unrecognized zone: fall back to UTC; the server re-validates anyway.
      return new Date(guess);
    }
  }
  function isoFromDay(value, tz, fallbackNow) {
    if (value) {
      const d = zonedMidnight(value, tz || 'UTC');
      if (d) return d.toISOString();
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
      const tz = String(fd.get('timezone') || '').trim() || 'UTC';
      const body = {
        name: String(fd.get('name') || '').trim(),
        subject_type: subjectType,
        kind: String(fd.get('kind') || ''),
        limit_value: Number(fd.get('limit_value')),
        currency: String(fd.get('currency') || '').trim().toUpperCase(),
        period: String(fd.get('period') || ''),
        timezone: tz,
        starts_at: isoFromDay(fd.get('starts_at'), tz, true),
      };
      if (subjectType !== 'tenant') {
        const picker = form.querySelector('[data-subject-for="' + subjectType + '"] select');
        body.subject_id = picker ? String(picker.value || '') : '';
      }
      const endsAt = isoFromDay(fd.get('ends_at'), tz, false);
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

  // Deactivate is a DELETE against the same budget row behind the shared
  // two-click confirm (app.js re-arms and fires enclava:confirm on the
  // second click); the row stays listed as inactive and admission stops
  // checking it.
  document.addEventListener('enclava:confirm', (e) => {
    const btn = e.target instanceof Element ? e.target.closest('[data-budget-id]') : null;
    if (!btn || !btn.dataset.budgetId) return;
    window.Enclava.api('DELETE', '/api/v1/admin/budgets/' + encodeURIComponent(btn.dataset.budgetId))
      .then(() => {
        window.Enclava.toast('success', 'Budget deactivated');
        window.Enclava.refresh();
      }).catch((err) => {
        const action = window.Enclava.errorAction(err);
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
