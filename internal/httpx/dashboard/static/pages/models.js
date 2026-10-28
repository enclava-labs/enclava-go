/* models.js — Models + Pricing page module (OA-27, P3.6).
 *
 * Namespaced data-actions (page-local; shared actions stay in app.js):
 *   models:endpoint  open the add/edit endpoint drawer (data-endpoint-id)
 *   models:price     open the Set price popover bound to the cell's unit
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

  // Mirror of the server money formatter for the popover preview: integer
  // micros → "$1,240.50" (symbol prefix for the usual codes, suffix else).
  function fmtMicros(micros, currency) {
    const cur = (currency || 'USD').toUpperCase();
    const sign = micros < 0 ? '-' : '';
    const abs = Math.abs(Number(micros) || 0);
    const whole = Math.floor(abs / 1e6).toString().replace(/\B(?=(\d{3})+(?!\d))/g, ',');
    const cents = Math.floor((abs % 1e6) / 1e4).toString().padStart(2, '0');
    const sym = SYMBOLS[cur];
    return sym ? `${sign}${sym}${whole}.${cents}` : `${sign}${whole}.${cents} ${cur}`;
  }

  function closePopoverFromOutside() {
    // Enclava exposes no closePopover; an outside click is the documented
    // dismissal path, so simulate one.
    document.body.dispatchEvent(new MouseEvent('click', { bubbles: true }));
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
    const capabilities = Array.prototype.map.call(
      form.querySelectorAll('input[name="capability"]:checked'),
      (input) => input.value,
    ).concat(aliases.bindings);
    const body = {
      protocol: form.elements.protocol.value,
      sdk_provider: form.elements.sdk_provider.value.trim(),
      capabilities: capabilities,
      base_origin: form.elements.base_origin.value.trim(),
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

  function openPricePopover(anchor) {
    const d = anchor.dataset;
    const form = document.createElement('form');
    form.setAttribute('data-models-price-form', '');
    form.dataset.endpoint = d.endpoint;
    form.dataset.model = d.model;
    form.dataset.capability = d.capability;
    form.dataset.unit = d.unit;
    form.innerHTML =
      '<b>Set ' + d.unitLabel + ' price</b>' +
      '<p class="help">' + d.model + ' @ ' + d.endpoint + ' · ' + d.capability + '</p>' +
      '<label class="label" for="models-price-rate">Rate — millionths of the currency unit, per 1M tokens</label>' +
      '<input class="field" id="models-price-rate" name="rate_micros" type="number" min="0" step="1" required>' +
      '<span class="help" data-models-price-preview>Enter a rate to preview it.</span>' +
      '<label class="label" for="models-price-currency">Currency</label>' +
      '<input class="field" id="models-price-currency" name="currency" value="USD" maxlength="3">' +
      '<p class="field-error" data-models-price-error hidden></p>' +
      '<div class="row"><button type="submit" class="btn primary">Save price</button></div>';
    const holder = document.createElement('div');
    holder.appendChild(form);
    Enclava.popover(anchor, holder.innerHTML);
  }

  async function submitPrice(form) {
    const rateInput = form.querySelector('#models-price-rate');
    const currencyInput = form.querySelector('#models-price-currency');
    const errorSlot = form.querySelector('[data-models-price-error]');
    errorSlot.hidden = true;
    const rate = Math.trunc(Number(rateInput.value));
    if (!rateInput.value.trim() || Number.isNaN(rate) || rate < 0) {
      errorSlot.textContent = 'Rate must be a whole number of millionths, zero or greater.';
      errorSlot.hidden = false;
      return;
    }
    try {
      await Enclava.api('POST', '/api/v1/admin/pricing', {
        subject_type: 'endpoint',
        subject_id: form.dataset.endpoint,
        capability: form.dataset.capability,
        unit: form.dataset.unit,
        rate_micros: rate,
        unit_quantity: 1000000,
        currency: (currencyInput.value.trim() || 'USD').toUpperCase(),
      });
      Enclava.toast('success', 'Price saved');
      closePopoverFromOutside();
      Enclava.refresh();
    } catch (err) {
      errorSlot.textContent = err.message || 'Saving the price failed';
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

  document.addEventListener('click', (e) => {
    if (!(e.target instanceof Element)) return;
    const trigger = e.target.closest('[data-action="models:endpoint"],[data-action="models:price"],[data-action="models:history"]');
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

  // Live preview of the popover rate through the shared money shape.
  document.addEventListener('input', (e) => {
    if (!(e.target instanceof Element) || e.target.id !== 'models-price-rate') return;
    const form = e.target.closest('[data-models-price-form]');
    const slot = form && form.querySelector('[data-models-price-preview]');
    if (!slot) return;
    const micros = Math.trunc(Number(e.target.value));
    slot.textContent = Number.isNaN(micros) || e.target.value.trim() === ''
      ? 'Enter a rate to preview it.'
      : fmtMicros(micros, form.querySelector('#models-price-currency').value) + ' per 1,000,000 tokens';
  });
})();
