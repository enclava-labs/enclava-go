/*
 * assistants.js — Assistants + Instructions page module (09-03, OA-24).
 * Writes go through /api/v1 only (D-17): drawer save → POST/PUT chatbots,
 * editor save → POST/PUT prompt-templates, deletes ride the shared two-step
 * confirm (enclava:confirm) → DELETE. Fragments re-render via Enclava.refresh().
 * The drawer/editor fragments carry markup only; this module owns every
 * behavior, including the variables_schema serialization (no JSON fields).
 */
(function () {
  if (!window.Enclava) return; // app.js loads first (defer order in layout)

  const E = window.Enclava;

  function fail(err) {
    if (err && err.status === 401) return;
    const action = E.errorAction(err);
    E.toast('error', action.text, { action: action });
  }

  function page() {
    const el = document.querySelector('[data-assistants-page]');
    return el ? el.dataset.assistantsPage : '';
  }

  // Deep links: /dashboard/assistants?new=1|?id=… and the instructions
  // page's ?new=1|?id=… open the matching drawer once per load.
  const params = new URLSearchParams(location.search);
  const drawer = document.getElementById('drawer');
  if (page() === 'assistants') {
    if (params.has('new')) E.openDrawer('/dashboard/partials/assistants/drawer');
    else if (params.get('id')) E.openDrawer('/dashboard/partials/assistants/drawer?id=' + encodeURIComponent(params.get('id')));
  } else if (page() === 'instructions' && drawer) {
    if (params.has('new')) E.openDrawer('/dashboard/partials/assistants/instructions/editor?new=1');
    else if (params.get('id')) E.openDrawer('/dashboard/partials/assistants/instructions/editor?id=' + encodeURIComponent(params.get('id')));
  }

  // Instructions picker preview: swap the selected template's read-only
  // prompts into the preview body.
  document.addEventListener('change', (e) => {
    if (!(e.target instanceof Element)) return;
    const select = e.target.closest('[data-instructions-select]');
    if (!select) return;
    const drawer = document.getElementById('drawer');
    const wrap = drawer && drawer.querySelector('[data-instructions-preview]');
    const body = drawer && drawer.querySelector('[data-preview-body]');
    if (!wrap || !body) return;
    const id = select.value;
    const tpl = id && drawer.querySelector('template[data-preview-for="' + CSS.escape(id) + '"]');
    body.innerHTML = '';
    if (!tpl) {
      wrap.hidden = true;
      return;
    }
    body.appendChild(tpl.content.cloneNode(true));
    wrap.hidden = false;
  });

  document.addEventListener('click', (e) => {
    if (!(e.target instanceof Element)) return;
    const trigger = e.target.closest('[data-action^="assistants:"]');
    if (!trigger) return;
    switch (trigger.dataset.action) {
      case 'assistants:add-var': {
        const drawer = document.getElementById('drawer');
        const list = drawer && drawer.querySelector('[data-var-list]');
        const tpl = drawer && drawer.querySelector('template[data-var-template]');
        if (list && tpl) {
          list.appendChild(tpl.content.cloneNode(true));
          const input = list.querySelector('[data-var-row]:last-child [data-var-name]');
          if (input) input.focus();
        }
        break;
      }
      case 'assistants:remove-var': {
        const row = trigger.closest('[data-var-row]');
        if (row) row.remove();
        break;
      }
      case 'assistants:save':
        saveAssistant(trigger);
        break;
      case 'assistants:save-template':
        saveTemplate(trigger);
        break;
    }
  });

  function field(form, name) {
    return form.querySelector('[name="' + name + '"]');
  }

  function checkedValues(form, name) {
    return Array.from(form.querySelectorAll('input[name="' + name + '"]:checked')).map((c) => c.value);
  }

  // Preserving serializer: hidden [data-setting-key] inputs carry every
  // non-tool setting the server stored (JSON-encoded); the checklist owns
  // "tools" only — a drawer save never drops the rest.
  function collectSettings(form) {
    const settings = {};
    form.querySelectorAll('[data-setting-key]').forEach((input) => {
      try {
        settings[input.dataset.settingKey] = JSON.parse(input.value);
      } catch (_) {
        // A malformed preserved value is dropped, never guessed.
      }
    });
    settings.tools = checkedValues(form, 'tools');
    return settings;
  }

  // Labeled rows → the variables_schema shape prompts.Service stores:
  // {"type":"object","properties":{…},"required":[…]}.
  function collectVariables(form) {
    const schema = { type: 'object', properties: {} };
    const required = [];
    form.querySelectorAll('[data-var-row]').forEach((row) => {
      const nameInput = row.querySelector('[data-var-name]');
      const key = nameInput && nameInput.value.trim();
      if (!key) return; // a blank row is not a variable
      const prop = { type: 'string' };
      const desc = row.querySelector('[data-var-description]');
      if (desc && desc.value.trim()) prop.description = desc.value.trim();
      const req = row.querySelector('[data-var-required]');
      if (req && req.checked) required.push(key);
      schema.properties[key] = prop;
    });
    if (required.length) schema.required = required;
    return schema;
  }

  async function saveAssistant(trigger) {
    const form = trigger.closest('form');
    if (!form) return;
    const name = field(form, 'name');
    const model = field(form, 'model');
    if (!name.value.trim()) {
      E.toast('error', 'Name is required');
      name.focus();
      return;
    }
    if (!model.value) {
      E.toast('error', 'Choose a model');
      model.focus();
      return;
    }
    const audience = [];
    const mode = form.querySelector('input[name="audience-mode"]:checked');
    if (mode && mode.value === 'all') audience.push('*');
    else audience.push(...checkedValues(form, 'audience-member'));
    const body = {
      name: name.value.trim(),
      description: field(form, 'description').value.trim(),
      model: model.value,
      prompt_template_id: field(form, 'prompt_template_id').value,
      rag_collection_ids: checkedValues(form, 'knowledge'),
      settings: collectSettings(form),
      audience: audience,
      is_enabled: field(form, 'is_enabled').checked,
    };
    const id = form.dataset.assistantId;
    try {
      if (id) await E.api('PUT', '/api/v1/chatbots/' + encodeURIComponent(id), body);
      else await E.api('POST', '/api/v1/chatbots', body);
      E.toast('success', id ? 'Assistant saved' : 'Assistant created');
      E.closeDrawer();
      E.refresh();
    } catch (err) {
      fail(err);
    }
  }

  async function saveTemplate(trigger) {
    const form = trigger.closest('form');
    if (!form) return;
    const name = field(form, 'name');
    if (!name.value.trim()) {
      E.toast('error', 'Name is required');
      name.focus();
      return;
    }
    const body = {
      name: name.value.trim(),
      system_prompt: field(form, 'system_prompt').value,
      user_prompt: field(form, 'user_prompt').value,
      variables_schema: collectVariables(form),
      is_default: field(form, 'is_default').checked,
    };
    const id = form.dataset.templateId;
    try {
      if (id) await E.api('PUT', '/api/v1/prompt-templates/' + encodeURIComponent(id), body);
      else await E.api('POST', '/api/v1/prompt-templates', body);
      E.toast('success', id ? 'Instructions saved' : 'Instructions created');
      E.closeDrawer();
      E.refresh();
    } catch (err) {
      fail(err);
    }
  }

  // Deletes ride the shared two-step confirm: the armed trigger fires
  // enclava:confirm and this module performs the API DELETE.
  document.addEventListener('enclava:confirm', async (e) => {
    const trigger = e.target;
    if (!(trigger instanceof Element)) return;
    if (trigger.dataset.assistantDelete) {
      try {
        await E.api('DELETE', '/api/v1/chatbots/' + encodeURIComponent(trigger.dataset.assistantDelete));
        E.toast('success', 'Assistant deleted');
        E.closeDrawer();
        E.refresh();
      } catch (err) { fail(err); }
    } else if (trigger.dataset.templateDelete) {
      try {
        await E.api('DELETE', '/api/v1/prompt-templates/' + encodeURIComponent(trigger.dataset.templateDelete));
        E.toast('success', 'Instructions deleted');
        E.closeDrawer();
        E.refresh();
      } catch (err) { fail(err); }
    }
  });
})();
