/* extract.js — the Extract Work page module (OA-25, plan 09-04).
 *
 * All writes go through window.Enclava.api to the /api/v1 extract and
 * knowledge-import APIs (D-17); fragments re-render via Enclava.refresh().
 * Delegated data-action handling for the extract:* namespace; destructive
 * controls run through the shared data-confirm two-step (enclava:confirm).
 * The template editor uses labeled schema rows only — never a JSON textarea
 * (OA-34); schema keys the labeled rows cannot represent are carried through
 * verbatim on save (preserve-unsupported rule).
 */
(function () {
  'use strict';

  const OUTPUT_TYPES = ['string', 'number', 'boolean', 'object', 'array'];
  const CONTEXT_TYPES = ['string', 'number', 'boolean'];

  let editorTemplateId = '';
  let editorOriginalSchema = null;

  const E = () => window.Enclava;
  const q = (sel, root) => (root || document).querySelector(sel);

  // Shared error→action plumbing (OA-35): every api failure renders the
  // capability-filtered next step; a retry:true outcome replays the failed
  // call via the toast's built-in Try-again (retryFn rides onRetry).
  function failAction(err, retryFn) {
    const Enclava = E();
    const act = Enclava && Enclava.errorAction ? Enclava.errorAction(err) : null;
    const msg = (act && act.text) || (err && err.message) || 'Something went wrong';
    const opts = {};
    if (act && (act.href || act.retry)) {
      if (act.retry && typeof retryFn === 'function') act.onRetry = retryFn;
      opts.action = act;
    }
    Enclava.toast('error', msg, opts);
  }

  // ── Process tab: real file input + drop zone + submit ──────────────────

  function showFileName(file) {
    const el = q('[data-extract-filename]');
    if (!el) return;
    if (file) {
      el.textContent = file.name + ' · ' + Math.max(1, Math.round(file.size / 1024)) + ' KB';
      el.hidden = false;
    } else {
      el.hidden = true;
      el.textContent = '';
    }
  }

  const fileInput = q('[data-extract-file]');
  if (fileInput) fileInput.addEventListener('change', () => showFileName(fileInput.files && fileInput.files[0]));

  const dropzone = q('[data-extract-dropzone]');
  if (dropzone) {
    ['dragenter', 'dragover'].forEach((t) => dropzone.addEventListener(t, (e) => e.preventDefault()));
    dropzone.addEventListener('drop', (e) => {
      e.preventDefault();
      const input = q('[data-extract-file]');
      if (input && e.dataTransfer && e.dataTransfer.files && e.dataTransfer.files.length) {
        input.files = e.dataTransfer.files;
        showFileName(input.files[0]);
      }
    });
  }

  async function submitJob() {
    const Enclava = E();
    const input = q('[data-extract-file]');
    const select = q('[data-extract-template-select]');
    if (!Enclava || !input || !select) return;
    const file = input.files && input.files[0];
    if (!file) { Enclava.toast('error', 'Choose a document to extract'); return; }
    if (!select.value) { Enclava.toast('error', 'Choose an extraction template'); return; }
    const fd = new FormData();
    fd.append('template_id', select.value);
    fd.append('file', file);
    try {
      await Enclava.api('POST', '/api/v1/extract/jobs', fd);
      Enclava.toast('success', 'Extraction queued');
      input.value = '';
      showFileName(null);
      Enclava.refresh();
    } catch (err) {
      failAction(err, submitJob); // price/file-size/template-denied → mapped next step
    }
  }

  async function cancelJob(btn) {
    const Enclava = E();
    try {
      await Enclava.api('POST', '/api/v1/extract/jobs/' + encodeURIComponent(btn.dataset.jobId) + '/cancel');
      Enclava.toast('info', 'Cancellation requested');
      Enclava.refresh();
    } catch (err) {
      failAction(err, () => cancelJob(btn));
    }
  }

  // ── Result drawer actions ──────────────────────────────────────────────

  function fetchResult(jobId) {
    return E().api('GET', '/api/v1/extract/jobs/' + encodeURIComponent(jobId) + '/result');
  }

  function formatValue(v) {
    if (v === null || v === undefined) return '';
    if (typeof v === 'object') return JSON.stringify(v);
    return String(v);
  }

  function csvCell(v) {
    const s = v === null || v === undefined ? '' : String(v);
    return /[",\n]/.test(s) ? '"' + s.replace(/"/g, '""') + '"' : s;
  }

  async function copyJSON(btn) {
    const Enclava = E();
    try {
      const result = await fetchResult(btn.dataset.jobId);
      await Enclava.copy(JSON.stringify(result, null, 2));
    } catch (err) {
      failAction(err);
    }
  }

  async function downloadCSV(btn) {
    const Enclava = E();
    try {
      const result = await fetchResult(btn.dataset.jobId);
      const data = (result && result.data) || {};
      const lines = ['field,value'];
      for (const k of Object.keys(data)) lines.push(csvCell(k) + ',' + csvCell(formatValue(data[k])));
      const blob = new Blob([lines.join('\n')], { type: 'text/csv' });
      const a = document.createElement('a');
      a.href = URL.createObjectURL(blob);
      a.download = ((btn.dataset.fileName || 'extraction').replace(/\.[^.]*$/, '') || 'extraction') + '-result.csv';
      document.body.appendChild(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(a.href), 1000);
      Enclava.toast('success', 'CSV downloaded');
    } catch (err) {
      failAction(err);
    }
  }

  async function importToKnowledge(btn) {
    const Enclava = E();
    const drawer = btn.closest('#drawer');
    const select = drawer && q('#extract-import-collection', drawer);
    if (!select || !select.value) { Enclava.toast('error', 'Choose a collection'); return; }
    try {
      // result_ref is the job id — the import resolves the stored result
      // artifact server-side (never a client-controlled artifact path).
      await Enclava.api('POST', '/api/v1/knowledge/imports', {
        collection_id: select.value,
        result_ref: btn.dataset.jobId,
      });
      Enclava.toast('success', 'Added to knowledge');
      Enclava.closeDrawer();
    } catch (err) {
      failAction(err); // forbidden/not-found → explanatory next step
    }
  }

  async function saveSettings(btn) {
    const Enclava = E();
    const select = q('#extract-default-model');
    if (!select) return;
    const max = parseInt(btn.dataset.maxMb || '0', 10);
    try {
      await Enclava.api('PUT', '/api/v1/extract/settings', {
        default_model: select.value,
        max_file_size_mb: max > 0 ? max : undefined,
      });
      Enclava.toast('success', 'Default model saved');
      Enclava.refresh();
    } catch (err) {
      failAction(err, () => saveSettings(btn));
    }
  }

  // ── Template editor: labeled schema rows + preserve-unsupported ────────

  function schemaRowsFrom(schema) {
    if (!schema || typeof schema !== 'object') return [];
    const props = schema.properties && typeof schema.properties === 'object' ? schema.properties : {};
    const required = Array.isArray(schema.required) ? schema.required : [];
    return Object.keys(props).map((name) => {
      const p = props[name] && typeof props[name] === 'object' && !Array.isArray(props[name]) ? props[name] : {};
      return {
        name,
        type: OUTPUT_TYPES.includes(p.type) ? p.type : 'string',
        required: required.includes(name),
        description: typeof p.description === 'string' ? p.description : '',
      };
    });
  }

  function contextRowsFrom(schema) {
    if (!schema || typeof schema !== 'object') return [];
    return Object.keys(schema).map((name) => ({
      name,
      type: CONTEXT_TYPES.includes(schema[name]) ? schema[name] : 'string',
    }));
  }

  // Constraints the labeled editor cannot represent: every root key outside
  // properties/required ($defs, additionalProperties, …) and every per-field
  // key outside type/description (pattern, enum, items, format, …).
  function unsupportedCount(schema) {
    if (!schema || typeof schema !== 'object') return 0;
    let n = 0;
    for (const k of Object.keys(schema)) {
      if (k !== 'properties' && k !== 'required' && k !== '$schema') n++;
    }
    const props = schema.properties && typeof schema.properties === 'object' ? schema.properties : {};
    for (const key of Object.keys(props)) {
      const p = props[key];
      if (!p || typeof p !== 'object' || Array.isArray(p)) { n++; continue; }
      for (const k of Object.keys(p)) {
        if (k !== 'type' && k !== 'description') n++;
      }
    }
    return n;
  }

  // Edited rows merge ONTO the untouched constraint map — unrecognized keys
  // survive a save round-trip unchanged, never silently dropped.
  function buildOutputSchema(original, rows) {
    const schema = Object.assign({}, original || {});
    const props = {};
    for (const row of rows) {
      const prev = original && original.properties && original.properties[row.name];
      const prop = Object.assign({}, prev && typeof prev === 'object' && !Array.isArray(prev) ? prev : {});
      prop.type = row.type;
      if (row.description) prop.description = row.description;
      else delete prop.description;
      props[row.name] = prop;
    }
    schema.properties = props;
    const required = rows.filter((r) => r.required).map((r) => r.name);
    if (required.length) schema.required = required;
    else delete schema.required;
    return schema;
  }

  function buildContextSchema(rows) {
    const schema = {};
    for (const row of rows) schema[row.name] = row.type;
    return schema;
  }

  function labeled(labelText, control) {
    const wrap = document.createElement('div');
    const lab = document.createElement('label');
    lab.className = 'label';
    lab.textContent = labelText;
    wrap.appendChild(lab);
    wrap.appendChild(control);
    return wrap;
  }

  function makeSchemaRow(kind, row) {
    const holder = document.createElement('div');
    holder.className = 'grid two';
    holder.dataset.schemaRow = kind;

    const name = document.createElement('input');
    name.className = 'field';
    name.dataset.rowName = '1';
    name.placeholder = kind === 'output' ? 'field_name' : 'hint_name';
    if (row && row.name) name.value = row.name;
    holder.appendChild(labeled('Field name', name));

    const type = document.createElement('select');
    type.className = 'field';
    type.dataset.rowType = '1';
    (kind === 'output' ? OUTPUT_TYPES : CONTEXT_TYPES).forEach((t) => {
      const opt = document.createElement('option');
      opt.value = t;
      opt.textContent = t;
      type.appendChild(opt);
    });
    if (row && row.type) type.value = row.type;
    holder.appendChild(labeled('Type', type));

    if (kind === 'output') {
      const req = document.createElement('input');
      req.type = 'checkbox';
      req.dataset.rowRequired = '1';
      if (row && row.required) req.checked = true;
      holder.appendChild(labeled('Required', req));

      const desc = document.createElement('input');
      desc.className = 'field';
      desc.dataset.rowDescription = '1';
      desc.placeholder = 'Shown to reviewers';
      if (row && row.description) desc.value = row.description;
      holder.appendChild(labeled('Description', desc));
    }

    const rm = document.createElement('button');
    rm.type = 'button';
    rm.className = 'btn danger';
    rm.textContent = 'Remove';
    rm.dataset.rowRemove = '1';
    holder.appendChild(rm);
    return holder;
  }

  function setEditorField(root, name, value) {
    const f = root.querySelector('[data-editor-field="' + name + '"]');
    if (!f) return;
    if (f.type === 'checkbox') f.checked = !!value;
    else f.value = value;
  }

  function editorField(root, name) {
    const f = root.querySelector('[data-editor-field="' + name + '"]');
    if (!f) return undefined;
    return f.type === 'checkbox' ? f.checked : f.value;
  }

  function readRows(container, kind) {
    const rows = [];
    if (!container) return rows;
    container.querySelectorAll('[data-schema-row="' + kind + '"]').forEach((row) => {
      const nameEl = row.querySelector('[data-row-name]');
      const typeEl = row.querySelector('[data-row-type]');
      const name = nameEl ? nameEl.value.trim() : '';
      if (!name) return;
      const r = { name, type: typeEl ? typeEl.value : 'string' };
      const reqEl = row.querySelector('[data-row-required]');
      if (reqEl) r.required = reqEl.checked;
      const descEl = row.querySelector('[data-row-description]');
      if (descEl) r.description = descEl.value.trim();
      rows.push(r);
    });
    return rows;
  }

  function newTemplateId(name) {
    let slug = name.toLowerCase().replace(/[^a-z0-9]+/g, '_').replace(/^_+|_+$/g, '');
    if (!slug) slug = 'template';
    const used = new Set();
    document.querySelectorAll('[data-template-id]').forEach((el) => used.add(el.getAttribute('data-template-id')));
    let id = slug;
    let n = 2;
    while (used.has(id)) { id = slug + '_' + n; n++; }
    return id;
  }

  async function openTemplateEditor(templateId) {
    const Enclava = E();
    const tpl = document.getElementById('extract-template-editor');
    if (!Enclava || !tpl || !tpl.content) return;

    let t = null;
    if (templateId) {
      try {
        t = await Enclava.api('GET', '/api/v1/extract/templates/' + encodeURIComponent(templateId));
      } catch (err) {
        failAction(err);
        return;
      }
    }
    editorTemplateId = templateId || '';
    editorOriginalSchema = (t && t.output_schema) || null;

    const holder = document.createElement('div');
    holder.appendChild(tpl.content.cloneNode(true));
    q('[data-editor-title]', holder).textContent = templateId ? 'Edit template' : 'New template';

    const preserved = unsupportedCount(editorOriginalSchema);
    const note = q('[data-editor-note]', holder);
    if (preserved > 0) {
      note.textContent = preserved + ' schema constraint' + (preserved === 1 ? '' : 's') +
        ' kept unchanged — the labeled rows edit names, types, required flags and descriptions only.';
      note.hidden = false;
    }

    const outBox = q('[data-editor-schema="output"]', holder);
    outBox.innerHTML = '';
    const rows = schemaRowsFrom(editorOriginalSchema);
    rows.forEach((r) => outBox.appendChild(makeSchemaRow('output', r)));
    if (!rows.length) outBox.appendChild(makeSchemaRow('output', null));
    const ctxBox = q('[data-editor-schema="context"]', holder);
    ctxBox.innerHTML = '';
    const ctxRows = contextRowsFrom(t && t.context_schema);
    ctxRows.forEach((r) => ctxBox.appendChild(makeSchemaRow('context', r)));
    if (!ctxRows.length) ctxBox.appendChild(makeSchemaRow('context', null));

    await Enclava.openDrawer(holder.innerHTML);
    // Values don't serialize with innerHTML — fill the live drawer instead.
    const drawer = document.getElementById('drawer');
    if (!drawer) return;
    setEditorField(drawer, 'name', (t && t.name) || '');
    setEditorField(drawer, 'description', (t && t.description) || '');
    setEditorField(drawer, 'system_prompt', (t && t.system_prompt) || '');
    setEditorField(drawer, 'user_prompt', (t && t.user_prompt) || '');
    setEditorField(drawer, 'model', (t && t.model) || '');
    setEditorField(drawer, 'is_active', t ? !!t.is_active : true);
    setEditorField(drawer, 'is_default', t ? !!t.is_default : false);
  }

  async function saveTemplate() {
    const Enclava = E();
    const drawer = document.getElementById('drawer');
    if (!Enclava || !drawer) return;
    const name = (editorField(drawer, 'name') || '').trim();
    const system = (editorField(drawer, 'system_prompt') || '').trim();
    const user = (editorField(drawer, 'user_prompt') || '').trim();
    if (!name || !system || !user) {
      Enclava.toast('error', 'Name and both prompts are required');
      return;
    }
    const payload = {
      name,
      description: editorField(drawer, 'description') || '',
      system_prompt: system,
      user_prompt: user,
      model: editorField(drawer, 'model') || '',
      is_active: !!editorField(drawer, 'is_active'),
      is_default: !!editorField(drawer, 'is_default'),
      context_schema: buildContextSchema(readRows(q('[data-editor-schema="context"]', drawer), 'context')),
      output_schema: buildOutputSchema(editorOriginalSchema, readRows(q('[data-editor-schema="output"]', drawer), 'output')),
    };
    try {
      if (editorTemplateId) {
        await Enclava.api('PUT', '/api/v1/extract/templates/' + encodeURIComponent(editorTemplateId), payload);
        Enclava.toast('success', 'Template saved');
      } else {
        payload.id = newTemplateId(name);
        await Enclava.api('POST', '/api/v1/extract/templates', payload);
        Enclava.toast('success', 'Template created');
      }
      Enclava.closeDrawer();
      Enclava.refresh();
    } catch (err) {
      failAction(err, saveTemplate);
    }
  }

  // ── Delegated dispatch (extract:* actions + editor + confirms) ─────────

  document.addEventListener('click', (e) => {
    if (!(e.target instanceof Element)) return;
    const Enclava = E();
    if (!Enclava) return;

    const add = e.target.closest('[data-editor-add-row]');
    if (add) {
      const drawer = add.closest('#drawer');
      const box = drawer && q('[data-editor-schema="' + add.dataset.editorAddRow + '"]', drawer);
      if (box) box.appendChild(makeSchemaRow(add.dataset.editorAddRow, null));
      return;
    }
    const rm = e.target.closest('[data-row-remove]');
    if (rm) {
      const row = rm.closest('[data-schema-row]');
      if (row) row.remove();
      return;
    }
    if (e.target.closest('[data-editor-save]')) { saveTemplate(); return; }

    const trigger = e.target.closest('[data-action^="extract:"]');
    if (!trigger) return;
    switch (trigger.dataset.action) {
      case 'extract:submit':
        submitJob();
        break;
      case 'extract:result':
        Enclava.openDrawer('/dashboard/partials/extract/result?job=' + encodeURIComponent(trigger.dataset.jobId));
        break;
      case 'extract:cancel':
        cancelJob(trigger);
        break;
      case 'extract:copy':
        copyJSON(trigger);
        break;
      case 'extract:csv':
        downloadCSV(trigger);
        break;
      case 'extract:import':
        importToKnowledge(trigger);
        break;
      case 'extract:save-settings':
        saveSettings(trigger);
        break;
      case 'extract:edit-template':
        openTemplateEditor(trigger.dataset.templateId || '');
        break;
    }
  });

  // Shared two-step confirm (app.js arms the button, this module performs).
  document.addEventListener('enclava:confirm', async (e) => {
    const t = e.target instanceof Element ? e.target : null;
    if (!t) return;
    const Enclava = E();
    if (!Enclava) return;
    try {
      if (t.dataset.extractDelete) {
        const out = await Enclava.api('DELETE', '/api/v1/extract/jobs/' + encodeURIComponent(t.dataset.extractDelete));
        Enclava.toast('success', out && out.status === 'cancellation_requested'
          ? 'Cancellation requested — the job is removed once it stops'
          : 'Extraction job deleted');
      } else if (t.dataset.extractTemplateDelete) {
        await Enclava.api('DELETE', '/api/v1/extract/templates/' + encodeURIComponent(t.dataset.extractTemplateDelete));
        Enclava.toast('success', 'Template deleted');
      } else if (t.hasAttribute('data-extract-reset-defaults')) {
        await Enclava.api('POST', '/api/v1/extract/templates/reset-defaults');
        Enclava.toast('success', 'Default templates restored');
      } else {
        return;
      }
      Enclava.refresh();
    } catch (err) {
      failAction(err);
    }
  });
})();
