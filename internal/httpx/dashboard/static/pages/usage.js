/* usage.js — Usage & billing page module (plan 09-08, OA-29). Writes go
 * through window.Enclava.api only (D-17): budget create is a POST with the
 * full 08-07 contract, a limit edit is a PATCH carrying {limit_value} and
 * nothing else — no drawer action can ever reset liability. Success re-renders
 * the budgets fragment through Enclava.refresh(); failures surface in the
 * drawer. No inline handlers, no hx-on (D-20). */
(function () {
  const SYMBOLS = { USD: '$', EUR: '\u20AC', GBP: '\u00A3' };

  // Amount budgets are entered as decimal currency; the API stores integer
  // millionths (micros). The string form avoids float error: at most six
  // decimals, padded to six digits — exact by construction.
  function parseLimitValue(raw, kind) {
    const text = String(raw || '').trim();
    if (kind === 'tokens') {
      if (!/^\d+$/.test(text)) return { error: 'Enter a whole number of units greater than zero.' };
      const v = Number(text);
      if (!Number.isSafeInteger(v) || v > Math.floor(Number.MAX_SAFE_INTEGER / 2)) {
        return { error: 'Enter a limit no greater than 4503599627370496.' };
      }
      if (v === 0) return { error: 'Enter a limit greater than zero.' };
      return { value: v };
    }
    const m = /^(\d+)(?:\.(\d{1,6}))?$/.exec(text);
    if (!m) return { error: 'Enter a limit greater than zero — decimals allowed, e.g. 10.00.' };
    const micros = Number(m[1]) * 1e6 + Number((m[2] || '').padEnd(6, '0'));
    if (!Number.isSafeInteger(micros)) {
      return { error: 'Enter a limit no greater than 4503599627.370496.' };
    }
    if (micros === 0) return { error: 'Enter a limit greater than zero.' };
    return { value: micros };
  }

  // A plain integer amount limit smells like micros re-typed: "10000000"
  // stores $10,000,000 — warn inline without blocking the submit.
  function unitMistakeWarn(text, kind) {
    if (kind !== 'amount' || !/^\d+$/.test(text.trim())) return '';
    const n = Number(text);
    if (n >= 100) {
      return 'Looks like a whole-number entry — this stores ' + n.toLocaleString('en-US') +
        ' currency units. If you meant millionths (e.g. 10000000 = 10.00), divide it by 1,000,000.';
    }
    return '';
  }

  // The 'stored as' line keeps the raw integer the API receives visible.
  function updateBudgetLimitHint(form) {
    const kind = form.dataset.budgetKind || (form.elements.kind && form.elements.kind.value) || 'amount';
    const input = form.querySelector('#budget-limit, #budget-limit-value');
    const hint = form.querySelector('[data-budget-hint]');
    const warn = form.querySelector('[data-budget-warn]');
    const symbol = form.querySelector('[data-budget-symbol]');
    if (symbol) {
      if (kind === 'tokens') {
        symbol.hidden = true;
      } else {
        const cur = form.dataset.budgetCurrency || (form.elements.currency && form.elements.currency.value.trim().toUpperCase()) || 'USD';
        symbol.textContent = SYMBOLS[cur] || cur;
        symbol.hidden = false;
      }
    }
    if (warn) {
      const w = unitMistakeWarn(input ? input.value : '', kind);
      warn.textContent = w;
      warn.hidden = !w;
    }
    if (!hint || !input) return;
    if (kind === 'tokens') {
      hint.textContent = 'Whole units — the limit counts token units.';
      return;
    }
    const parsed = parseLimitValue(input.value, 'amount');
    const cur = form.dataset.budgetCurrency || (form.elements.currency && form.elements.currency.value.trim().toUpperCase()) || 'USD';
    hint.textContent = parsed.value !== undefined
      ? 'Stored as ' + String(parsed.value) + ' micro-' + cur + '.'
      : 'Enter the amount in the budget\u2019s currency, decimals allowed — e.g. 10.00.';
  }

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
    const kind = budgetId ? (form.dataset.budgetKind || 'amount') : String(fd.get('kind') || '');
    // UX-A09: the field takes a decimal currency amount (or whole units for
    // token budgets) and converts to the integer the API stores.
    const limit = parseLimitValue(fd.get('limit_value'), kind);
    if (limit.error) {
      showFormError(form, limit.error);
      return;
    }
    let request;
    if (budgetId) {
      request = window.Enclava.api('PATCH',
        '/api/v1/admin/budgets/' + encodeURIComponent(budgetId),
        { limit_value: limit.value });
    } else {
      const subjectType = String(fd.get('subject_type') || 'tenant');
      const tz = String(fd.get('timezone') || '').trim() || 'UTC';
      const body = {
        name: String(fd.get('name') || '').trim(),
        subject_type: subjectType,
        kind: kind,
        limit_value: limit.value,
        currency: kind === 'amount' ? (String(fd.get('currency') || '').trim().toUpperCase() || 'USD') : '',
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

  document.addEventListener('submit', (e) => {
    if (e.target.matches('form[data-reservation-form]')) e.preventDefault();
  });

  document.addEventListener('enclava:confirm', (e) => {
    const form = e.target.closest('form[data-reservation-form]');
    if (!form) return;
    const error = form.querySelector('.field-error');
    if (error) error.hidden = true;
    if (!form.reportValidity()) return;
    const fields = ['input_tokens', 'cached_input_tokens', 'output_tokens'];
    const body = {};
    for (const field of fields) {
      const raw = form.elements[field].value;
      const value = Number(raw);
      if (!/^\d+$/.test(raw) || !Number.isSafeInteger(value) || value < 0) {
        showFormError(form, 'Token counts must be non-negative whole numbers.');
        return;
      }
      body[field] = value;
    }
    if (body.cached_input_tokens > body.input_tokens) {
      showFormError(form, 'Cached input tokens must not exceed input tokens.');
      return;
    }
    window.Enclava.api('POST', '/api/v1/admin/reservations/' +
      encodeURIComponent(form.dataset.reservationId) + '/resolve', body).then(() => {
      window.Enclava.closeDrawer();
      window.Enclava.toast('success', 'Reservation resolved');
      window.Enclava.refresh();
    }).catch((err) => {
      const action = window.Enclava.errorAction(err);
      showFormError(form, action.text || (err && err.message) || 'Request failed');
    });
  });

  // Deactivate is a DELETE against the same budget row behind the shared
  // confirm group (app.js arms the group and fires enclava:confirm on the
  // trigger's Confirm); the row stays listed as inactive and admission stops
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
  // The kind select swaps the limit field between decimal currency and
  // whole units.
  document.addEventListener('change', (e) => {
    if (!(e.target instanceof Element)) return;
    if (e.target.id === 'budget-subject-type') {
      const type = e.target.value;
      document.querySelectorAll('#drawer [data-subject-for]').forEach((wrap) => {
        wrap.hidden = wrap.dataset.subjectFor !== type;
      });
    } else if (e.target.id === 'budget-kind') {
      const form = e.target.closest('form[data-usage-form]');
      if (form) updateBudgetLimitHint(form);
    }
  });

  // Typing a limit or currency refreshes the 'stored as' line, the currency
  // symbol prefix and the unit-mistake hint.
  document.addEventListener('input', (e) => {
    if (!(e.target instanceof Element)) return;
    if (!e.target.matches('#budget-limit, #budget-limit-value, #budget-currency')) return;
    const form = e.target.closest('form[data-usage-form]');
    if (form) updateBudgetLimitHint(form);
  });
})();

