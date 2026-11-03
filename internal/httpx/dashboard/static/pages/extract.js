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
  let editorOriginalContext = null;

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

  let stripTimer = null;

  function renderStrip(el, job) {
    const status = job && job.status ? job.status : 'queued';
    if (status === 'failed') {
      const act = E().errorAction({ message: job.error_message || '' });
      el.className = 'extract-strip warn';
      el.innerHTML = '';
      const span = document.createElement('span');
      span.textContent = 'Extraction failed — ' + ((act && act.text) || job.error_message || 'see the job for details');
      el.appendChild(span);
      const a = document.createElement('a');
      a.href = '/dashboard/extract?tab=jobs&job=' + encodeURIComponent(el.dataset.jobId);
      a.textContent = 'Open job';
      el.appendChild(a);
      return false;
    }
    if (status === 'succeeded') {
      el.className = 'extract-strip';
      el.innerHTML = '';
      const needs = job.validation_status === 'needs_review';
      const span = document.createElement('span');
      span.textContent = needs ? 'Needs review — ' + job.validation_error_count + ' validation error(s)' : 'Extraction ready';
      el.appendChild(span);
      const a = document.createElement('a');
      a.href = '/dashboard/extract?tab=jobs&job=' + encodeURIComponent(el.dataset.jobId);
      a.textContent = 'Open job';
      el.appendChild(a);
      return false;
    }
    if (status === 'cancelled' || status === 'unknown') {
      el.className = 'extract-strip warn';
      el.textContent = 'Extraction ' + status;
      return false;
    }
    el.className = 'extract-strip';
    el.textContent = status === 'queued' ? 'Queued — waiting for a worker' : 'Processing…';
    return true;
  }

  function pollJobStrip(jobId) {
    clearInterval(stripTimer);
    stripTimer = setInterval(async () => {
      const el = q('[data-extract-strip]');
      if (!el || el.dataset.jobId !== jobId) { clearInterval(stripTimer); stripTimer = null; return; }
      try {
        const detail = await E().api('GET', '/api/v1/extract/jobs/' + encodeURIComponent(jobId));
        if (!renderStrip(el, detail && detail.job)) { clearInterval(stripTimer); stripTimer = null; }
      } catch (err) { /* transient poll miss — keep the same 3s cadence */ }
    }, 3000);
  }

  function showQueuedBand(fileName, jobId) {
    const form = q('[data-extract-form]');
    const band = q('[data-extract-band]');
    if (!form || !band) return;
    form.hidden = true;
    band.hidden = false;
    q('[data-extract-band-file]', band).textContent = fileName;
    q('[data-extract-band-id]', band).textContent = jobId;
    q('[data-extract-band-view]', band).href = '/dashboard/extract?tab=jobs&job=' + encodeURIComponent(jobId);
    const strip = q('[data-extract-strip]', band);
    strip.hidden = false;
    strip.dataset.jobId = jobId;
    renderStrip(strip, { status: 'queued' });
    pollJobStrip(jobId);
  }

  function resetProcessForm() {
    const form = q('[data-extract-form]');
    const band = q('[data-extract-band]');
    if (stripTimer) { clearInterval(stripTimer); stripTimer = null; }
    if (band) band.hidden = true;
    if (form) form.hidden = false;
  }

  async function submitJob() {
    const Enclava = E();
    const input = q('[data-extract-file]');
    const select = q('[data-extract-template-select]');
    if (!Enclava || !input || !select) return;
    const file = input.files && input.files[0];
    if (!file) { Enclava.toast('error', 'Choose a document to extract'); return; }
    if (!select.value) { Enclava.toast('error', 'Choose an extraction template'); return; }
    // Pricing preflight: the server marks picker options whose route is not
    // fully priced — warn through the shared refusal mapping, then submit.
    const option = select.selectedOptions && select.selectedOptions[0];
    if (option && option.hasAttribute('data-unpriced')) {
      const act = Enclava.errorAction ? Enclava.errorAction({ code: 'price_missing' }) : null;
      Enclava.toast('error', 'This template’s model isn’t fully priced — the job may fail.',
        act && act.href ? { action: { label: act.label, href: act.href } } : {});
    }
    const fd = new FormData();
    fd.append('template_id', select.value);
    fd.append('file', file);
    try {
      const out = await Enclava.api('POST', '/api/v1/extract/jobs', fd);
      input.value = '';
      showFileName(null);
      showQueuedBand(file.name, out && out.job_id ? out.job_id : '');
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
    const ack = drawer && q('[data-extract-ack]', drawer);
    try {
      // result_ref is the job id — the import resolves the stored result
      // artifact server-side (never a client-controlled artifact path).
      await Enclava.api('POST', '/api/v1/knowledge/imports', {
        collection_id: select.value,
        result_ref: btn.dataset.jobId,
        acknowledge_validation: !!(ack && ack.checked),
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

  // Context definitions are objects ({type, description, …}); pre-fix
  // templates may carry bare type strings — read either shape and keep
  // the whole def so save preserves metadata the rows don't show.
  function contextRowsFrom(schema) {
    if (!schema || typeof schema !== 'object') return [];
    return Object.keys(schema).map((name) => {
      const raw = schema[name];
      const def = raw && typeof raw === 'object' && !Array.isArray(raw) ? raw : {};
      return {
        name,
        type: CONTEXT_TYPES.includes(def.type) ? def.type : (CONTEXT_TYPES.includes(raw) ? raw : 'string'),
        description: typeof def.description === 'string' ? def.description : '',
        def,
      };
    });
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

  // Context definitions serialize as objects — the flat {name: type} shape
  // the old editor wrote is rejected by template validation. The prior def
  // object is merged so metadata rows can't represent (required, …) survives.
  function buildContextSchema(original, rows) {
    const schema = {};
    for (const row of rows) {
      const prev = original && typeof original[row.name] === 'object' && original[row.name] !== null && !Array.isArray(original[row.name])
        ? original[row.name] : {};
      const def = Object.assign({}, prev);
      def.type = row.type;
      if (row.description) def.description = row.description;
      else delete def.description;
      schema[row.name] = def;
    }
    return schema;
  }

  // label wraps the control — the label text is the accessible name.
  function labeled(labelText, control) {
    const wrap = document.createElement('div');
    const lab = document.createElement('label');
    lab.className = 'label';
    lab.textContent = labelText;
    lab.appendChild(control);
    wrap.appendChild(lab);
    return wrap;
  }

  // Values go on ATTRIBUTES, not properties: the editor serializes through
  // innerHTML before openDrawer, and property-only values never survive
  // that round trip (saving an unchanged template wiped its schemas).
  function makeSchemaRow(kind, row) {
    const holder = document.createElement('div');
    holder.className = 'grid two';
    holder.dataset.schemaRow = kind;

    const name = document.createElement('input');
    name.className = 'field';
    name.dataset.rowName = '1';
    name.placeholder = kind === 'output' ? 'field_name' : 'hint_name';
    if (row && row.name) name.setAttribute('value', row.name);
    holder.appendChild(labeled('Field name', name));

    const type = document.createElement('select');
    type.className = 'field';
    type.dataset.rowType = '1';
    (kind === 'output' ? OUTPUT_TYPES : CONTEXT_TYPES).forEach((t) => {
      const opt = document.createElement('option');
      opt.value = t;
      opt.textContent = t;
      if (row && row.type === t) opt.setAttribute('selected', '');
      type.appendChild(opt);
    });
    holder.appendChild(labeled('Type', type));

    if (kind === 'output') {
      const req = document.createElement('input');
      req.type = 'checkbox';
      req.dataset.rowRequired = '1';
      if (row && row.required) req.setAttribute('checked', '');
      holder.appendChild(labeled('Required', req));
    }

    const desc = document.createElement('input');
    desc.className = 'field';
    desc.dataset.rowDescription = '1';
    desc.placeholder = kind === 'output' ? 'Shown to reviewers' : 'What this hint is for';
    if (row && row.description) desc.setAttribute('value', row.description);
    holder.appendChild(labeled('Description', desc));

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
    editorOriginalContext = (t && t.context_schema) || null;
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
    // Static template fields fill the live drawer; schema-row values already
    // serialized correctly because makeSchemaRow sets attributes.
    const drawer = document.getElementById('drawer');
    if (!drawer) return;
    setEditorField(drawer, 'name', (t && t.name) || '');
    setEditorField(drawer, 'description', (t && t.description) || '');
    setEditorField(drawer, 'system_prompt', (t && t.system_prompt) || '');
    setEditorField(drawer, 'user_prompt', (t && t.user_prompt) || '');
    setEditorField(drawer, 'model', (t && t.model) || '');
    setEditorField(drawer, 'is_active', t ? !!t.is_active : true);
    setEditorField(drawer, 'max_output_tokens', t && t.max_output_tokens ? String(t.max_output_tokens) : '');
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
    const maxTokRaw = (editorField(drawer, 'max_output_tokens') || '').trim();
    if (maxTokRaw && (!/^\d+$/.test(maxTokRaw) || +maxTokRaw < 1 || +maxTokRaw > 32768)) {
      Enclava.toast('error', 'Max output tokens must be between 1 and 32768 (or blank)');
      return;
    }
    const payload = {
      name,
      description: editorField(drawer, 'description') || '',
      system_prompt: system,
      user_prompt: user,
      model: editorField(drawer, 'model') || '',
      max_output_tokens: maxTokRaw ? parseInt(maxTokRaw, 10) : 0,
      is_active: !!editorField(drawer, 'is_active'),
      is_default: !!editorField(drawer, 'is_default'),
      context_schema: buildContextSchema(editorOriginalContext, readRows(q('[data-editor-schema="context"]', drawer), 'context')),
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
      // Schema errors are template-validation feedback — the shared mapper
      // would mislabel them as a tool-schema change; show the server's words.
      const msg = err && err.message ? err.message : '';
      if (msg.toLowerCase().indexOf('schema') !== -1 || msg.indexOf('max_output') !== -1) {
        Enclava.toast('error', msg || 'Template validation failed');
        return;
      }
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
      case 'extract:another':
        resetProcessForm();
        break;
    }
  });

  // The needs-review import button stays disabled until the acknowledgement
  // checkbox is ticked; the server re-checks the flag either way.
  document.addEventListener('change', (e) => {
    const ack = e.target instanceof Element && e.target.closest('[data-extract-ack]');
    if (!ack) return;
    const drawer = ack.closest('#drawer');
    const btn = drawer && q('[data-action="extract:import"]', drawer);
    if (btn) btn.disabled = !ack.checked;
  });

  // The band's View job deep-links mark the row; scroll it into view when
  // the jobs fragment lands (initial render and each htmx refresh).
  function scrollJobHighlight() {
    const row = q('#extract-jobs tr.job-highlight');
    if (row && row.scrollIntoView) row.scrollIntoView({ block: 'center' });
  }
  scrollJobHighlight();
  document.addEventListener('htmx:afterSwap', (e) => {
    if (e.target && e.target.id === 'extract-jobs') scrollJobHighlight();
  });

  // Shared two-step confirm (app.js arms the button, this module performs).
  document.addEventListener('enclava:confirm', async (e) => {
    const t = e.target instanceof Element ? e.target : null;
    if (!t) return;
    const Enclava = E();
    if (!Enclava) return;
    try {
      if (t.dataset.extractRetry) {
        const out = await Enclava.api('POST', '/api/v1/extract/jobs/' + encodeURIComponent(t.dataset.extractRetry) + '/retry');
        Enclava.toast('success', out && out.job_id
          ? 'Retry queued as job ' + out.job_id
          : 'Retry queued — a new job was created');
      } else if (t.dataset.extractDelete) {
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
