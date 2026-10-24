(function () {
  var select = document.getElementById('agent-select');
  if (!select) return;
  var messages = document.getElementById('agent-messages');
  var status = document.getElementById('agent-status');
  var form = document.getElementById('agent-run-form');
  var cancel = document.getElementById('agent-cancel');
  var send = document.getElementById('agent-send');
  var conversation = document.getElementById('conversation-id');
  var activeRun = '';
  var pollTimer = 0;

  function request(path, options) {
    options = options || {};
    options.credentials = 'same-origin';
    options.cache = 'no-store';
    options.headers = options.headers || {};
    var csrf = document.cookie.match(/(?:^|;\\s*)enclava_csrf=([^;]*)/);
    if (csrf) options.headers['X-CSRF-Token'] = decodeURIComponent(csrf[1]);
    return fetch(path, options).then(function (res) {
      return res.json().catch(function () { return {}; }).then(function (body) {
        if (!res.ok) throw new Error(body.error && body.error.message || body.message || ('Request failed (' + res.status + ')'));
        return body;
      });
    });
  }
  function addMessage(role, text, refs) {
    if (messages.firstElementChild && messages.firstElementChild.textContent.indexOf('Choose an assistant') === 0) messages.textContent = '';
    var article = document.createElement('article');
    article.className = 'rounded-md bg-muted/50 p-3';
    var title = document.createElement('h3'); title.className = 'text-xs font-semibold uppercase'; title.textContent = role;
    var body = document.createElement('p'); body.className = 'mt-1 whitespace-pre-wrap break-words text-sm'; body.textContent = text;
    article.append(title, body);
    (refs || []).forEach(function (ref) {
      var link = document.createElement('a');
      link.className = 'mt-2 mr-2 inline-block text-xs underline';
      link.href = '/api/v1/rag/evidence/' + encodeURIComponent(ref);
      link.textContent = 'Evidence ' + ref;
      link.target = '_blank'; link.rel = 'noopener';
      article.appendChild(link);
    });
    messages.appendChild(article); messages.scrollTop = messages.scrollHeight;
  }
  function loadMessages(id) {
    if (!id) return Promise.resolve();
    return request('/api/v1/conversations/' + encodeURIComponent(id)).then(function (view) {
      messages.textContent = '';
      (view.messages || []).forEach(function (m) { addMessage(m.role, m.content, m.evidence_ids); });
      if (!view.messages || !view.messages.length) messages.textContent = 'No messages in this conversation.';
    });
  }
  function updateRun(id) {
    request('/api/v1/agent-runs/' + encodeURIComponent(id)).then(function (run) {
      status.textContent = 'Run ' + run.status + (run.job_id ? ' · Job ' + run.job_id : '');
      if (run.status === 'succeeded' || run.status === 'failed' || run.status === 'cancelled' || run.status === 'unknown') {
        window.clearTimeout(pollTimer); activeRun = ''; cancel.disabled = true; send.disabled = false;
        if (run.output) addMessage('assistant', run.output, run.evidence_ids);
        if (run.error) status.textContent += ' — ' + run.error;
        conversation.value = run.conversation_id || conversation.value;
        return loadMessages(conversation.value).catch(function (err) { status.textContent += ' · Could not refresh conversation: ' + err.message; });
      }
      pollTimer = window.setTimeout(function () { updateRun(id); }, 1200);
    }).catch(function (err) { status.textContent = 'Could not retrieve run status: ' + err.message; window.clearTimeout(pollTimer); activeRun = ''; cancel.disabled = true; send.disabled = false; });
  }
  request('/api/v1/agents').then(function (data) {
    var bots = data.chatbots || [];
    select.textContent = '';
    bots.forEach(function (bot) {
      var option = document.createElement('option'); option.value = bot.id; option.textContent = bot.name + (bot.is_enabled ? '' : ' (disabled)'); option.disabled = !bot.is_enabled; select.appendChild(option);
    });
    if (!bots.length) { var empty = document.createElement('option'); empty.value = ''; empty.textContent = 'Create an enabled assistant above first'; select.appendChild(empty); }
    var params = new URLSearchParams(window.location.search);
    if (params.get('conversation')) { conversation.value = params.get('conversation'); loadMessages(conversation.value).catch(function (err) { status.textContent = err.message; }); }
  }).catch(function (err) { select.innerHTML = '<option value="">Assistant list unavailable</option>'; status.textContent = err.message; });

  form.addEventListener('submit', function (event) {
    event.preventDefault();
    var input = document.getElementById('agent-input').value.trim();
    if (!select.value || !input || activeRun) return;
    send.disabled = true; status.textContent = 'Submitting run…';
    request('/api/v1/agent-runs', { method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({assistant_id: select.value, conversation_id: conversation.value.trim(), input: input}) }).then(function (run) {
      document.getElementById('agent-input').value = '';
      conversation.value = run.conversation_id || conversation.value;
      addMessage('user', input);
      activeRun = run.id; cancel.disabled = false; status.textContent = 'Run ' + run.status + (run.job_id ? ' · Job ' + run.job_id : '');
      updateRun(run.id);
    }).catch(function (err) { send.disabled = false; status.textContent = 'Run could not start: ' + err.message; });
  });
  cancel.addEventListener('click', function () {
    if (!activeRun) return;
    cancel.disabled = true; status.textContent = 'Cancellation requested…';
    request('/api/v1/agent-runs/' + encodeURIComponent(activeRun), {method:'DELETE'}).then(function () { updateRun(activeRun); }).catch(function (err) { cancel.disabled = false; status.textContent = 'Cancellation failed: ' + err.message; });
  });
  conversation.addEventListener('change', function () { loadMessages(conversation.value.trim()).catch(function (err) { status.textContent = 'Conversation unavailable: ' + err.message; }); });
})();
