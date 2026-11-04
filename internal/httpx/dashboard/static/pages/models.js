/* models.js — Models + Pricing page module (OA-27, P3.6).
 *
 * Namespaced data-actions (page-local; shared actions stay in app.js):
 *   models:endpoint  open the add/edit endpoint drawer (data-endpoint-id)
 *   models:price     open the Set-price form covering all three categories
 *   models:history   open the price-history drawer for the row pair
 *
 * Writes go through Enclava.api → /api/v1 (D-17) and fragments re-render
 * via Enclava.refresh(). The endpoint payload carries only endpointRequest
 * fields — assurance_verified is NEVER sent: it is observed verification
 * state and the API rejects payloads that set it.
 */
(function () {
  if (window.__enclavaModelsPage) return;
  window.__enclavaModelsPage = true;

  const SYMBOLS = { USD: '$', EUR: '\u20AC', GBP: '\u00A3' };

  // Prices are entered and shown as currency per 1M tokens, as price lists
  // quote them. rate_micros per unit_quantity tokens is exactly
  // rate_micros / unit_quantity currency units per 1M, so both directions are
  // exact; past six decimals the rate moves to a larger token block instead
  // of being rounded.
  function priceToRate(text) {
    const m = /^(\d+)(?:\.(\d*))?$/.exec(String(text).trim());
    if (!m) return null;
    const frac = (m[2] || '').replace(/0+$/, '');
    const extra = Math.max(0, frac.length - 6);
    if (extra > 6) return null; // finer than one micro per trillion tokens
    const rate = Number(m[1] + frac.padEnd(6 + extra, '0'));
    if (!Number.isSafeInteger(rate)) return null;
    return { rate_micros: rate, unit_quantity: 1000000 * 10 ** extra };
  }

  // The stored rate as a per-1M decimal; '' when unit_quantity is not a power
  // of ten (set over the API), so the form never shows a rounded value.
  function rateToPrice(rateMicros, unitQuantity) {
    const q = String(unitQuantity);
    if (!/^10*$/.test(q)) return '';
    const places = q.length - 1;
    const digits = String(rateMicros).padStart(places + 1, '0');
    const whole = digits.slice(0, digits.length - places);
    const frac = digits.slice(digits.length - places).replace(/0+$/, '');
    return frac ? whole + '.' + frac : whole;
  }

  // Mirror of the server tokenPrice formatter: "$0.001", "$2.50", "1,500.00 NOK".
  function fmtPrice(price, currency) {
    const cur = (currency || 'USD').toUpperCase();
    const [whole, frac = ''] = price.split('.');
    const amount = whole.replace(/\B(?=(\d{3})+(?!\d))/g, ',') + '.' + frac.padEnd(2, '0');
    const sym = SYMBOLS[cur];
    return sym ? sym + amount : amount + ' ' + cur;
  }

  // API failures keep their inline slot AND surface the shared
  // error→action toast (OA-35): a fix link (or Try again) when the central
  // map has one, nothing extra when the failure is already explained.
  function errorToast(err) {
    if (typeof Enclava.errorAction !== 'function') return false;
    const a = Enclava.errorAction(err);
    if (!a || !(a.href || a.retry)) return false;
    Enclava.toast('error', a.text || (err && err.message), { action: a });
    return true;
  }

  // Non-form writes have no inline slot: the toast carries the map's
  // sentence (member-safe when capability-filtered) plus any fix action.
  function errorToastOr(err, fallback) {
    if (errorToast(err)) return;
    const a = typeof Enclava.errorAction === 'function' && Enclava.errorAction(err);
    Enclava.toast('error', (a && a.text) || (err && err.message) || fallback);
  }

  function showFormError(form, message) {
    const slot = form.querySelector('[data-models-form-error]');
    if (slot) {
      slot.textContent = message;
      slot.hidden = false;
    } else {
      Enclava.toast('error', message);
    }
  }

  // "gpt-4o, acme=up-1" → ["model:gpt-4o", "model:acme=up-1"]; an empty
  // alias or segment is rejected before anything is sent.
  function parseAliases(raw) {
    const bindings = [];
    for (const part of raw.split(',')) {
      const value = part.trim();
      if (!value) continue;
      const at = value.indexOf('=');
      const alias = (at >= 0 ? value.slice(0, at) : value).trim();
      if (!alias) return { error: 'Model aliases need at least one name before "=".' };
      if (at >= 0) {
        const upstream = value.slice(at + 1).trim();
        if (!upstream) return { error: `Alias "${alias}" needs an upstream id after "=".` };
        bindings.push('model:' + alias + '=' + upstream);
      } else {
        bindings.push('model:' + alias);
      }
    }
    return { bindings };
  }

  async function submitEndpoint(form) {
    const editing = form.dataset.endpointId;
    const aliases = parseAliases(form.querySelector('#mep-models').value);
    if (aliases.error) {
      showFormError(form, aliases.error);
      return;
    }
    const capabilities = form.elements.capabilities.value
      .split(',').map((s) => s.trim()).filter(Boolean).concat(aliases.bindings);
    const body = {
      protocol: form.elements.protocol.value,
      sdk_provider: form.elements.sdk_provider.value.trim(),
      capabilities: capabilities,
      base_origin: form.elements.base_origin.value.trim(),
      enclave_repo: (form.elements.enclave_repo ? form.elements.enclave_repo.value : '').trim(),
      credential_ref: form.elements.credential_ref.value.trim(),
      assurance_mode: form.elements.assurance_mode.value,
      authorized_private: form.elements.authorized_private.checked,
      is_enabled: form.elements.is_enabled.checked,
    };
    if (!editing) {
      const id = form.elements.id.value.trim();
      if (id) body.id = id;
    }
    const path = editing
      ? '/api/v1/providers/' + encodeURIComponent(editing)
      : '/api/v1/providers';
    try {
      await Enclava.api(editing ? 'PUT' : 'POST', path, body);
      Enclava.toast('success', editing ? 'Endpoint saved' : 'Endpoint added');
      Enclava.closeDrawer();
      Enclava.refresh();
    } catch (err) {
      showFormError(form, err.message || 'Saving the endpoint failed');
      errorToast(err);
    }
  }

  // Discover reads the endpoint's own server (/v1/models plus 1-token
  // capability probes) and prefills capabilities and aliases with observed
  // fact. Only a 'yes' probe ADDS a capability — 'unknown'/'no' never remove
  // a saved selection (PR-I02), and per-capability probe detail is listed
  // so the merge stays auditable.
  async function discoverEndpoint(form, button) {
  button.disabled = true;
  const body = {
    protocol: form.elements.protocol.value,
    sdk_provider: form.elements.sdk_provider.value.trim(),
    base_origin: form.elements.base_origin.value.trim(),
    enclave_repo: (form.elements.enclave_repo ? form.elements.enclave_repo.value : '').trim(),
    credential_ref: form.elements.credential_ref.value.trim(),
    authorized_private: form.elements.authorized_private.checked,
  };
  try {
    const res = await Enclava.api('POST', '/api/v1/providers/discover', body);
    const discovered = (res && res.discovered) || {};
    const models = discovered.models || [];
    if (models.length) {
      form.querySelector('#mep-models').value = models.join(', ');
    }

    // Merge into the saved selection: 'yes' confirms/adds, anything else
    // keeps whatever was already selected — an unprobeable capability must
    // never silently lose its saved claim.
    const kept = form.elements.capabilities.value
      .split(',').map((s) => s.trim()).filter(Boolean);
    let confirmed = 0;
    form.querySelectorAll('[data-capability-chip]').forEach((chip) => {
      const key = chip.dataset.capabilityChip;
      const verdict = discovered.capabilities && discovered.capabilities[key];
      const label = chip.textContent.replace(/ [✓✗?]$/, '');
      const selected = chip.classList.contains('active');
      if (verdict === 'yes') {
        confirmed += 1;
        if (!kept.includes(key)) kept.push(key);
        chip.textContent = label + ' ✓';
        chip.classList.add('active');
      } else {
        chip.textContent = label + (selected ? ' ✓' : ' ?');
      }
    });
    form.elements.capabilities.value = kept.join(',');

    // Every probe row renders the capability, the model it ran against and
    // its verdict — the "unknown" a chip keeps is now traceable.
    const probesSlot = form.querySelector('[data-models-probe-results]');
    if (probesSlot) {
      probesSlot.innerHTML = '';
      const probes = discovered.probes || [];
      if (probes.length) {
        const ul = document.createElement('ul');
        ul.className = 'probe-results';
        probes.forEach((p) => {
          const li = document.createElement('li');
          li.textContent = (p.capability || '?') + ': ' + (p.result || 'unknown') +
            (p.model ? ' · ' + p.model : '') + (p.error ? ' — ' + p.error : '');
          li.className = 'probe-' + (p.result === 'yes' ? 'yes' : p.result === 'no' ? 'no' : 'unknown');
          ul.appendChild(li);
        });
        probesSlot.appendChild(ul);
      }
    }

    showFormError(form,
      'Discovered ' + models.length + ' model' + (models.length === 1 ? '' : 's') +
      (discovered.probe_model ? ' (probed with ' + discovered.probe_model + ')' : '') +
      ' · ' + confirmed + (confirmed === 1 ? ' capability' : ' capabilities') + ' confirmed — existing selections kept');

    const slot = form.querySelector('[data-models-form-error]');
    if (slot) slot.classList.add('muted');
    Enclava.toast('success', 'Discovery complete');
  } catch (err) {
    const slot = form.querySelector('[data-models-form-error]');
    if (slot) slot.classList.remove('muted');
    showFormError(form, err.message || 'Discovery failed');
  } finally {
    button.disabled = false;
  }
}
  async function disableEndpoint(button) {
    const id = button.dataset.endpointId;
    if (!id) return;
    try {
      await Enclava.api('DELETE', '/api/v1/providers/' + encodeURIComponent(id));
      Enclava.toast('success', 'Endpoint disabled');
      Enclava.closeDrawer();
      Enclava.refresh();
    } catch (err) {
      errorToastOr(err, 'Disabling the endpoint failed');
    }
  }

  // The price form's unit fields mirror the matrix columns (input /
  // cached_input / output); labels are re-rendered, never trusted.
  const PRICE_UNITS = [
    { unit: 'input', label: 'Input' },
    { unit: 'cached_input', label: 'Cached input' },
    { unit: 'output', label: 'Output' },
  ];

  // One Set-price form for the whole row (P-05): every category entered is
  // sent in a single atomic batch write, so a save can never leave a
  // half-priced matrix. Cells carry their stored rate as data-* for prefill.
  // Aliases and ids are admin-entered: they go in as text, never markup.
  function openPricePopover(anchor) {
    const d = anchor.dataset;
    const form = document.createElement('form');
    form.setAttribute('data-models-price-form', '');
    form.dataset.endpoint = d.endpoint;
    form.dataset.model = d.model;
    form.dataset.capability = d.capability;
    form.dataset.subjectType = d.subjectType;
    form.dataset.subjectId = d.subjectId;
    form.className = 'price-pop';
    let fields = '';
    PRICE_UNITS.forEach((u) => {
      fields +=
        '<label class="label" for="models-price-rate-' + u.unit + '">' + u.label + ' price per 1M tokens</label>' +
        '<input class="field" id="models-price-rate-' + u.unit + '" name="price_' + u.unit +
        '" type="number" min="0" step="any" inputmode="decimal" data-unit="' + u.unit +
        '" aria-describedby="models-price-help-' + u.unit + '">' +
        '<span class="help" id="models-price-help-' + u.unit + '" data-models-price-preview="' + u.unit +
        '">Leave empty to keep ' + u.label.toLowerCase() + ' unpriced.</span>';
    });
    form.innerHTML =
      '<div class="drawer-head"><h2>Set prices</h2><button type="button" class="iconbtn" data-action="close-drawer" aria-label="Close drawer">×</button></div>' +
      '<p class="help" data-models-price-pair></p>' +
      '<p class="help" data-models-price-scope></p>' +
      fields +
      '<label class="label" for="models-price-currency">Currency — shared by every entered price</label>' +
      '<input class="field" id="models-price-currency" name="currency" maxlength="3">' +
      '<p class="field-error" data-models-price-error hidden></p>' +
      '<div class="row"><button type="submit" class="btn primary">Save prices</button></div>';
    form.querySelector('[data-models-price-pair]').textContent = d.model + ' @ ' + d.endpoint + ' · ' + d.capability;
    form.querySelector('[data-models-price-scope]').textContent = d.subjectType === 'endpoint'
      ? 'Endpoint price set — applies to every model on ' + d.subjectId + '.'
      : 'Model price set — also applies to ' + d.subjectId + ' on other endpoints that don\u2019t set all of their own prices.';
    // Prefill each category's stored rate restated per 1M tokens (what this
    // form saves); a rate over a non-decimal token block stays blank, and a
    // price set is one currency — the shared field takes the first seen.
    const row = anchor.closest('tr[data-price-row]');
    let currency = '';
    PRICE_UNITS.forEach((u) => {
      const cell = row && row.querySelector('td[data-price-cell="' + d.endpoint + '|' + u.unit + '"]');
      if (!cell || !cell.dataset.rateMicros) return;
      currency = currency || cell.dataset.currency || '';
      const price = rateToPrice(cell.dataset.rateMicros, cell.dataset.unitQuantity);
      if (!price) return;
      const input = form.querySelector('#models-price-rate-' + u.unit);
      input.setAttribute('value', price);
      const preview = form.querySelector('[data-models-price-preview="' + u.unit + '"]');
      preview.textContent = fmtPrice(price, cell.dataset.currency) + ' per 1,000,000 tokens';
    });
    form.querySelector('#models-price-currency').setAttribute('value', currency || 'USD');
    const holder = document.createElement('div');
    holder.appendChild(form);
    Enclava.openDrawer(holder.innerHTML);
  }

  async function submitPrice(form) {
    const currencyInput = form.querySelector('#models-price-currency');
    const errorSlot = form.querySelector('[data-models-price-error]');
    errorSlot.hidden = true;
    const currency = (currencyInput.value.trim() || 'USD').toUpperCase();
    const prices = [];
    for (const u of PRICE_UNITS) {
      const input = form.querySelector('#models-price-rate-' + u.unit);
      if (!input || input.value.trim() === '') continue;
      const rate = priceToRate(input.value);
      if (!rate) {
        errorSlot.textContent = u.label + ': enter a price of zero or more, e.g. 0.15 or 0.001.';
        errorSlot.hidden = false;
        return;
      }
      // The row's own price set (data-subject-*): a write anywhere else is
      // hidden by the whole-set rule and the cell keeps reading unpriced.
      prices.push({
        subject_type: form.dataset.subjectType,
        subject_id: form.dataset.subjectId,
        capability: form.dataset.capability,
        unit: u.unit,
        rate_micros: rate.rate_micros,
        unit_quantity: rate.unit_quantity,
        currency: currency,
      });
    }
    if (!prices.length) {
      errorSlot.textContent = 'Enter at least one category price — empty fields are kept as they are.';
      errorSlot.hidden = false;
      return;
    }
    try {
      // One atomic write for every entered category (P-05).
      await Enclava.api('POST', '/api/v1/admin/pricing', { prices: prices });
      Enclava.toast('success', prices.length === 1 ? 'Price saved' : prices.length + ' prices saved');
      Enclava.closeDrawer();
      Enclava.refresh();
    } catch (err) {
      errorSlot.textContent = err.message || 'Saving the prices failed';
      errorSlot.hidden = false;
      errorToast(err);
    }
  }


  function openHistory(anchor) {
    const d = anchor.dataset;
    const params = new URLSearchParams({
      endpoint: d.endpoint,
      model: d.model,
      capability: d.capability,
    });
    Enclava.openDrawer('/dashboard/partials/models/price-history?' + params.toString());
  }

  // Models tab: each control PUTs one patch to /api/v1/models/settings/{m}.
  // A refused write (e.g. fallback to itself) shows the API's own message
  // and the section refreshes so the control reverts to stored state.
  async function saveModelSetting(model, patch, verb) {
    try {
      await Enclava.api('PUT', '/api/v1/models/settings/' + encodeURIComponent(model), patch);
      Enclava.toast('success', model + ': ' + verb);
      Enclava.refresh();
    } catch (err) {
      Enclava.toast('error', (err && err.message) || 'Saving the model setting failed');
      Enclava.refresh();
    }
  }

  document.addEventListener('change', (e) => {
    if (!(e.target instanceof Element)) return;
    const el = e.target;
    const model = el.dataset && el.dataset.model;
    if (!model) return;
    if (el.matches('[data-models-available]')) {
      saveModelSetting(model, { available: el.checked }, el.checked ? 'available to people' : 'hidden from people');
    } else if (el.matches('[data-models-default]') && el.checked) {
      saveModelSetting(model, { default: true }, 'set as default');
    } else if (el.matches('[data-models-fallback]')) {
      saveModelSetting(model, { fallback_model: el.value }, el.value ? 'fallback set to ' + el.value : 'fallback cleared');
    }
  });

  document.addEventListener('click', (e) => {
    if (!(e.target instanceof Element)) return;
    const trigger = e.target.closest('[data-action="models:endpoint"],[data-action="models:price"],[data-action="models:history"],[data-action="models:clear-default"],[data-models-discover]');
    if (!trigger) return;
    switch (trigger.dataset.action) {
      case 'models:endpoint': {
        const id = trigger.dataset.endpointId || '';
        Enclava.openDrawer('/dashboard/partials/models/endpoint-drawer' + (id ? '?id=' + encodeURIComponent(id) : ''));
        break;
      }
      case 'models:price':
        openPricePopover(trigger);
        break;
      case 'models:history':
        openHistory(trigger);
        break;
      case 'models:clear-default': {
        const current = document.querySelector('[data-models-default]:checked');
        if (current && current.dataset.model) {
          saveModelSetting(current.dataset.model, { default: false }, 'default cleared');
        }
        break;
      }
    }
    if (trigger.matches('[data-models-discover]')) {
      e.preventDefault();
      discoverEndpoint(trigger.closest('[data-models-endpoint-form]'), trigger);
    }
  });


  document.addEventListener('submit', (e) => {
    if (!(e.target instanceof Element)) return;
    if (e.target.matches('[data-models-endpoint-form]')) {
      e.preventDefault();
      submitEndpoint(e.target);
    } else if (e.target.matches('[data-models-price-form]')) {
      e.preventDefault();
      submitPrice(e.target);
    }
  });

  // The confirm action arms in app.js; the fired event carries the delete.
  document.addEventListener('enclava:confirm', (e) => {
    if (e.target instanceof Element && e.target.matches('[data-models-disable]')) {
      disableEndpoint(e.target);
    }
  });

  // Live preview of each entered category price in the shared money shape.
  document.addEventListener('input', (e) => {
    if (!(e.target instanceof HTMLInputElement)) return;
    const form = e.target.closest('[data-models-price-form]');
    if (!form || (!e.target.id.startsWith('models-price-rate-') && e.target.id !== 'models-price-currency')) return;
    PRICE_UNITS.forEach((u) => {
      const input = form.querySelector('#models-price-rate-' + u.unit);
      const slot = form.querySelector('[data-models-price-preview="' + u.unit + '"]');
      const rate = input.value.trim() === '' ? null : priceToRate(input.value);
      slot.textContent = rate
        ? fmtPrice(rateToPrice(rate.rate_micros, rate.unit_quantity), form.querySelector('#models-price-currency').value) + ' per 1,000,000 tokens'
        : input.value.trim() === ''
          ? 'Leave empty to keep the existing price.'
          : 'Enter a price of zero or more, e.g. 0.15 or 0.001.';
    });
  });
})();
