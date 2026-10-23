(function () {
  'use strict';

  var profileSelect = document.getElementById('document-profile');
  var profileDetails = document.getElementById('document-profile-details');
  var form = document.getElementById('document-preview-form');
  if (!profileSelect || !profileDetails || !form) return;

  var statusNode = document.getElementById('document-preview-status');
  var submitButton = document.getElementById('document-preview-submit');
  var cancelButton = document.getElementById('document-preview-cancel');
  var warningsNode = document.getElementById('document-preview-warnings');
  var chunksNode = document.getElementById('document-preview-chunks');
  var csrfCookie = /(?:^|;\s*)enclava_csrf=([^;]*)/;
  var pollAttempts = 0;
  var maxPollAttempts = 300;
  var profileRequest = 0;
  var timer = null;
  var currentJob = '';
  var maxResponseBytes = 64 * 1024 * 1024;

  function csrfToken() {
    var match = document.cookie.match(csrfCookie);
    return match ? decodeURIComponent(match[1]) : '';
  }

  function request(path, options) {
    options = options || {};
    options.credentials = 'same-origin';
    options.cache = 'no-store';
    options.headers = options.headers || {};
    var token = csrfToken();
    if (token) options.headers['X-CSRF-Token'] = token;
    return fetch(path, options).then(function (response) {
      return response.text().then(function (text) {
        if (text.length > maxResponseBytes) throw new Error('The server response exceeded the preview size limit.');
        var body = {};
        if (text) {
          try { body = JSON.parse(text); }
          catch (_) { throw new Error('The server returned an invalid response.'); }
        }
        if (!response.ok) throw new Error(body.error || body.message || 'Request failed (' + response.status + ').');
        return body;
      });
    });
  }

  function element(tag, className, text) {
    var node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined && text !== null) node.textContent = String(text);
    return node;
  }

  function value(object, keys) {
    if (!object) return undefined;
    for (var i = 0; i < keys.length; i++) {
      if (object[keys[i]] !== undefined) return object[keys[i]];
    }
    return undefined;
  }

  function recordDetail(profile) {
    profileDetails.replaceChildren();
    if (!profile) {
      profileDetails.textContent = 'Select an existing profile version to review its immutable controls.';
      return;
    }
    var list = document.createElement('dl');
    list.className = 'grid grid-cols-1 sm:grid-cols-2 gap-x-4 gap-y-2';
    var rows = [
      ['Version ID', profile.id], ['Profile family', profile.profile_id], ['Version', profile.version],
      ['Boundary strategy', profile.boundary_strategy], ['Size (Unicode code points)', profile.size_codepoints],
      ['Overlap (Unicode code points)', profile.overlap_codepoints], ['Parent context', profile.include_parents ? 'Included' : 'Not included'],
      ['Table handling', profile.table_handling], ['Preserve clauses', profile.preserve_clauses ? 'Yes' : 'No'],
      ['Preserve authorship', profile.preserve_authorship ? 'Yes' : 'No']
    ];
    rows.forEach(function (row) {
      if (row[1] === undefined || row[1] === null || row[1] === '') return;
      var wrapper = element('div', 'min-w-0');
      wrapper.appendChild(element('dt', 'text-xs font-medium text-muted-foreground', row[0]));
      wrapper.appendChild(element('dd', 'break-words text-sm', row[1]));
      list.appendChild(wrapper);
    });
    profileDetails.appendChild(list);
  }

  function loadProfile(id) {
    var requestID = ++profileRequest;
    if (!id) { recordDetail(null); return Promise.resolve(false); }
    return request('/api/v1/document-profiles/' + encodeURIComponent(id)).then(function (body) {
      if (requestID !== profileRequest) return false;
      recordDetail(body.profile);
      return !!body.profile;
    }).catch(function (error) {
      if (requestID === profileRequest) {
        recordDetail(null);
        profileDetails.textContent = 'Profile unavailable: ' + error.message;
      }
      return false;
    });
  }

  function loadProfiles() {
    request('/api/v1/document-profiles').then(function (body) {
      var profiles = Array.isArray(body.profiles) ? body.profiles : [];
      profileSelect.replaceChildren();
      if (!profiles.length) {
        profileSelect.appendChild(element('option', '', 'No profiles are available.'));
        profileDetails.textContent = 'No existing document profiles are available.';
        return;
      }
      profiles.forEach(function (profile) {
        if (!profile || !profile.id) return;
        var option = element('option', '', (profile.profile_id || 'Profile') + ' · v' + profile.version + ' · ' + profile.id);
        option.value = profile.id;
        profileSelect.appendChild(option);
      });
      profileSelect.disabled = profileSelect.options.length === 0;
      submitButton.disabled = true;
      if (profileSelect.options.length) {
        var selectedID = profileSelect.value;
        loadProfile(selectedID).then(function (available) {
          if (profileSelect.value === selectedID) submitButton.disabled = !available;
        });
      }
    }).catch(function (error) {
      profileSelect.replaceChildren(element('option', '', 'Profiles unavailable'));
      profileDetails.textContent = 'Could not load existing profiles: ' + error.message;
      statusNode.textContent = 'Preview is unavailable until a profile can be loaded.';
    });
  }

  function showStatus(value) {
    statusNode.textContent = value;
  }

  function appendWarning(text) {
    var warningNode = element('p', 'rounded-md border border-amber-300 bg-amber-50 p-3 text-sm text-amber-950 dark:border-amber-900 dark:bg-amber-950/30 dark:text-amber-100', text);
    warningNode.style.overflowWrap = 'anywhere';
    warningsNode.appendChild(warningNode);
  }

  function renderWarnings(warnings) {
    warningsNode.replaceChildren();
    if (!Array.isArray(warnings)) return;
    warnings.forEach(function (warning) {
      appendWarning(typeof warning === 'string' ? warning : JSON.stringify(warning));
    });
  }

  function renderPreview(body) {
    var documentData = body.document || {};
    var blocks = Array.isArray(documentData.blocks) ? documentData.blocks : [];
    var chunks = Array.isArray(body.chunks) ? body.chunks : (Array.isArray(documentData.chunks) ? documentData.chunks : []);
    var blockByID = Object.create(null);
    var displayedBlocks = Object.create(null);
    var chunkByID = Object.create(null);
    blocks.forEach(function (block) {
      var id = value(block, ['id', 'ID']);
      if (id) blockByID[id] = block;
    });
    chunks.forEach(function (chunk) {
      var id = value(chunk, ['id', 'ID']);
      if (id) chunkByID[id] = chunk;
    });
    renderWarnings((Array.isArray(body.warnings) ? body.warnings : []).concat(Array.isArray(documentData.warnings) ? documentData.warnings : []));
    chunksNode.replaceChildren();
    var ref = value(documentData, ['ref', 'Ref']);
    if (ref) {
      chunksNode.appendChild(element('p', 'break-words text-xs text-muted-foreground', 'Source version · ' + value(ref, ['object_id', 'ObjectID']) + ' · revision ' + value(ref, ['observed_seq', 'ObservedSeq'])));
    }
    if (body.chunk_profile_version_id) {
      chunksNode.appendChild(element('p', 'break-words text-xs text-muted-foreground', 'Preview profile version · ' + body.chunk_profile_version_id));
    }
    var originalURL = body.original_url || documentData.original_url;
    if (originalURL) {
      try {
        var originalLinkURL = new URL(originalURL, window.location.origin);
        if (originalLinkURL.origin === window.location.origin) {
          var originalLink = element('a', 'inline-flex text-sm text-primary underline underline-offset-4 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring', 'Open retained original document');
          originalLink.href = originalLinkURL.href;
          originalLink.target = '_blank';
          originalLink.rel = 'noopener';
          chunksNode.appendChild(originalLink);
        }
      } catch (_) {}
    }
    chunks.forEach(function (chunk, index) {
      var card = element('article', 'rounded-lg border p-4 space-y-3');
      var ordinal = value(chunk, ['ordinal', 'Ordinal']);
      var id = value(chunk, ['id', 'ID']);
      var heading = 'Chunk ' + (ordinal === undefined ? index + 1 : ordinal + 1);
      if (id) heading += ' · ' + id;
      card.appendChild(element('h3', 'font-medium', heading));
      var chunkText = element('pre', 'rounded-md bg-muted/40 p-3 text-sm font-mono', value(chunk, ['text', 'Text']) || '');
      chunkText.style.whiteSpace = 'pre-wrap';
      chunkText.style.overflowWrap = 'anywhere';
      card.appendChild(chunkText);
      var spans = value(chunk, ['spans', 'Spans']);
      spans = Array.isArray(spans) ? spans : [];
      spans.forEach(function (span) {
        var blockID = value(span, ['block_id', 'BlockID', 'blockId']);
        var sourceBlock = blockByID[blockID];
        var sourceLocation = sourceBlock && value(sourceBlock, ['locator', 'Locator']);
        var start = value(span, ['byte_start', 'ByteStart']);
        var end = value(span, ['byte_end', 'ByteEnd']);
        var provenance = 'Source span';
        if (start !== undefined && end !== undefined) provenance += ' · UTF-8 bytes ' + start + '–' + end;
        if (sourceBlock) {
          provenance += ' · block ' + value(sourceBlock, ['ordinal', 'Ordinal']) + ' (' + (value(sourceBlock, ['kind', 'Kind']) || 'source') + ')';
          if (sourceLocation) provenance += ' · ' + JSON.stringify(sourceLocation);
          var sourceText = value(sourceBlock, ['text', 'Text']);
          if (sourceText && !displayedBlocks[blockID]) {
            displayedBlocks[blockID] = true;
            var sourceTextNode = element('pre', 'rounded-md border p-3 text-xs font-mono', sourceText);
            sourceTextNode.style.whiteSpace = 'pre-wrap';
            sourceTextNode.style.overflowWrap = 'anywhere';
            card.appendChild(sourceTextNode);
          }
        }
        var provenanceNode = element('p', 'text-xs text-muted-foreground', provenance);
        provenanceNode.style.overflowWrap = 'anywhere';
        card.appendChild(provenanceNode);
      });
      var parentID = value(chunk, ['parent_chunk_id', 'ParentChunkID']);
      if (parentID) {
        var parent = chunkByID[parentID];
        card.appendChild(element('p', 'text-xs text-muted-foreground', 'Parent context' + (parent && value(parent, ['text', 'Text']) ? ': ' + value(parent, ['text', 'Text']) : ' · ' + parentID)));
      }
      var chunkWarnings = value(chunk, ['warnings', 'Warnings']);
      (Array.isArray(chunkWarnings) ? chunkWarnings : []).forEach(function (warning) { card.appendChild(element('p', 'text-sm text-amber-700 dark:text-amber-300', typeof warning === 'string' ? warning : JSON.stringify(warning))); });
      chunksNode.appendChild(card);
    });
    if (!chunks.length) chunksNode.appendChild(element('p', 'text-sm text-muted-foreground', 'The preview completed without chunks.'));
  }

  function stopPolling() {
    if (timer) window.clearTimeout(timer);
    timer = null;
  }

  function poll() {
    if (!currentJob) return;
    if (pollAttempts >= maxPollAttempts) {
      stopPolling();
      showStatus('Preview is still running. Automatic polling paused after five minutes; cancelling will resume status checks.');
      return;
    }
    pollAttempts++;
    request('/api/v1/document-previews/' + encodeURIComponent(currentJob)).then(function (job) {
      var state = job.status || 'unknown';
      showStatus('Preview ' + state + (job.error_message ? ': ' + job.error_message : '.'));
      if (state === 'queued' || state === 'running') {
        timer = window.setTimeout(poll, 1000);
        return;
      }
      stopPolling();
      submitButton.disabled = false;
      cancelButton.hidden = true;
      if (state === 'succeeded') {
        return request('/api/v1/document-previews/' + encodeURIComponent(currentJob) + '/result').then(renderPreview).catch(function (error) {
          chunksNode.replaceChildren();
          renderWarnings([]);
          appendWarning('Preview result unavailable: ' + error.message);
          showStatus('Preview succeeded, but its result is unavailable.');
        });
      }
      chunksNode.replaceChildren();
    }).catch(function (error) {
      stopPolling();
      cancelButton.hidden = true;
      chunksNode.replaceChildren();
      renderWarnings([]);
      appendWarning('Preview unavailable: ' + error.message);
      showStatus('Preview status unavailable.');
      submitButton.disabled = false;
    });
  }

  profileSelect.addEventListener('change', function () {
    var selectedID = profileSelect.value;
    submitButton.disabled = true;
    loadProfile(selectedID).then(function (available) {
      if (profileSelect.value === selectedID) submitButton.disabled = !available;
    });
  });

  form.addEventListener('submit', function (event) {
    event.preventDefault();
    if (!profileSelect.value || !form.elements.file.files.length) return;
    stopPolling();
    chunksNode.replaceChildren();
    renderWarnings([]);
    submitButton.disabled = true;
    cancelButton.hidden = true;
    showStatus('Uploading document and creating preview…');
    var data = new FormData();
    data.append('chunk_profile_version_id', profileSelect.value);
    data.append('file', form.elements.file.files[0]);
    request('/api/v1/document-previews', { method: 'POST', body: data }).then(function (job) {
      if (!job.job_id) throw new Error('The preview service did not return a job ID.');
      currentJob = job.job_id;
      showStatus('Preview ' + (job.status || 'queued') + '.');
      pollAttempts = 0;
      cancelButton.hidden = false;
      poll();
    }).catch(function (error) {
      showStatus('Preview could not be started: ' + error.message);
      submitButton.disabled = false;
    });
  });

  cancelButton.addEventListener('click', function () {
    if (!currentJob) return;
    cancelButton.disabled = true;
    showStatus('Cancelling preview…');
    request('/api/v1/document-previews/' + encodeURIComponent(currentJob) + '/cancel', { method: 'POST' }).then(function () {
      showStatus('Cancellation requested; waiting for the terminal job state…');
      pollAttempts = 0;
      poll();
    }).catch(function (error) {
      showStatus('Preview could not be cancelled: ' + error.message);
    }).then(function () { cancelButton.disabled = false; });
  });

  loadProfiles();
}());
