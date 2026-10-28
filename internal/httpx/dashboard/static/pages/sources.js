/* sources.js — Sources page module (OA-23, plan 09-02).
 *
 * Owns: the five-step add-source wizard served into the shared #drawer by
 * GET /dashboard/partials/sources/drawer, the table row actions, and the
 * access.manage-gated user-bindings drawer. Every write goes through
 * window.Enclava.api against /api/v1/connectors/* (D-17) — the dashboard
 * handlers render only.
 *
 * Wizard state lives in the DOM (visible pane, selected tile), so a drawer
 * re-open always starts clean and no module state can go stale. Config
 * carries ONLY the selected provider's keys (each group contributes its own
 * data-config fields), sync_interval lands inside config as a duration
 * string, is_enabled stays top-level, and a successful create is ALWAYS
 * followed — in the same success path — by
 * POST /api/v1/connectors/sources/{id}/sync before any toast/refresh, so
 * the Syncing row and the toast reflect the sync call's real result.
 */
(function () {
  'use strict';

  if (window.Enclava === undefined) return;

  var PROVIDERS = {
    slack: { label: 'Slack' },
    teams: { label: 'Microsoft Teams' },
    github: { label: 'GitHub' },
    notion: { label: 'Notion' },
    google_drive: { label: 'Google Drive' },
  };
  var STEPS = 5;

  function drawer() { return document.getElementById('drawer'); }
  function q(sel) { var d = drawer(); return d ? d.querySelector(sel) : null; }
  function qa(sel) {
    var d = drawer();
    return d ? Array.prototype.slice.call(d.querySelectorAll(sel)) : [];
  }
  function activePane() { return q('[data-step-pane]:not([hidden])'); }
  function currentStep() {
    var pane = activePane();
    return pane ? Number(pane.dataset.stepPane) : 1;
  }
  function currentProvider() {
    var group = q('[data-provider-fields]:not([hidden])');
    return group ? group.dataset.providerFields : '';
  }

  function showError(msg) {
    var pane = activePane();
    var slot = pane && pane.querySelector('[data-wizard-error]');
    if (!slot) return;
    slot.textContent = msg || '';
    slot.hidden = !msg;
  }

  function reportError(err, id) {
    var action = Enclava.errorAction(err, id);
    Enclava.toast('error', action.text || (err && err.message) || 'That request failed', { action: action });
  }

  function setProvider(id) {
    qa('[data-provider-fields]').forEach(function (n) { n.hidden = n.dataset.providerFields !== id; });
    qa('[data-provider-access]').forEach(function (n) { n.hidden = n.dataset.providerAccess !== id; });
    qa('.tile').forEach(function (t) { t.classList.toggle('selected', t.dataset.provider === id); });
    // Switching provider can never leak another provider's keys into the
    // payload: collection happens at submit from the active group only.
    validateStep(currentStep());
  }

  // ── Collection from the DOM ─────────────────────────────────────────────

  function fieldList(raw) {
    return String(raw || '').split(/[\n,]/).map(function (s) { return s.trim(); })
      .filter(function (s) { return s !== ''; });
  }

  // collectConfig reads the active provider group; an unknown provider
  // yields an empty config (the server rejects the create — nothing is
  // invented client-side).
  function collectConfig(group) {
    var config = {};
    if (!group) return config;
    Array.prototype.forEach.call(group.querySelectorAll('[data-config]'), function (input) {
      var key = input.dataset.config;
      var type = input.dataset.type || 'text';
      if (type === 'bool') {
        if (input.checked) config[key] = true;
        return;
      }
      var raw = String(input.value || '').trim();
      if (raw === '') return;
      if (type === 'number') {
        var n = Number(raw);
        if (!isNaN(n)) config[key] = n;
        return;
      }
      if (type === 'list') {
        var items = fieldList(raw);
        if (items.length) config[key] = items;
        return;
      }
      config[key] = raw;
    });
    // Teams scope: single team → team_id, several → team_ids[]; channel
    // pairs build the adapter's allowedTeamChannel map {team: [ids]} —
    // omitted entirely when every channel of the selected teams is wanted.
    if (group.dataset.providerFields === 'teams') {
      var teams = fieldList(group.querySelector('[data-team-scope]').value);
      if (teams.length === 1) config.team_id = teams[0];
      else if (teams.length > 1) config.team_ids = teams;
      var pairs = fieldList(group.querySelector('[data-channels]').value);
      if (pairs.length) {
        var map = {};
        pairs.forEach(function (pair) {
          var i = pair.indexOf(':');
          if (i <= 0 || i === pair.length - 1) return;
          var team = pair.slice(0, i).trim();
          var channel = pair.slice(i + 1).trim();
          if (!team || !channel) return;
          (map[team] = map[team] || []).push(channel);
        });
        if (Object.keys(map).length) config.channel_ids = map;
      }
    }
    return config;
  }

  function collectAudiences() {
    return qa('[data-audience-row]').map(function (row) {
      var id = row.querySelector('[data-audience-id]').value.trim();
      var members = fieldList(row.querySelector('[data-audience-members]').value);
      return { id: id, members: members };
    }).filter(function (a) { return a.id !== '' && a.members.length > 0; });
  }

  function collectPayload(provider, step) {
    var group = q('[data-provider-fields="' + provider + '"]');
    var config = collectConfig(group);
    var interval = ((q('[data-source-interval]') || {}).value || '').trim();
    if (interval !== '') config.sync_interval = interval;
    var cred = group ? group.querySelector('[data-credential]') : null;
    var payload = {
      type: provider,
      name: ((q('[data-source-name]') || {}).value || '').trim(),
      config: config,
      credential_ref: cred ? (cred.value || '').trim() : '',
      target_collection_id: (q('[data-source-collection]') || {}).value || '',
      is_enabled: q('[data-source-enabled]') ? q('[data-source-enabled]').checked : true,
    };
    if (provider === 'notion' && step >= 3) {
      var pages = fieldList((q('[data-notion-pages]') || {}).value);
      var audiences = collectAudiences();
      payload.config.selected_pages = pages.map(function (pageID) {
        return { page_id: pageID, audiences: audiences };
      });
    }
    return payload;
  }

  // ── Step validation + navigation ────────────────────────────────────────

  function stepValid(step, provider) {
    var group;
    switch (step) {
      case 1:
        return !!provider;
      case 2:
        group = q('[data-provider-fields="' + provider + '"]');
        if (!group) return false;
        var missing = Array.prototype.some.call(
          group.querySelectorAll('[required]:not([data-credential])'),
          function (input) { return !String(input.value || '').trim(); });
        if (missing) return false;
        var cred = group.querySelector('[data-credential]');
        if (cred && cred.required && !cred.value.trim()) return false;
        if (provider === 'teams' && fieldList(group.querySelector('[data-team-scope]').value).length === 0) return false;
        return true;
      case 3:
        if (provider !== 'notion') return true;
        return fieldList((q('[data-notion-pages]') || {}).value).length > 0 && collectAudiences().length > 0;
      case 4:
        return ((q('[data-source-name]') || {}).value || '').trim() !== '' &&
          ((q('[data-source-collection]') || {}).value || '') !== '';
      default:
        return true;
    }
  }

  function validateStep(step, provider) {
    var next = q('[data-action="sources:next"]');
    var submit = q('[data-action="sources:submit"]');
    if (!next || !submit) return false;
    var ok = stepValid(step, provider);
    next.disabled = !ok;
    submit.disabled = !ok;
    if (!ok) showError('');
    return ok;
  }

  function applyStep(step) {
    qa('[data-step-pane]').forEach(function (pane) {
      pane.hidden = Number(pane.dataset.stepPane) !== step;
    });
    qa('[data-step-dot]').forEach(function (dot) {
      dot.classList.toggle('active', Number(dot.dataset.stepDot) <= step);
    });
    var back = q('[data-action="sources:back"]');
    var next = q('[data-action="sources:next"]');
    var submit = q('[data-action="sources:submit"]');
    if (back) back.disabled = step === 1;
    if (next && submit) {
      next.hidden = step === STEPS;
      submit.hidden = step !== STEPS;
    }
    if (step === STEPS) renderReview();
    validateStep(step, currentProvider());
  }

  // ── Review ───────────────────────────────────────────────────────────────

  function reviewRows(provider, payload) {
    var label = (PROVIDERS[provider] || { label: provider }).label;
    var scope = [];
    Object.keys(payload.config).forEach(function (key) {
      if (key === 'sync_interval') return;
      var value = payload.config[key];
      if (key === 'selected_pages') {
        scope.push('pages: ' + value.map(function (p) { return p.page_id; }).join(', '));
        if (value.length && value[0].audiences.length) {
          scope.push('audiences: ' + value[0].audiences.map(function (a) { return a.id; }).join(', '));
        }
        return;
      }
      if (key === 'channel_ids') {
        scope.push('channels: ' + Object.keys(value).map(function (team) {
          return team + ' → ' + value[team].join(', ');
        }).join('; '));
        return;
      }
      scope.push(key + ': ' + (Array.isArray(value) ? value.join(', ') : String(value)));
    });
    var collection = q('[data-source-collection]');
    var option = collection && collection.selectedOptions && collection.selectedOptions[0];
    return [
      ['Provider', label],
      ['Scope', scope.join(' · ') || '—'],
      ['Credential reference', payload.credential_ref || '—'],
      ['Destination', option ? option.textContent : payload.target_collection_id],
      ['Schedule', payload.config.sync_interval || 'default (1h)'],
      ['Enabled', payload.is_enabled ? 'Yes' : 'No — paused on creation'],
    ];
  }

  function renderReview() {
    var target = q('[data-source-review]');
    if (!target) return;
    var provider = currentProvider();
    var payload = collectPayload(provider, 5);
    target.innerHTML = '';
    reviewRows(provider, payload).forEach(function (pair) {
      var row = document.createElement('div');
      row.className = 'row';
      var name = document.createElement('span');
      name.className = 'label';
      name.textContent = pair[0];
      var value = document.createElement('span');
      value.textContent = pair[1];
      row.appendChild(name);
      row.appendChild(value);
      target.appendChild(row);
    });
  }

  // ── Submit: create, then chain the real first sync ──────────────────────

  function submitWizard() {
    var step = currentStep();
    var provider = currentProvider();
    if (!validateStep(step, provider)) return;
    var payload = collectPayload(provider, step);
    var submit = q('[data-action="sources:submit"]');
    if (submit) submit.disabled = true;
    Enclava.api('POST', '/api/v1/connectors/sources', payload, { onRetry: submitWizard })
      .then(function (created) {
        // The SAME success path starts the real first sync — create alone
        // never schedules one. Toast + close + refresh happen only after the
        // sync call resolves, so the row shows the sync call's real result
        // (a failed sync call surfaces its API error, never a fabricated
        // Syncing).
        return Enclava.api('POST', '/api/v1/connectors/sources/' + encodeURIComponent(created.id) + '/sync')
          .then(function () {
            Enclava.toast('success', 'Source created — first sync started');
          }, function (err) {
            reportError(err, created.id);
          });
      })
      .then(function () {
        Enclava.closeDrawer();
        Enclava.refresh();
      })
      .catch(function (err) {
        if (submit) submit.disabled = false;
        showError((err && err.message) || 'Creating the source failed.');
        reportError(err);
      });
  }

  // ── Row actions ──────────────────────────────────────────────────────────

  function syncNow(id) {
    Enclava.api('POST', '/api/v1/connectors/sources/' + encodeURIComponent(id) + '/sync')
      .then(function () { Enclava.toast('success', 'Sync started'); Enclava.refresh(); })
      .catch(function (err) { reportError(err, id); });
  }

  function toggleEnabled(button) {
    var id = button.dataset.sourceId;
    // Read the current state first — a stale render must never flip the
    // wrong value.
    Enclava.api('GET', '/api/v1/connectors/sources/' + encodeURIComponent(id))
      .then(function (source) {
        return Enclava.api('PUT', '/api/v1/connectors/sources/' + encodeURIComponent(id),
          { is_enabled: !source.is_enabled });
      })
      .then(function (source) {
        Enclava.toast('success', source.is_enabled ? 'Source resumed' : 'Source paused');
        Enclava.refresh();
      })
      .catch(function (err) { reportError(err, id); });
  }

  function deleteSource(id) {
    Enclava.api('DELETE', '/api/v1/connectors/sources/' + encodeURIComponent(id))
      .then(function () { Enclava.toast('success', 'Source deleted'); Enclava.refresh(); })
      .catch(function (err) { reportError(err, id); });
  }

  function bindingsURL(id) {
    return '/dashboard/partials/sources/bindings?source=' + encodeURIComponent(id);
  }

  function bindUser(button) {
    var root = drawer();
    var payload = {
      source_user_native_id: ((root.querySelector('[data-bind-native]') || {}).value || '').trim(),
      mapped_user_id: ((root.querySelector('[data-bind-mapped]') || {}).value || '').trim(),
      verification_evidence: ((root.querySelector('[data-bind-evidence]') || {}).value || '').trim(),
    };
    if (!payload.source_user_native_id || !payload.mapped_user_id || !payload.verification_evidence) {
      var slot = root.querySelector('[data-wizard-error]');
      if (slot) { slot.textContent = 'Native user, mapped user and verification evidence are required.'; slot.hidden = false; }
      return;
    }
    Enclava.api('POST', '/api/v1/connectors/sources/' + encodeURIComponent(button.dataset.sourceId) + '/bindings', payload)
      .then(function () {
        Enclava.toast('success', 'User bound');
        return Enclava.openDrawer(bindingsURL(button.dataset.sourceId));
      })
      .catch(function (err) { reportError(err, button.dataset.sourceId); });
  }

  function unbindUser(button) {
    Enclava.api('DELETE', '/api/v1/connectors/sources/' + encodeURIComponent(button.dataset.sourceId) +
      '/bindings/' + encodeURIComponent(button.dataset.bindingId))
      .then(function () {
        Enclava.toast('success', 'Binding revoked');
        return Enclava.openDrawer(bindingsURL(button.dataset.sourceId));
      })
      .catch(function (err) { reportError(err, button.dataset.sourceId); });
  }


  function saveCredential() {
    var section = q('[data-source-edit]');
    var input = q('[data-edit-credential]');
    if (!section || !input) return;
    var id = section.dataset.sourceEdit;
    Enclava.api('PUT', '/api/v1/connectors/sources/' + encodeURIComponent(id), { credential_ref: input.value.trim() })
      .then(function () { Enclava.closeDrawer(); Enclava.toast('success', 'Connection saved'); Enclava.refresh(); })
      .catch(function (err) { reportError(err, id); });
  }
  // ── Events ───────────────────────────────────────────────────────────────

  function addAudienceRow() {
    var list = q('#notion-audiences');
    if (!list) return;
    var first = list.querySelector('[data-audience-row]');
    var row = first.cloneNode(true);
    Array.prototype.forEach.call(row.querySelectorAll('input'), function (input) { input.value = ''; });
    list.appendChild(row);
    validateStep(currentStep(), currentProvider());
  }

  document.addEventListener('click', function (e) {
    if (!(e.target instanceof Element)) return;
    var trigger = e.target.closest('[data-action^="sources:"]');
    if (!trigger) return;
    switch (trigger.dataset.action) {
      case 'sources:provider':
        setProvider(trigger.dataset.provider);
        break;
      case 'sources:next': {
        var step = currentStep();
        if (step < STEPS) applyStep(step + 1);
        break;
      }
      case 'sources:back': {
        var back = currentStep();
        if (back > 1) applyStep(back - 1);
        break;
      }
      case 'sources:submit':
        submitWizard();
        break;
      case 'sources:save-credential':
        saveCredential();
        break;
      case 'sources:sync':
        syncNow(trigger.dataset.sourceId);
        break;
      case 'sources:toggle':
        toggleEnabled(trigger);
        break;
      case 'sources:bindings':
        Enclava.openDrawer(bindingsURL(trigger.dataset.sourceId));
        break;
      case 'sources:bind':
        bindUser(trigger);
        break;
      case 'sources:unbind':
        unbindUser(trigger);
        break;
      case 'sources:audience-add':
        addAudienceRow();
        break;
      case 'sources:audience-remove': {
        var row = trigger.closest('[data-audience-row]');
        if (qa('[data-audience-row]').length > 1) row.remove();
        else Array.prototype.forEach.call(row.querySelectorAll('input'), function (i) { i.value = ''; });
        validateStep(currentStep(), currentProvider());
        break;
      }
    }
  });

  // Two-step destructive confirm (shared `confirm` action) → delete source.
  document.addEventListener('enclava:confirm', function (e) {
    if (!(e.target instanceof Element)) return;
    if (e.target.dataset.sourceDel === '1' && e.target.dataset.sourceId) {
      deleteSource(e.target.dataset.sourceId);
    }
  });

  // Required-field gate: enable Continue only once the visible step is valid.
  document.addEventListener('input', function (e) {
    if (!(e.target instanceof Element)) return;
    var root = drawer();
    if (!root || !root.contains(e.target)) return;
    validateStep(currentStep(), currentProvider());
  });

  // Wizard opens via ?new=1 (optionally preset with &collection={id}) — the
  // same URL the Add source empty-state action carries.
  function openFromDeepLink() {
    var params = new URLSearchParams(location.search);
    var edit = params.get('edit');
    if (edit) {
      Enclava.openDrawer('/dashboard/partials/sources/drawer?source=' + encodeURIComponent(edit)).then(function () {
        var input = q('[data-edit-credential]');
        if (input) input.focus();
      });
      return;
    }
    if (params.get('new') !== '1') return;
    var collection = params.get('collection') || '';
    Enclava.openDrawer('/dashboard/partials/sources/drawer' +
      (collection ? '?collection=' + encodeURIComponent(collection) : ''));
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', openFromDeepLink);
  } else {
    openFromDeepLink();
  }
})();
