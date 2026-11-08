/* sources.js — Sources page module (OA-23, plan 09-02).
 *
 * Owns: the five-step add-source wizard served into the shared #drawer by
 * GET /dashboard/partials/sources/drawer and the table row actions. Every
 * write goes through
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
    gitlab: { label: 'GitLab' },
    jira: { label: 'Jira' },
    linear: { label: 'Linear' },
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
    qa('.tile').forEach(function (t) {
      var on = t.dataset.provider === id;
      t.classList.toggle('selected', on);
      t.setAttribute('aria-pressed', on ? 'true' : 'false');
    });
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
      payload.config.selected_pages = pages.map(function (pageID) {
        return { page_id: pageID };
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
        return fieldList((q('[data-notion-pages]') || {}).value).length > 0;
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
      var reached = Number(dot.dataset.stepDot) <= step;
      dot.classList.toggle('active', reached);
      if (Number(dot.dataset.stepDot) === step) dot.setAttribute('aria-current', 'step');
      else dot.removeAttribute('aria-current');
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
    // Move focus onto the new pane's first VISIBLE control (provider groups
    // stay in the DOM hidden); a read-only pane falls back to the primary
    // action so keyboard users land inside the step.
    var pane = activePane();
    if (pane) {
      var candidates = pane.querySelectorAll('input:not([type="hidden"]), select, textarea, button');
      var target = null;
      for (var i = 0; i < candidates.length; i++) {
        if (candidates[i].offsetParent !== null) { target = candidates[i]; break; }
      }
      if (!target && submit && !submit.hidden) target = submit;
      if (!target && next && !next.hidden) target = next;
      if (target) target.focus();
    }
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
        return;
      }
      if (key === 'channel_id' && provider === 'slack') {
        var pick = q('[data-slack-channels]');
        var picked = pick && pick.selectedOptions && pick.selectedOptions[0];
        scope.push('channel: ' + (picked && picked.value === value ? picked.textContent : String(value)));
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
    if (provider === 'slack' && !payload.config.channel_id) scope.unshift('channels: all the credential is a member of');
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


  function saveCredential() {
    var section = q('[data-source-edit]');
    var input = q('[data-edit-credential]');
    if (!section || !input) return;
    var id = section.dataset.sourceEdit;
    Enclava.api('PUT', '/api/v1/connectors/sources/' + encodeURIComponent(id), { credential_ref: input.value.trim() })
      .then(function () { Enclava.closeDrawer(); Enclava.toast('success', 'Connection saved'); Enclava.refresh(); })
      .catch(function (err) { reportError(err, id); });
  }

  // ── Credentials tab (access.manage): every write via Enclava.api ────────

  function credsSection() { return document.getElementById('sources-credentials'); }
  function credsCard(provider) {
    var root = credsSection();
    return root ? root.querySelector('[data-cred-provider="' + provider + '"]') : null;
  }
  function credsField(card, sel) {
    var input = card && card.querySelector(sel);
    return input ? String(input.value || '').trim() : '';
  }
  function credsShowError(card, msg) {
    var slot = card && card.querySelector('[data-creds-error]');
    if (slot) { slot.textContent = msg || ''; slot.hidden = !msg; }
  }

  function credsAdd(provider) {
    var card = credsCard(provider);
    if (!card) return;
    var name = credsField(card, '[data-add-name]');
    var secret = credsField(card, '[data-add-secret]');
    if (!name || !secret) {
      credsShowError(card, 'Name and secret are required.');
      return;
    }
    var payload = { provider: provider, name: name, secret: secret };
    var origin = credsField(card, '[data-add-origin]');
    if (origin) payload.origin = origin;
    Enclava.api('POST', '/api/v1/connectors/credentials', payload)
      .then(function () { Enclava.toast('success', 'Credential added'); Enclava.refresh(); })
      .catch(function (err) {
        credsShowError(card, (err && err.message) || 'Adding the credential failed.');
        reportError(err);
      });
  }

  function credsRotate(button) {
    var card = button.closest('[data-cred-provider]');
    var secret = credsField(card, '[data-rotate-secret]');
    if (!secret) {
      credsShowError(card, 'Enter the new secret first.');
      return;
    }
    Enclava.api('PUT', '/api/v1/connectors/credentials/' + encodeURIComponent(button.dataset.credId), { secret: secret })
      .then(function () { Enclava.toast('success', 'Credential rotated'); Enclava.refresh(); })
      .catch(function (err) { credsShowError(card, (err && err.message) || 'Rotating the credential failed.'); reportError(err); });
  }

  function credsDelete(id) {
    Enclava.api('DELETE', '/api/v1/connectors/credentials/' + encodeURIComponent(id))
      .then(function () { Enclava.toast('success', 'Credential deleted'); Enclava.refresh(); })
      .catch(function (err) { reportError(err); });
  }

  function credsOAuthSave(provider) {
    var card = credsCard(provider);
    if (!card) return;
    var payload = {
      client_id: credsField(card, '[data-oauth-client-id]'),
      client_secret: credsField(card, '[data-oauth-client-secret]'),
    };
    if (!payload.client_id || !payload.client_secret) {
      credsShowError(card, 'Client ID and client secret are required.');
      return;
    }
    Enclava.api('PUT', '/api/v1/connectors/oauth-apps/' + encodeURIComponent(provider), payload)
      .then(function () { Enclava.toast('success', 'OAuth app saved'); Enclava.refresh(); })
      .catch(function (err) { credsShowError(card, (err && err.message) || 'Saving the OAuth app failed.'); reportError(err); });
  }

  function credsOAuthRemove(provider) {
    Enclava.api('DELETE', '/api/v1/connectors/oauth-apps/' + encodeURIComponent(provider))
      .then(function () { Enclava.toast('success', 'OAuth app removed'); Enclava.refresh(); })
      .catch(function (err) { reportError(err); });
  }

  // OAuth start is a full-page navigation — the server redirects to the
  // provider and returns to ?connected=/?error= on this tab.
  function credsConnect(provider, button) {
    var card = credsCard(provider);
    var name = credsField(card, '[data-connect-name]');
    if (!name) {
      credsShowError(card, 'Name the credential first.');
      return;
    }
    location.assign('/dashboard/connectors/oauth/start/' + encodeURIComponent(provider) +
      '?name=' + encodeURIComponent(name));
  }
  // ── Events ───────────────────────────────────────────────────────────────


  document.addEventListener('click', function (e) {
    if (!(e.target instanceof Element)) return;
    var trigger = e.target.closest('[data-action^="sources:"], [data-action^="creds:"]');
    if (!trigger) return;
    switch (trigger.dataset.action) {
      case 'sources:provider':
        setProvider(trigger.dataset.provider);
        // Selecting a provider is the step-1 answer — move straight into
        // its Connect form.
        applyStep(2);
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
      case 'creds:add':
        credsAdd(trigger.dataset.provider);
        break;
      case 'creds:rotate':
        credsRotate(trigger);
        break;
      case 'creds:oauth-save':
        credsOAuthSave(trigger.dataset.provider);
        break;
      case 'creds:connect':
        credsConnect(trigger.dataset.provider, trigger);
        break;
    }
  });

  // Destructive confirm (shared `confirm` action group) → delete source.
  document.addEventListener('enclava:confirm', function (e) {
    if (!(e.target instanceof Element)) return;
    if (e.target.dataset.sourceDel === '1' && e.target.dataset.sourceId) {
      deleteSource(e.target.dataset.sourceId);
    }
    if (e.target.dataset.credDel === '1' && e.target.dataset.credId) {
      credsDelete(e.target.dataset.credId);
    }
    if (e.target.dataset.oauthDel === '1' && e.target.dataset.provider) {
      credsOAuthRemove(e.target.dataset.provider);
    }
  });

  // ── Slack channel picker: the chosen credential's member channels ───────

  var SLACK_REF = /^CONNECTOR_SLACK_[A-Z0-9_]+$/;
  var slackChannelsSeq = 0;
  var slackChannelsTimer = null;

  function slackChannelsStatus(text) {
    var status = q('[data-slack-channels-status]');
    if (status) status.textContent = text;
  }

  function loadSlackChannels(ref) {
    var pick = q('[data-slack-channels]');
    if (!pick) return;
    var seq = ++slackChannelsSeq;
    var keep = pick.value;
    while (pick.options.length > 1) pick.remove(1); // option 0: all channels
    if (!SLACK_REF.test(ref)) {
      slackChannelsStatus('Choose a credential to list its channels — the bot only sees channels it was invited to.');
      return;
    }
    slackChannelsStatus('Loading channels…');
    Enclava.api('GET', '/api/v1/connectors/slack/channels?credential_ref=' + encodeURIComponent(ref))
      .then(function (res) {
        if (seq !== slackChannelsSeq) return;
        var channels = (res && res.channels) || [];
        channels.forEach(function (ch) {
          var opt = document.createElement('option');
          opt.value = ch.id;
          opt.textContent = '#' + ch.name + (ch.is_private ? ' (private)' : '');
          pick.appendChild(opt);
        });
        if (keep && Array.prototype.some.call(pick.options, function (o) { return o.value === keep; })) pick.value = keep;
        slackChannelsStatus(channels.length
          ? channels.length + (channels.length === 1 ? ' channel' : ' channels') + ' — invite the bot in Slack to add more.'
          : 'The bot is in no channels yet — invite it to channels in Slack.');
      })
      .catch(function (err) {
        if (seq !== slackChannelsSeq) return;
        slackChannelsStatus('Could not list channels: ' + ((err && err.message) || 'request failed'));
      });
  }

  // Required-field gate: enable Continue only once the visible step is valid.
  document.addEventListener('input', function (e) {
    if (!(e.target instanceof Element)) return;
    var root = drawer();
    if (!root || !root.contains(e.target)) return;
    validateStep(currentStep(), currentProvider());
    if (e.target.matches('[data-provider-fields="slack"] [data-credential]')) {
      var ref = e.target.value.trim();
      clearTimeout(slackChannelsTimer);
      slackChannelsTimer = setTimeout(function () { loadSlackChannels(ref); }, 300);
    }
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
