(function () {
  var root = document.getElementById('mcp-admin');
  if (!root) return;
  var message = document.getElementById('mcp-admin-message');
  var serverList = document.getElementById('mcp-server-list');
  var discoveryMount = document.getElementById('mcp-discovered-tools');
  var approvalsMount = document.getElementById('mcp-approved-tools');
  var selectedServer = '';
  var servers = [];

  function request(path, options) {
    options = options || {};
    options.credentials = 'same-origin';
    options.headers = options.headers || {};
    var csrf = document.cookie.match(/(?:^|;\s*)enclava_csrf=([^;]*)/);
    if (csrf) options.headers['X-CSRF-Token'] = decodeURIComponent(csrf[1]);
    return fetch(path, options).then(function (response) {
      return response.json().catch(function () { return {}; }).then(function (body) {
        if (!response.ok) throw new Error(body.error && body.error.message || body.message || 'MCP request failed (HTTP ' + response.status + ').');
        return body;
      });
    });
  }

  function setMessage(text, error) {
    message.textContent = text || '';
    message.className = 'min-h-6 text-sm ' + (error ? 'text-destructive' : 'text-green-700 dark:text-green-300');
  }

  function button(label, action, danger) {
    var el = document.createElement('button');
    el.type = 'button';
    el.textContent = label;
    el.className = 'inline-flex h-8 items-center justify-center rounded-md border px-3 text-xs font-medium ' + (danger ? 'border-red-300 text-red-700 dark:text-red-300' : '');
    el.addEventListener('click', action);
    return el;
  }

  function scopes(value) {
    return (value || '').split(',').map(function (part) { return part.trim(); }).filter(Boolean);
  }

  function serverPayload(server) {
    return {
      id: server.id,
      origin: server.origin,
      endpoint_path: server.endpoint_path || '/',
      credential_ref: server.credential_ref || '',
      authorized_private: !!server.authorized_private,
      audience_scopes: server.audience_scopes || [],
      enabled: !!server.enabled
    };
  }

  function loadApprovals(serverID) {
    request('/api/v1/mcp/servers/' + encodeURIComponent(serverID) + '/tools').then(function (body) {
      approvalsMount.replaceChildren();
      var tools = body.tools || [];
      var title = document.createElement('h3');
      title.className = 'font-semibold';
      title.textContent = 'Approved / previously approved tools for ' + serverID;
      approvalsMount.appendChild(title);
      if (!tools.length) {
        var empty = document.createElement('p');
        empty.className = 'text-sm text-muted-foreground';
        empty.textContent = 'No tools have been approved.';
        approvalsMount.appendChild(empty);
      }
      tools.forEach(function (tool) {
        var row = document.createElement('div');
        row.className = 'flex flex-col gap-2 rounded-md border p-3 sm:flex-row sm:items-center sm:justify-between';
        var details = document.createElement('div');
        details.className = 'min-w-0';
        var label = document.createElement('p');
        label.className = 'font-medium break-all';
        label.textContent = tool.model_name + ' ← ' + tool.remote_name + ' · schema v' + tool.schema_version + ' · ' + (tool.enabled ? 'enabled' : 'disabled');
        details.appendChild(label);
        var schema = document.createElement('pre');
        schema.className = 'mt-1 max-h-24 overflow-auto whitespace-pre-wrap break-all text-xs text-muted-foreground';
        schema.textContent = JSON.stringify(tool.input_schema, null, 2);
        details.appendChild(schema);
        row.appendChild(details);
        if (tool.enabled) row.appendChild(button('Revoke', function () {
          request('/api/v1/mcp/servers/' + encodeURIComponent(serverID) + '/tools/' + encodeURIComponent(tool.tool_id), { method: 'DELETE' })
            .then(function () { setMessage('Tool approval revoked.'); loadApprovals(serverID); })
            .catch(function (error) { setMessage(error.message, true); });
        }, true));
        approvalsMount.appendChild(row);
      });
    }).catch(function (error) { setMessage(error.message, true); });
  }

  function discover(serverID) {
    selectedServer = serverID;
    discoveryMount.replaceChildren();
    document.getElementById('mcp-discovery-server').textContent = 'Discovering tools from ' + serverID + '…';
    request('/api/v1/mcp/servers/' + encodeURIComponent(serverID) + '/discovery', { method: 'POST' }).then(function (body) {
      document.getElementById('mcp-discovery-server').textContent = 'Discovery snapshot for ' + serverID + '. No tool is enabled until explicitly approved.';
      (body.tools || []).forEach(function (tool) {
        var panel = document.createElement('div');
        panel.className = 'rounded-md border p-4';
        var name = document.createElement('h3');
        name.className = 'font-medium break-all';
        name.textContent = tool.name;
        panel.appendChild(name);
        var schema = document.createElement('pre');
        schema.className = 'mt-2 max-h-40 overflow-auto whitespace-pre-wrap break-all text-xs text-muted-foreground';
        schema.textContent = JSON.stringify({ input: tool.input_schema, output: tool.output_schema }, null, 2);
        panel.appendChild(schema);
        var form = document.createElement('form');
        form.className = 'mt-3 grid gap-2 sm:grid-cols-3';
        var model = document.createElement('input');
        model.className = 'rounded-md border bg-background px-3 py-2 text-sm';
        model.setAttribute('aria-label', 'Assistant-visible model name');
        model.placeholder = 'Assistant-visible model name';
        model.value = tool.name.replace(/[^A-Za-z0-9_.-]/g, '_').slice(0, 64);
        model.required = true;
        var audience = document.createElement('input');
        audience.className = 'rounded-md border bg-background px-3 py-2 text-sm';
        audience.setAttribute('aria-label', 'Caller scopes');
        audience.placeholder = 'Caller scopes (comma separated)';
        audience.value = 'mcp.use';
        audience.required = true;
        var approve = document.createElement('button');
        approve.className = 'inline-flex h-10 items-center justify-center rounded-md bg-primary px-4 text-sm font-medium text-primary-foreground';
        approve.textContent = 'Approve / reapprove';
        form.appendChild(model);
        form.appendChild(audience);
        form.appendChild(approve);
        form.addEventListener('submit', function (event) {
          event.preventDefault();
          request('/api/v1/mcp/servers/' + encodeURIComponent(serverID) + '/tools/' + encodeURIComponent(tool.name), {
            method: 'PUT', headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ model_name: model.value.trim(), caller_scopes: scopes(audience.value) })
          }).then(function () { setMessage('Tool schema approved.'); loadApprovals(serverID); })
            .catch(function (error) { setMessage(error.message, true); });
        });
        panel.appendChild(form);
        discoveryMount.appendChild(panel);
      });
      loadApprovals(serverID);
    }).catch(function (error) {
      document.getElementById('mcp-discovery-server').textContent = 'Discovery failed.';
      setMessage(error.message, true);
    });
  }

  function loadServers() {
    request('/api/v1/mcp/servers').then(function (body) {
      servers = body.servers || [];
      serverList.replaceChildren();
      if (!servers.length) {
        var empty = document.createElement('tr');
        var cell = document.createElement('td');
        cell.colSpan = 5;
        cell.className = 'px-4 py-4 text-muted-foreground';
        cell.textContent = 'No MCP servers registered.';
        empty.appendChild(cell);
        serverList.appendChild(empty);
        return;
      }
      servers.forEach(function (server) {
        var row = document.createElement('tr');
        [server.id, server.origin + (server.endpoint_path || '/'), (server.audience_scopes || []).join(', '), server.enabled ? 'Enabled' : 'Disabled'].forEach(function (value) {
          var cell = document.createElement('td');
          cell.className = 'px-4 py-3 align-top break-all';
          cell.textContent = value;
          row.appendChild(cell);
        });
        var actions = document.createElement('td');
        actions.className = 'px-4 py-3 align-top';
        var wrap = document.createElement('div');
        wrap.className = 'flex flex-wrap gap-2';
        wrap.appendChild(button('Discover', function () { discover(server.id); }));
        wrap.appendChild(button(server.enabled ? 'Disable' : 'Enable', function () {
          var payload = serverPayload(server);
          payload.enabled = !server.enabled;
          request('/api/v1/mcp/servers/' + encodeURIComponent(server.id), { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload) })
            .then(function () { setMessage('Server updated.'); loadServers(); })
            .catch(function (error) { setMessage(error.message, true); });
        }));
        wrap.appendChild(button('Delete', function () {
          if (!window.confirm('Delete this MCP server and its approvals?')) return;
          request('/api/v1/mcp/servers/' + encodeURIComponent(server.id), { method: 'DELETE' })
            .then(function () { setMessage('Server deleted.'); if (selectedServer === server.id) { selectedServer = ''; discoveryMount.replaceChildren(); approvalsMount.replaceChildren(); } loadServers(); })
            .catch(function (error) { setMessage(error.message, true); });
        }, true));
        actions.appendChild(wrap);
        row.appendChild(actions);
        serverList.appendChild(row);
      });
    }).catch(function (error) { setMessage(error.message, true); });
  }

  document.getElementById('mcp-server-form').addEventListener('submit', function (event) {
    event.preventDefault();
    var form = event.currentTarget;
    var data = new FormData(form);
    request('/api/v1/mcp/servers', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        id: String(data.get('id') || '').trim(), origin: String(data.get('origin') || '').trim(),
        endpoint_path: String(data.get('endpoint_path') || '/').trim(), credential_ref: String(data.get('credential_ref') || '').trim(),
        authorized_private: form.elements.authorized_private.checked, audience_scopes: scopes(data.get('audience_scopes')), enabled: true
      })
    }).then(function () { form.reset(); form.elements.endpoint_path.value = '/mcp'; setMessage('MCP server registered. Discover and approve tools explicitly.'); loadServers(); })
      .catch(function (error) { setMessage(error.message, true); });
  });

  loadServers();
})();
