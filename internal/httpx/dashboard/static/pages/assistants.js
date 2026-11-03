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

  // Save guard: an enabled assistant with a "specific members" audience
  // and nobody picked warns inline instead of silently becoming
  // unreachable; a new assistant must get a deliberate choice. The
  // confirmed state rides the form's data attribute, never the DOM warning.
  function audienceWarn(form, text, allowAnyway) {
    const note = form.querySelector('[data-audience-warn]');
    if (!note) return;
    note.querySelector('[data-audience-warn-text]').textContent = text;
    const anyway = note.querySelector('[data-audience-save-anyway]');
    if (anyway) anyway.hidden = !allowAnyway;
    note.hidden = false;
  }

  document.addEventListener('click', (e) => {
    if (!(e.target instanceof Element)) return;
    const anyway = e.target.closest('[data-audience-save-anyway]');
    if (!anyway) return;
    const form = anyway.closest('form');
    if (!form) return;
    form.dataset.audienceConfirmed = 'yes';
    const note = form.querySelector('[data-audience-warn]');
    if (note) note.hidden = true;
    saveAssistant(form.querySelector('[data-action="assistants:save"]'));
  });

  // Editing the audience after a warning re-arms the guard: the choice has
  // to mean what the save will store.
  document.addEventListener('change', (e) => {
    if (!(e.target instanceof Element)) return;
    if (e.target.name !== 'audience-mode' && e.target.name !== 'audience-member') return;
    const form = e.target.closest('form');
    if (!form) return;
    const note = form.querySelector('[data-audience-warn]');
    if (note) note.hidden = true;
    delete form.dataset.audienceConfirmed;
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
    if (!mode) {
      audienceWarn(form, 'Choose who can use this assistant.', false);
      return;
    }
    if (mode.value === 'all') audience.push('*');
    else audience.push(...checkedValues(form, 'audience-member'));
    if (field(form, 'is_enabled').checked && mode.value !== 'all' &&
        audience.length === 0 && form.dataset.audienceConfirmed !== 'yes') {
      audienceWarn(form, 'Nobody can use this assistant yet', true);
      return;
    }
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

  // The Coverage drawer's "Fix for <role>" opens the access role editor
  // inside this page's drawer — access.js never loads on this page, so
  // this module serializes and submits the role form identically: every
  // grant section emits its canonical document and sections sharing a
  // kind (Models + Embedding models → grants.models) merge items.
  function roleGrantDoc(section) {
    const all = section.querySelector('[data-grant-all]');
    if (all && all.checked) return { mode: 'all' };
    return {
      mode: 'list',
      items: Array.from(section.querySelectorAll('[data-grant-item]:checked')).map((i) => i.value),
    };
  }

  function mergeGrantDoc(doc, incoming) {
    if (!doc || incoming.mode === 'all') return incoming;
    if (doc.mode === 'all') return doc;
    return { mode: 'list', items: Array.from(new Set(doc.items.concat(incoming.items))) };
  }

  document.addEventListener('submit', (e) => {
    const form = e.target;
    if (!(form instanceof HTMLFormElement) || form.dataset.form !== 'access-role') return;
    e.preventDefault();
    const grants = {};
    form.querySelectorAll('fieldset[data-grant]').forEach((section) => {
      const kind = section.dataset.grant;
      grants[kind] = mergeGrantDoc(grants[kind], roleGrantDoc(section));
    });
    const body = {
      name: form.elements.name.value.trim(),
      scopes: checkedValues(form, 'scope'),
      grants: grants,
    };
    const roleID = form.dataset.roleId;
    const request = roleID
      ? E.api('PATCH', '/api/v1/identities/roles/' + encodeURIComponent(roleID), body)
      : E.api('POST', '/api/v1/identities/roles', body);
    request.then(() => {
      E.toast('success', roleID ? 'Role saved' : 'Role created');
      E.closeDrawer();
      E.refresh();
    }).catch(fail);
  });

  // Toggling a section's All disables its items — the rendered state never
  // disagrees with the serialized one (mirrors access.js).
  document.addEventListener('change', (e) => {
    const input = e.target;
    if (!(input instanceof HTMLInputElement) || !input.hasAttribute('data-grant-all')) return;
    const section = input.closest('fieldset[data-grant]');
    if (!section) return;
    section.querySelectorAll('[data-grant-item]').forEach((item) => { item.disabled = input.checked; });
  });

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
      } catch (err) {
        const message = trigger.closest('.card').querySelector('[data-template-delete-error]');
        if (err && err.status === 409 && message) {
          message.textContent = err.message;
          message.hidden = false;
        } else {
          fail(err);
        }
      }
    }
  });
})();
