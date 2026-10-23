(function () {
  function request(url, options) {
    options = options || {};
    options.credentials = 'same-origin';
    options.headers = Object.assign({'Content-Type': 'application/json'}, options.headers || {});
    return fetch(url, options).then(function (response) {
      return response.json().catch(function () { return {}; }).then(function (body) {
        if (!response.ok) throw new Error(body.error && body.error.message || body.message || 'Request failed');
        return body;
      });
    });
  }
  function node(tag, text, cls) {
    var el = document.createElement(tag);
    if (text) el.textContent = text;
    if (cls) el.className = cls;
    return el;
  }
  function attach(root) {
    var form = root.querySelector('#knowledge-search-form');
    if (form && !form.dataset.bound) {
      form.dataset.bound = 'true';
      form.addEventListener('submit', function (event) {
        event.preventDefault();
        var data = new FormData(form);
        var target = root.querySelector('#knowledge-search-results');
        target.replaceChildren(node('p', 'Searching…', 'text-sm text-muted-foreground'));
        var collection = String(data.get('collection_id'));
        request('/api/v1/rag/collections/' + encodeURIComponent(collection) + '/search', {method: 'POST', body: JSON.stringify({query: data.get('query'), mode: data.get('mode'), limit: 10})})
          .then(function (result) {
            target.replaceChildren();
            (result.results || []).forEach(function (item) {
              var card = node('article', null, 'mb-3 rounded-md border p-3');
              card.appendChild(node('p', item.content || '', 'whitespace-pre-wrap text-sm'));
              card.appendChild(node('p', 'Evidence: ' + (item.evidence_id || 'unavailable') + ' · ' + (item.file_name || 'source'), 'mt-2 font-mono text-xs'));
              card.appendChild(node('p', 'Generation: ' + (item.generation || 'unknown'), 'text-xs text-muted-foreground'));
              if (item.evidence_id) {
                var evidenceURL = '/api/v1/rag/evidence/' + encodeURIComponent(item.evidence_id);
                var evidenceState = node('p', 'Evidence state: resolving…', 'text-xs text-muted-foreground');
                var evidenceRequest;
                function resolveEvidence() {
                  if (!evidenceRequest) evidenceRequest = request(evidenceURL);
                  return evidenceRequest;
                }
                card.appendChild(evidenceState);
                resolveEvidence().then(function (evidence) {
                  evidenceState.textContent = 'Evidence state: ' + (evidence.status || 'unknown');
                }).catch(function () {
                  evidenceState.textContent = 'Evidence state: unavailable';
                });
                var link = node('a', 'Resolve evidence');
                link.href = evidenceURL;
                link.className = 'mr-3 text-sm underline';
                link.addEventListener('click', function (e) {
                  e.preventDefault();
                  resolveEvidence().then(function (evidence) {
                    var detail = node('p', null, 'mt-2 text-xs');
                    detail.textContent = 'Status: ' + evidence.status + '; locations: ' + JSON.stringify(evidence.locations || evidence.spans || []);
                    if (evidence.native_link) { var native = node('a', 'Open source'); native.href = evidence.native_link; native.rel = 'noopener noreferrer'; detail.appendChild(document.createTextNode(' ')); detail.appendChild(native); }
                    if (evidence.retained_copy_url) { detail.appendChild(document.createTextNode(' ' + (evidence.retained_copy_label || 'retained copy — may differ from current source') + ': ')); var copy = node('a', 'Download'); copy.href = evidence.retained_copy_url; detail.appendChild(copy); }
                    card.appendChild(detail);
                  }).catch(function () { card.appendChild(node('p', 'Evidence unavailable.', 'mt-2 text-xs text-red-700')); });
                });
                card.appendChild(link);
              }
              target.appendChild(card);
            });
            if (!(result.results || []).length) target.appendChild(node('p', 'No authorized results.', 'text-sm text-muted-foreground'));
            var context = result.context || {};
            if (context.incomplete || context.neighbor_withheld || context.denied || context.incomplete_context || context.denied_neighbors) target.appendChild(node('p', context.neighbor_withheld ? 'Context is incomplete; a neighboring source item was withheld because it is unavailable or denied.' : 'Context is incomplete; some source content is unavailable.', 'rounded bg-yellow-50 p-2 text-sm text-yellow-900'));
          })
          .catch(function (error) { target.replaceChildren(node('p', error.message, 'text-sm text-red-700')); });
      });
    }
    var importForm = root.querySelector('#knowledge-import-form');
    if (importForm && !importForm.dataset.bound) {
      importForm.dataset.bound = 'true';
      importForm.addEventListener('submit', function (event) {
        event.preventDefault();
        var data = new FormData(importForm), status = root.querySelector('#knowledge-import-status');
        request('/api/v1/knowledge/imports', {method: 'POST', body: JSON.stringify({collection_id: data.get('collection_id'), result_ref: data.get('result_ref')})})
          .then(function () { status.textContent = 'Import queued.'; })
          .catch(function (error) { status.textContent = error.message; });
      });
    }
  }
  document.addEventListener('DOMContentLoaded', function () { attach(document); });
  document.addEventListener('htmx:afterSwap', function (event) { attach(event.target); });
  // This script loads inside the swapped partial itself, after this swap's
  // htmx:afterSwap already fired — attach immediately (idempotent).
  attach(document);
})();
