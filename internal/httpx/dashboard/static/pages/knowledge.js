/* knowledge.js — Knowledge pages module (OA-22, plan 09-01).
 *
 * Owns: the New-collection drawer (#knowledge-new-tpl cloned into the shared
 * #drawer — /dashboard/knowledge?new=1 deep link included), collection
 * document actions (upload via FormData, retry, delete via the shared
 * two-step confirm), the Settings tab save/delete, the Search tab's mode
 * chips, and the document-preview tool on /dashboard/knowledge/profiles
 * (ported verbatim from the deleted documents.js — same DOM ids).
 * Every write goes through window.Enclava.api against /api/v1 (D-17) — the
 * dashboard handlers render only.
 */
(function () {
  'use strict';

  if (window.Enclava === undefined) return;

  // ── Processing-profile selects (drawer + settings): hydrated from the ──
  // ── read-only profiles API; empty on failure, never a fabricated option ──

  function hydrateProfileSelects() {
    var selects = document.querySelectorAll('[data-knowledge-profile-select]');
    if (!selects.length) return;
    Enclava.api('GET', '/api/v1/document-profiles').then(function (body) {
      var profiles = (body && body.profiles) || [];
      Array.prototype.forEach.call(selects, function (select) {
        var current = select.dataset.current || '';
        profiles.forEach(function (p) {
          if (!p || !p.id) return;
          var opt = document.createElement('option');
          opt.value = p.id;
          opt.textContent = (p.profile_id || 'Profile') + ' · v' + p.version + ' · ' + p.id;
          if (p.id === current) opt.selected = true;
          select.appendChild(opt);
        });
        if (!profiles.length) {
          var opt = document.createElement('option');
          opt.textContent = 'No profiles available';
          select.appendChild(opt);
          select.disabled = true;
        }
      });
    }).catch(function () {
      Array.prototype.forEach.call(selects, function (select) {
        var note = select.parentElement && select.parentElement.querySelector('[data-knowledge-profile-note]');
        if (note) note.textContent = 'Profiles unavailable — the collection keeps its default.';
      });
    });
  }

  // ── New-collection drawer ───────────────────────────────────────────────

  function openNewDrawer() {
    var tpl = document.getElementById('knowledge-new-tpl');
    if (!tpl || !tpl.content) return;
    var holder = document.createElement('div');
    holder.appendChild(tpl.content.cloneNode(true));
    Enclava.openDrawer(holder.innerHTML);
    hydrateProfileSelects();
  }

  function submitNewCollection(form) {
    var err = form.querySelector('[data-knowledge-form-error]');
    var payload = {
      name: (form.elements.name.value || '').trim(),
      description: (form.elements.description.value || '').trim(),
    };
    var model = form.elements.embedding_model.value;
    if (model) payload.embedding_model = model;
    var profile = form.elements.chunk_profile_version_id.value;
    if (profile) payload.chunk_profile_version_id = profile;
    payload.acl = form.elements.access.value === 'org' ? { audience: ['*'] } : {};
    Enclava.api('POST', '/api/v1/rag/collections', payload).then(function (c) {
      Enclava.closeDrawer();
      Enclava.toast('success', 'Collection created');
      if (c && c.id) location.assign('/dashboard/knowledge/' + encodeURIComponent(c.id));
      else Enclava.refresh();
    }).catch(function (e) {
      if (err) { err.textContent = (e && e.message) || 'Create failed'; err.hidden = false; }
      else Enclava.toast('error', (e && e.message) || 'Create failed');
    });
  }

  // ── Documents tab: upload / retry / delete ──────────────────────────────

  function collectionID(root) {
    var wrap = root || document.getElementById('knowledge-documents');
    if (!wrap) return '';
    var match = (wrap.getAttribute('hx-get') || '').match(/collection=([^&]+)/);
    return match ? decodeURIComponent(match[1]) : '';
  }

  function uploadDocument(file) {
    if (!file) return;
    var id = collectionID();
    if (!id) return;
    var data = new FormData();
    data.append('file', file);
    Enclava.api('POST', '/api/v1/rag/collections/' + encodeURIComponent(id) + '/documents', data)
      .then(function () {
        Enclava.toast('success', 'Document uploaded — processing started');
        Enclava.refresh();
      })
      .catch(function (e) { Enclava.toast('error', (e && e.message) || 'Upload failed'); });
  }

  function retryDocument(documentID) {
    Enclava.api('POST', '/api/v1/rag/documents/' + encodeURIComponent(documentID) + '/reprocess')
      .then(function () {
        Enclava.toast('success', 'Reprocess queued');
        Enclava.refresh();
      })
      .catch(function (e) { Enclava.toast('error', (e && e.message) || 'Reprocess failed'); });
  }

  function deleteDocument(documentID) {
    Enclava.api('DELETE', '/api/v1/rag/documents/' + encodeURIComponent(documentID))
      .then(function () {
        Enclava.toast('success', 'Document deleted');
        Enclava.refresh();
      })
      .catch(function (e) { Enclava.toast('error', (e && e.message) || 'Delete failed'); });
  }

  // ── Settings tab: save (PUT) / delete collection ────────────────────────

  function saveSettings(form) {
    var id = form.dataset.collectionId;
    var err = form.querySelector('[data-knowledge-form-error]');
    var payload = {
      name: (form.elements.name.value || '').trim(),
      description: (form.elements.description.value || '').trim(),
      embedding_model: form.elements.embedding_model.value,
    };
    var profile = form.elements.chunk_profile_version_id.value;
    if (profile) payload.chunk_profile_version_id = profile;
    payload.acl = form.elements.access.value === 'org' ? { audience: ['*'] } : { audience: [] };
    Enclava.api('PUT', '/api/v1/rag/collections/' + encodeURIComponent(id), payload)
      .then(function () {
        Enclava.toast('success', 'Collection saved');
        Enclava.refresh();
      })
      .catch(function (e) {
        if (err) { err.textContent = (e && e.message) || 'Save failed'; err.hidden = false; }
        else Enclava.toast('error', (e && e.message) || 'Save failed');
      });
  }

  function deleteCollection(id) {
    Enclava.api('DELETE', '/api/v1/rag/collections/' + encodeURIComponent(id))
      .then(function () {
        Enclava.toast('success', 'Collection deleted');
        location.assign('/dashboard/knowledge');
      })
      .catch(function (e) { Enclava.toast('error', (e && e.message) || 'Delete failed'); });
  }

  // ── Events ──────────────────────────────────────────────────────────────

  document.addEventListener('click', function (e) {
    if (!(e.target instanceof Element)) return;
    var trigger = e.target.closest('[data-action]');
    if (!trigger) return;
    switch (trigger.dataset.action) {
      case 'knowledge:new':
        openNewDrawer();
        break;
      case 'knowledge:retry':
        retryDocument(trigger.dataset.documentId);
        break;
      case 'knowledge:filter':
        break; // chips are plain links — this hook exists for future grouping
    }
    // Search tab mode chips: toggle like search.js does (the search module
    // is not loaded on this page).
    var chip = e.target.closest('.chip[data-mode]');
    if (chip && document.getElementById('knowledgeSearchMode')) {
      var input = document.getElementById('knowledgeSearchMode');
      input.value = chip.dataset.mode;
      chip.parentElement.querySelectorAll('.chip[data-mode]').forEach(function (c) {
        c.classList.toggle('active', c === chip);
        c.setAttribute('aria-pressed', c === chip ? 'true' : 'false');
      });
    }
  });

  document.addEventListener('submit', function (e) {
    if (!(e.target instanceof HTMLFormElement)) return;
    if (e.target.matches('[data-knowledge-new-form]')) {
      e.preventDefault();
      submitNewCollection(e.target);
    } else if (e.target.matches('[data-knowledge-settings-form]')) {
      e.preventDefault();
      saveSettings(e.target);
    }
  });

  document.addEventListener('change', function (e) {
    if (!(e.target instanceof Element)) return;
    if (e.target.matches('[data-knowledge-upload-input]') && e.target.files && e.target.files.length) {
      var file = e.target.files[0];
      e.target.value = '';
      uploadDocument(file);
    }
  });

  // Two-step destructive confirm (shared `confirm` action) → the second
  // click lands here as enclava:confirm.
  document.addEventListener('enclava:confirm', function (e) {
    if (!(e.target instanceof Element)) return;
    if (e.target.dataset.knowledgeDocDel === '1' && e.target.dataset.documentId) {
      deleteDocument(e.target.dataset.documentId);
    } else if (e.target.dataset.knowledgeCollectionDel === '1' && e.target.dataset.collectionId) {
      deleteCollection(e.target.dataset.collectionId);
    }
  });

  // ?new=1 deep link opens the drawer (managers only — no template, no-op).
  function openFromDeepLink() {
    if (new URLSearchParams(location.search).get('new') === '1') openNewDrawer();
  }

  hydrateProfileSelects();
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', openFromDeepLink);
  } else {
    openFromDeepLink();
  }

  // ── Document-preview tool (ported verbatim from the deleted ─────────────
  // ── documents.js; runs only on /dashboard/knowledge/profiles) ────────────

  var profileSelect = document.getElementById('document-profile');
  var profileDetails = document.getElementById('document-profile-details');
  var form = document.getElementById('document-preview-form');
  if (!profileSelect || !profileDetails || !form) return;

  var statusNode = document.getElementById('document-preview-status');
  var submitButton = document.getElementById('document-preview-submit');
  var cancelButton = document.getElementById('document-preview-cancel');
  var warningsNode = document.getElementById('document-preview-warnings');
  var chunksNode = document.getElementById('document-preview-chunks');
  var profilesBody = document.getElementById('knowledge-profiles-body');
  var pollAttempts = 0;
  var maxPollAttempts = 300;
  var profileRequest = 0;
  var timer = null;
  var currentJob = '';

  function request(path, options) {
    return Enclava.api(options && options.method || 'GET', path, options && options.body);
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
    var rows = [
      ['Version ID', profile.id], ['Profile family', profile.profile_id], ['Version', profile.version],
      ['Boundary strategy', profile.boundary_strategy], ['Size (Unicode code points)', profile.size_codepoints],
      ['Overlap (Unicode code points)', profile.overlap_codepoints], ['Parent context', profile.include_parents ? 'Included' : 'Not included'],
      ['Table handling', profile.table_handling], ['Preserve clauses', profile.preserve_clauses ? 'Yes' : 'No'],
      ['Preserve authorship', profile.preserve_authorship ? 'Yes' : 'No']
    ];
    rows.forEach(function (row) {
      if (row[1] === undefined || row[1] === null || row[1] === '') return;
      var wrapper = element('div');
      wrapper.appendChild(element('dt', 'muted', row[0]));
      wrapper.appendChild(element('dd', '', row[1]));
      list.appendChild(wrapper);
    });
    profileDetails.appendChild(list);
  }

  function renderProfilesTable(profiles) {
    if (!profilesBody) return;
    profilesBody.replaceChildren();
    if (!profiles.length) {
      var row = element('tr');
      var cell = element('td', 'muted', 'No processing profiles exist yet.');
      cell.setAttribute('colspan', '3');
      row.appendChild(cell);
      profilesBody.appendChild(row);
      return;
    }
    profiles.forEach(function (p) {
      if (!p || !p.id) return;
      var row = element('tr');
      var name = element('td', '', p.profile_id || 'Profile');
      name.setAttribute('data-label', 'Profile');
      name.appendChild(element('span', 'subline', p.id));
      var version = element('td', '', 'v' + p.version);
      version.setAttribute('data-label', 'Version');
      var settings = element('td', '', [
        p.size_codepoints + ' code points',
        p.overlap_codepoints + ' overlap',
        p.boundary_strategy,
        p.include_parents ? 'parents' : null,
        p.table_handling
      ].filter(Boolean).join(' · '));
      settings.setAttribute('data-label', 'Settings');
      row.appendChild(name); row.appendChild(version); row.appendChild(settings);
      profilesBody.appendChild(row);
    });
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
      renderProfilesTable(profiles);
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
      renderProfilesTable([]);
      profileSelect.replaceChildren(element('option', '', 'Profiles unavailable'));
      profileDetails.textContent = 'Could not load existing profiles: ' + error.message;
      statusNode.textContent = 'Preview is unavailable until a profile can be loaded.';
    });
  }

  function showStatus(v) { statusNode.textContent = v; }

  function appendWarning(text) {
    var warningNode = element('p', 'field-error', text);
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
      chunksNode.appendChild(element('p', 'muted', 'Source version · ' + value(ref, ['object_id', 'ObjectID']) + ' · revision ' + value(ref, ['observed_seq', 'ObservedSeq'])));
    }
    if (body.chunk_profile_version_id) {
      chunksNode.appendChild(element('p', 'muted', 'Preview profile version · ' + body.chunk_profile_version_id));
    }
    var originalURL = body.original_url || documentData.original_url;
    if (originalURL) {
      try {
        var originalLinkURL = new URL(originalURL, window.location.origin);
        if (originalLinkURL.origin === window.location.origin) {
          var originalLink = element('a', '', 'Open retained original document');
          originalLink.href = originalLinkURL.href;
          originalLink.target = '_blank';
          originalLink.rel = 'noopener';
          chunksNode.appendChild(originalLink);
        }
      } catch (_) {}
    }
    chunks.forEach(function (chunk, index) {
      var card = element('article', 'card');
      var ordinal = value(chunk, ['ordinal', 'Ordinal']);
      var id = value(chunk, ['id', 'ID']);
      var heading = 'Chunk ' + (ordinal === undefined ? index + 1 : ordinal + 1);
      if (id) heading += ' · ' + id;
      card.appendChild(element('h3', '', heading));
      var chunkText = element('pre', '', value(chunk, ['text', 'Text']) || '');
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
            var sourceTextNode = element('pre', '', sourceText);
            sourceTextNode.style.whiteSpace = 'pre-wrap';
            sourceTextNode.style.overflowWrap = 'anywhere';
            card.appendChild(sourceTextNode);
          }
        }
        var provenanceNode = element('p', 'muted', provenance);
        provenanceNode.style.overflowWrap = 'anywhere';
        card.appendChild(provenanceNode);
      });
      var parentID = value(chunk, ['parent_chunk_id', 'ParentChunkID']);
      if (parentID) {
        var parent = chunkByID[parentID];
        card.appendChild(element('p', 'muted', 'Parent context' + (parent && value(parent, ['text', 'Text']) ? ': ' + value(parent, ['text', 'Text']) : ' · ' + parentID)));
      }
      var chunkWarnings = value(chunk, ['warnings', 'Warnings']);
      (Array.isArray(chunkWarnings) ? chunkWarnings : []).forEach(function (warning) { card.appendChild(element('p', 'field-error', typeof warning === 'string' ? warning : JSON.stringify(warning))); });
      chunksNode.appendChild(card);
    });
    if (!chunks.length) chunksNode.appendChild(element('p', 'muted', 'The preview completed without chunks.'));
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
    Enclava.api('POST', '/api/v1/document-previews', data).then(function (job) {
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
    Enclava.api('POST', '/api/v1/document-previews/' + encodeURIComponent(currentJob) + '/cancel').then(function () {
      showStatus('Cancellation requested; waiting for the terminal job state…');
      pollAttempts = 0;
      poll();
    }).catch(function (error) {
      showStatus('Preview could not be cancelled: ' + error.message);
    }).then(function () { cancelButton.disabled = false; });
  });

  loadProfiles();
})();
