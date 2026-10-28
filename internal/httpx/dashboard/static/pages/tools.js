/* tools.js — Tools page module (OA-26). Every write goes through
 * window.Enclava.api to the platform.admin /api/v1/mcp surface; fragments
 * re-render through Enclava.refresh() → htmx "refresh from:body". The
 * Approve PUT carries the row's reviewed schema_version — a 409
 * schema_changed means the durable row moved on: toast + re-render, never a
 * blind approve.
 */
(function () {
  if (!window.Enclava) return;

  function serverPath(id) {
    return '/api/v1/mcp/servers/' + encodeURIComponent(id);
  }

  function toolPath(server, tool) {
    return serverPath(server) + '/tools/' + encodeURIComponent(tool);
  }

  function checkedScopes(root, name) {
    return Array.prototype.slice.call(root.querySelectorAll('input[name="' + name + '"]'))
      .filter(function (c) { return c.checked; })
      .map(function (c) { return c.value; });
  }

  function fail(err, fallback) {
    if (!err || err.status !== 403) Enclava.toast('error', (err && err.message) || fallback);
  }

  async function approve(trigger) {
    var card = trigger.closest('[data-tool-card]');
    if (!card) return;
    var nameInput = card.querySelector('input[name="model_name"]');
    var modelName = (nameInput && nameInput.value.trim()) || trigger.dataset.remoteName || '';
    var body = {
      model_name: modelName,
      caller_scopes: checkedScopes(card, 'caller_scope'),
      // The reviewed version binds the approval: a stale value 409s.
      schema_version: parseInt(trigger.dataset.schemaVersion, 10),
    };
    try {
      await Enclava.api('PUT', toolPath(trigger.dataset.server, trigger.dataset.tool), body);
      Enclava.toast('success', 'Tool approved');
      Enclava.refresh();
    } catch (err) {
      if (err && err.status === 409) {
        Enclava.toast('error', 'Schema changed since your review — re-review');
        Enclava.refresh();
      } else {
        fail(err, 'Approval failed');
      }
    }
  }

  async function reject(trigger) {
    try {
      await Enclava.api('DELETE', toolPath(trigger.dataset.server, trigger.dataset.tool));
      Enclava.toast('success', 'Tool rejected');
      Enclava.refresh();
    } catch (err) {
      fail(err, 'Rejection failed');
    }
  }

  async function discover(trigger) {
    trigger.disabled = true;
    try {
      var out = await Enclava.api('POST', serverPath(trigger.dataset.server) + '/discovery');
      var n = out && out.tools ? out.tools.length : 0;
      Enclava.toast('success', 'Discovery finished — ' + n + (n === 1 ? ' tool' : ' tools') + ' found');
      Enclava.refresh();
    } catch (err) {
      fail(err, 'Discovery failed');
    } finally {
      trigger.disabled = false;
    }
  }

  async function saveServer(form) {
    var id = (form.dataset.server || '').trim();
    var body = {
      origin: form.elements.origin.value.trim(),
      endpoint_path: form.elements.endpoint_path.value.trim() || '/mcp',
      credential_ref: form.elements.credential_ref.value.trim(),
      authorized_private: form.elements.authorized_private.checked,
      enabled: form.elements.enabled.checked,
      audience_scopes: checkedScopes(form, 'audience_scope'),
    };
    var edit = form.dataset.mode === 'edit';
    if (edit) {
      body.id = id;
    } else {
      body.id = (form.elements.id.value || '').trim();
      id = body.id;
    }
    if (!id || !body.origin) {
      Enclava.toast('error', 'Server ID and origin are required');
      return;
    }
    if (!body.audience_scopes.length) {
      Enclava.toast('error', 'Check at least one audience scope');
      return;
    }
    try {
      if (edit) {
        await Enclava.api('PUT', serverPath(id), body);
      } else {
        await Enclava.api('POST', '/api/v1/mcp/servers', body);
      }
      Enclava.closeDrawer();
      Enclava.toast('success', edit ? 'Server updated' : 'Server registered');
      Enclava.refresh();
    } catch (err) {
      fail(err, 'Could not save the server');
    }
  }

  async function deleteServer(trigger) {
    try {
      await Enclava.api('DELETE', serverPath(trigger.dataset.server));
      Enclava.toast('success', 'Server deleted');
      Enclava.refresh();
    } catch (err) {
      fail(err, 'Delete failed');
    }
  }

  document.addEventListener('click', function (e) {
    if (!(e.target instanceof Element)) return;
    var trigger = e.target.closest('[data-action^="tools:"]');
    if (!trigger) return;
    if (trigger.dataset.action === 'tools:approve') approve(trigger);
    else if (trigger.dataset.action === 'tools:reject') reject(trigger);
    else if (trigger.dataset.action === 'tools:discover') discover(trigger);
  });

  document.addEventListener('submit', function (e) {
    var form = e.target;
    if (!(form instanceof HTMLFormElement) || form.dataset.toolsForm !== 'server') return;
    e.preventDefault();
    saveServer(form);
  });

  // The shared two-step confirm dispatches enclava:confirm on the second
  // click; this page owns every confirm trigger that carries data-server.
  document.addEventListener('enclava:confirm', function (e) {
    var t = e.target;
    if (t instanceof Element && t.dataset.action === 'confirm' && t.dataset.server) deleteServer(t);
  });
})();
