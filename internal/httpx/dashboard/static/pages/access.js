/* access.js — Access page module (09-07, OA-28).
 *
 * Every write goes through Enclava.api to the real /api/v1 producers (D-17):
 *   people   POST /api/v1/identities/users · PUT|DELETE …/users/{id}/roles/{roleID}
 *            POST …/users/{id}/password · PATCH …/users/{id} (status) ·
 *            DELETE …/users/{id}/sessions/{sid} · DELETE …/users/{id}
 *   roles    POST /api/v1/identities/roles · PATCH|DELETE …/roles/{id} — the
 *            PATCH/POST body always carries all four grant kinds in the
 *            canonical stored shape {"mode":"all"}/{"mode":"list","items":[…]}
 *            (absent/empty denies; no "*" sentinels, no {all,items} conversion)
 *   accounts POST /api/v1/identities/service-accounts · PATCH …/{id} (status)
 *   keys     POST /api/v1/admin/api-keys (scopes ALWAYS submitted) ·
 *            POST …/{id}/regenerate (rotate) · DELETE …/{id} (revoke)
 * Destructive controls arm through the shared data-confirm flow and finish on
 * the `enclava:confirm` event; errors surface their body message (403s toast
 * inside Enclava.api).
 */
(function () {
  'use strict';

  const Enclava = window.Enclava;

  function checkedValues(scope, name) {
    return Array.prototype.slice
      .call(scope.querySelectorAll('input[name="' + name + '"]:checked'))
      .map((i) => i.value);
  }

  // grantDoc serializes one picker fieldset into the canonical stored
  // Allowlist document — the exact shape the roles API returns and accepts.
  function grantDoc(section) {
    const all = section.querySelector('[data-grant-all]');
    if (all && all.checked) return { mode: 'all' };
    return {
      mode: 'list',
      items: Array.prototype.slice
        .call(section.querySelectorAll('[data-grant-item]:checked'))
        .map((i) => i.value),
    };
  }

  function intOrNull(value) {
    const v = String(value || '').trim();
    if (!v) return null;
    const n = Number(v);
    return Number.isFinite(n) && n > 0 ? Math.floor(n) : null;
  }

  // report routes every failure through the shared error→action map (11-03):
  // the engine toasts the mapped sentence + fix link (or a retry affordance)
  // and already handles 401/403; role_has_members lands here as a 409 whose
  // API message explains members must be moved first — sentence only.
  function report(err) {
    if (err && (err.status === 403 || err.status === 401)) return;
    Enclava.toast('error', (err && err.message) || 'That request failed', {
      action: Enclava.errorAction(err),
    });
  }

  function rerun(fn) {
    return fn().then(
      (v) => v,
      (err) => { report(err); throw err; },
    );
  }

  // ── secret-once presentation (create + rotate): DOM-built, copy button ──

  function showSecretOnce(container, secret) {
    if (!container || !secret) return;
    container.hidden = false;
    while (container.firstChild) container.removeChild(container.firstChild);
    const warn = document.createElement('p');
    warn.className = 'help';
    warn.textContent = 'Copy the secret now — it is shown only once.';
    const code = document.createElement('code');
    code.className = 'field';
    code.style.wordBreak = 'break-all';
    code.textContent = secret;
    const row = document.createElement('div');
    row.className = 'row';
    const copy = document.createElement('button');
    copy.type = 'button';
    copy.className = 'btn';
    copy.textContent = 'Copy secret';
    copy.addEventListener('click', () => Enclava.copy(secret));
    row.appendChild(code);
    row.appendChild(copy);
    container.appendChild(warn);
    container.appendChild(row);
  }

  function secretModal(secret, heading) {
    const holder = document.createElement('div');
    const head = document.createElement('div');
    head.className = 'drawer-head';
    const title = document.createElement('h2');
    title.textContent = heading;
    head.appendChild(title);
    const body = document.createElement('div');
    body.className = 'stack';
    const warn = document.createElement('p');
    warn.className = 'help';
    warn.textContent = 'Copy the secret now — it is shown only once.';
    const code = document.createElement('code');
    code.className = 'field';
    code.style.wordBreak = 'break-all';
    code.textContent = secret;
    const row = document.createElement('div');
    row.className = 'row';
    const copy = document.createElement('button');
    copy.type = 'button';
    copy.className = 'btn';
    copy.textContent = 'Copy secret';
    copy.addEventListener('click', () => Enclava.copy(secret));
    const done = document.createElement('button');
    done.type = 'button';
    done.className = 'btn primary';
    done.textContent = 'Done';
    done.addEventListener('click', () => Enclava.closeModal());
    row.appendChild(copy);
    row.appendChild(done);
    body.appendChild(warn);
    body.appendChild(code);
    body.appendChild(row);
    holder.appendChild(head);
    holder.appendChild(body);
    return holder.innerHTML;
  }

  // ── drawer forms ──

  function submitUserCreate(form) {
    const body = {
      email: form.elements.email.value.trim(),
      display_name: form.elements.display_name.value.trim(),
      password: form.elements.password.value,
      role_ids: checkedValues(form, 'role'),
    };
    return Enclava.api('POST', '/api/v1/identities/users', body).then(() => {
      Enclava.toast('success', 'Invite created');
      Enclava.closeDrawer();
      Enclava.refresh();
    });
  }

  // Role changes emit one bounded identities subroute per delta; a full save
  // then re-renders the drawer from the server's answer.
  function submitUserRoles(form) {
    const userID = form.dataset.userId;
    const boxes = Array.prototype.slice.call(form.querySelectorAll('input[name="role"]'));
    const steps = [];
    boxes.forEach((box) => {
      const assigned = box.dataset.assigned === '1';
      if (box.checked && !assigned) {
        steps.push(() => Enclava.api('PUT', '/api/v1/identities/users/' + userID + '/roles/' + box.value, {}));
      } else if (!box.checked && assigned) {
        steps.push(() => Enclava.api('DELETE', '/api/v1/identities/users/' + userID + '/roles/' + box.value));
      }
    });
    return steps
      .reduce((p, step) => p.then(step), Promise.resolve())
      .then(() => {
        Enclava.toast('success', 'Roles saved');
        Enclava.openDrawer('/dashboard/partials/access/user-drawer?id=' + encodeURIComponent(userID));
        Enclava.refresh();
      });
  }

  function submitUserPassword(form) {
    const body = { password: form.elements.password.value };
    return Enclava.api('POST', '/api/v1/identities/users/' + form.dataset.userId + '/password', body).then(() => {
      Enclava.toast('success', 'Password updated');
      form.reset();
    });
  }

  // The editor always sends every grant kind explicitly: an explicit empty
  // list clears (deny), an omitted field would leave the stored grant alone.
  // Two sections may share one kind (Models + Embedding models both write
  // grants.models): merge — "all" wins, items union — never last-wins.
  function mergeGrantDoc(doc, incoming) {
    if (!doc || incoming.mode === 'all') return incoming;
    if (doc.mode === 'all') return doc;
    return { mode: 'list', items: Array.from(new Set(doc.items.concat(incoming.items))) };
  }

  function submitRole(form) {
    const grants = {};
    Array.prototype.slice.call(form.querySelectorAll('fieldset[data-grant]')).forEach((section) => {
      const kind = section.dataset.grant;
      grants[kind] = mergeGrantDoc(grants[kind], grantDoc(section));
    });
    const body = {
      name: form.elements.name.value.trim(),
      scopes: checkedValues(form, 'scope'),
      grants: grants,
    };
    const roleID = form.dataset.roleId;
    const request = roleID
      ? Enclava.api('PATCH', '/api/v1/identities/roles/' + roleID, body)
      : Enclava.api('POST', '/api/v1/identities/roles', body);
    return request.then(() => {
      Enclava.toast('success', roleID ? 'Role saved' : 'Role created');
      Enclava.closeDrawer();
      Enclava.refresh();
    });
  }

  function submitServiceAccount(form) {
    const body = {
      name: form.elements.name.value.trim(),
      scopes: checkedValues(form, 'scope'),
    };
    return Enclava.api('POST', '/api/v1/identities/service-accounts', body).then(() => {
      Enclava.toast('success', 'Service account created');
      form.reset();
      Enclava.refresh();
    });
  }

  // Scopes go out even when empty — omitted grant fields would widen (403).
  function submitKey(form) {
    const body = {
      name: form.elements.name.value.trim(),
      scopes: checkedValues(form, 'scope'),
    };
    const owner = form.elements.owner.value;
    if (owner.indexOf('user:') === 0) body.owner_user_id = owner.slice(5);
    else if (owner.indexOf('service_account:') === 0) body.service_account_id = owner.slice(16);
    const expires = form.elements.expires.value;
    if (expires) {
      const d = new Date(expires);
      if (!Number.isNaN(d.getTime())) body.expires_at = d.toISOString();
    }
    ['rate_limit_per_minute', 'rate_limit_per_hour', 'rate_limit_per_day'].forEach((field) => {
      const n = intOrNull(form.elements[field].value);
      if (n !== null) body[field] = n;
    });
    return Enclava.api('POST', '/api/v1/admin/api-keys', body).then((res) => {
      Enclava.toast('success', 'Key created');
      showSecretOnce(form.querySelector('[data-key-result]'), res && res.secret_key);
      Enclava.refresh();
    });
  }

  const forms = {
    'access-user-create': submitUserCreate,
    'access-user-roles': submitUserRoles,
    'access-user-password': submitUserPassword,
    'access-role': submitRole,
    'access-sa': submitServiceAccount,
    'access-key': submitKey,
  };

  document.addEventListener('submit', (e) => {
    const form = e.target;
    if (!(form instanceof HTMLFormElement)) return;
    const handler = forms[form.dataset.form];
    if (!handler) return;
    e.preventDefault();
    rerun(() => handler(form));
  });

  // ── armed destructive + rotate actions (shared data-confirm flow) ──

  document.addEventListener('enclava:confirm', (e) => {
    const trigger = e.target;
    if (!(trigger instanceof Element)) return;
    const kind = trigger.dataset.accessConfirm;
    if (!kind) return;
    const after = () => Enclava.refresh();
    switch (kind) {
      case 'session':
        rerun(() => Enclava.api(
          'DELETE',
          '/api/v1/identities/users/' + encodeURIComponent(trigger.dataset.userId) +
            '/sessions/' + encodeURIComponent(trigger.dataset.sessionId),
        )).then(() => {
          Enclava.toast('success', 'Session revoked');
          return Enclava.openDrawer('/dashboard/partials/access/user-drawer?id=' +
            encodeURIComponent(trigger.dataset.userId));
        }).catch(() => {});
        break;
      case 'user-status':
        rerun(() => Enclava.api(
          'PATCH',
          '/api/v1/identities/users/' + encodeURIComponent(trigger.dataset.userId),
          { status: trigger.dataset.status },
        )).then(() => {
          Enclava.toast('success', trigger.dataset.status === 'disabled' ? 'Person disabled' : 'Person enabled');
          Enclava.refresh();
          return Enclava.openDrawer('/dashboard/partials/access/user-drawer?id=' +
            encodeURIComponent(trigger.dataset.userId));
        }).catch(() => {});
        break;
      case 'user-delete':
        rerun(() => Enclava.api(
          'DELETE',
          '/api/v1/identities/users/' + encodeURIComponent(trigger.dataset.userId),
        )).then(() => {
          Enclava.toast('success', 'Person deleted');
          Enclava.closeDrawer();
          after();
        }).catch(() => {});
        break;
      case 'role-delete':
        rerun(() => Enclava.api(
          'DELETE',
          '/api/v1/identities/roles/' + encodeURIComponent(trigger.dataset.roleId),
        )).then(() => {
          Enclava.toast('success', 'Role deleted');
          after();
        }).catch(() => {});
        break;
      case 'key-rotate':
        rerun(() => Enclava.api(
          'POST',
          '/api/v1/admin/api-keys/' + encodeURIComponent(trigger.dataset.keyId) + '/regenerate',
        )).then((res) => {
          Enclava.openModal(secretModal(res && res.secret_key, 'Key rotated'));
          after();
        }).catch(() => {});
        break;
      case 'key-revoke':
        rerun(() => Enclava.api(
          'DELETE',
          '/api/v1/admin/api-keys/' + encodeURIComponent(trigger.dataset.keyId),
        )).then(() => {
          Enclava.toast('success', 'Key revoked');
          after();
        }).catch(() => {});
        break;
      case 'sa-disable':
        rerun(() => Enclava.api(
          'PATCH',
          '/api/v1/identities/service-accounts/' + encodeURIComponent(trigger.dataset.saId),
          { status: 'disabled' },
        )).then(() => {
          Enclava.toast('success', 'Service account disabled');
          after();
        }).catch(() => {});
        break;
    }
  });

  // Toggling a section's All makes its item list irrelevant; disable the
  // items so the rendered state never disagrees with the serialized one.
  document.addEventListener('change', (e) => {
    const input = e.target;
    if (!(input instanceof HTMLInputElement) || !input.hasAttribute('data-grant-all')) return;
    const section = input.closest('fieldset[data-grant]');
    if (!section) return;
    section.querySelectorAll('[data-grant-item]').forEach((item) => { item.disabled = input.checked; });
  });

  // Deep links: /dashboard/access/<tab>?new=1 opens that tab's create drawer
  // once per load — the empty-state actions point here (managers only; the
  // whole page sits behind access.manage).
  (function () {
    if (new URLSearchParams(location.search).get('new') !== '1') return;
    var drawers = {
      people: '/dashboard/partials/access/user-drawer',
      roles: '/dashboard/partials/access/role-editor',
      'api-keys': '/dashboard/partials/access/key-drawer',
    };
    var tab = location.pathname.split('/').pop();
    if (tab === 'access') tab = 'people';
    if (drawers[tab]) Enclava.openDrawer(drawers[tab]);
  })();
})();
