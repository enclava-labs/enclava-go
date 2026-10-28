/* app.js — the shared dashboard client surface (OA-11, phase 7).
 *
 * window.Enclava is the ONLY global page modules may call:
 *   Enclava.api(method, path, body)   JSON/FormData fetch + X-CSRF-Token from the
 *                                     enclava_csrf cookie; {message, code} rejections;
 *                                     401 → login return_to; 403 → error toast
 *   Enclava.toast(kind, msg, opts?)   one #toast live region; errors persist + action link
 *   Enclava.openDrawer/closeDrawer    shared overlay + focus trap (fetch or inline HTML)
 *   Enclava.openModal/closeModal      same overlay, modal-sized
 *   Enclava.popover(anchor, src?)     #pop card from HTML or a <template> selector
 *   Enclava.refresh()                 dispatches 'refresh' on body (htmx re-fetch)
 *   Enclava.copy(textOrSelector)      clipboard + success toast
 *   Enclava.evidence.open(id)         /dashboard/partials/evidence/{id} → chat panel
 *                                     ≥1280/<760 or the shared #drawer everywhere else
 *   Enclava.evidence.close()
 *
 * CSP-safe: no inline handlers, no eval, no external fetches; htmx is wired through
 * document listeners, never hx-on. All DOM hooks resolve lazily at call time so a
 * later htmx body swap can never leave this module holding stale nodes.
 */
(function () {
  if (window.Enclava) return;

  const el = (id) => document.getElementById(id);

  // The CSRF cookie is script-readable on purpose; mutations echo it back in
  // X-CSRF-Token and the server compares against the stored session token.
  function csrfToken() {
    const match = document.cookie.match(/(?:^|;\s*)enclava_csrf=([^;]*)/);
    return match ? decodeURIComponent(match[1]) : '';
  }

  function loginRedirect() {
    location.assign('/dashboard/login?return_to=' +
      encodeURIComponent(location.pathname + location.search));
  }

  function errorShape(data, status, fallback) {
    const e = (data && (data.error || data)) || {};
    return {
      message: (typeof e.message === 'string' && e.message) || fallback || 'Request failed',
      code: e.code || 'error',
      status: status,
    };
  }

  // api(method, path, body) → Promise<parsed body>. FormData is sent raw (the
  // browser sets its multipart boundary); anything else goes out as JSON.
  async function api(method, path, body) {
    const options = {
      method: method,
      credentials: 'same-origin',
      headers: { 'X-CSRF-Token': csrfToken() },
    };
    if (body instanceof FormData) {
      options.body = body;
    } else if (body !== undefined && body !== null) {
      options.body = JSON.stringify(body);
      options.headers['Content-Type'] = 'application/json';
    }
    let res;
    try {
      res = await fetch(path, options);
    } catch (err) {
      toast('error', 'Network error — try again');
      throw err;
    }
    if (res.status === 401) {
      loginRedirect();
      throw { message: 'Sign in required', code: 'unauthorized', status: 401 };
    }
    let data = null;
    if (res.status !== 204) data = await res.json().catch(() => null);
    if (!res.ok) {
      const err = errorShape(data, res.status);
      if (res.status === 403) toast('error', err.message);
      throw err;
    }
    return data;
  }

  // Shared gate for every HTML fragment load (drawer/modal/evidence): status is
  // resolved BEFORE anything is injected — an expired session never swaps the
  // login document into an overlay, and 403/error bodies never reach the DOM.
  async function fetchHTML(url) {
    let res;
    try {
      res = await fetch(url, {
        credentials: 'same-origin',
        headers: { 'HX-Request': 'true', 'HX-Current-URL': location.href },
      });
    } catch (err) {
      toast('error', 'Network error — try again');
      throw err;
    }
    if (res.status === 401 ||
        (res.redirected && new URL(res.url, location.href).pathname === '/dashboard/login')) {
      loginRedirect();
      throw { message: 'Sign in required', code: 'unauthorized', status: 401 };
    }
    if (res.status === 403) {
      const data = await res.json().catch(() => null);
      const err = errorShape(data, 403, "You don't have access to do that");
      toast('error', err.message);
      throw err;
    }
    const type = res.headers.get('Content-Type') || '';
    if (res.ok && type.includes('text/html')) return res.text();
    const err = errorShape(null, res.status, 'That request failed');
    toast('error', err.message);
    throw err;
  }

  let toastTimer = null;
  function toast(kind, msg, opts) {
    const t = el('toast');
    if (!t) return;
    clearTimeout(toastTimer);
    t.classList.remove('show', 'success', 'info', 'error');
    t.textContent = '';
    t.classList.add('show');
    if (kind) t.classList.add(kind);
    const span = document.createElement('span');
    span.textContent = msg || '';
    t.appendChild(span);
    if (kind === 'error') {
      const o = opts || {};
      if (o.action && o.action.label && o.action.href) {
        const a = document.createElement('a');
        a.href = o.action.href;
        a.textContent = o.action.label;
        t.appendChild(a);
      }
      const close = document.createElement('button');
      close.type = 'button';
      close.className = 'iconbtn';
      close.setAttribute('aria-label', 'Dismiss notification');
      close.textContent = '×';
      close.addEventListener('click', () => {
        clearTimeout(toastTimer);
        t.classList.remove('show');
        t.textContent = '';
      });
      t.appendChild(close);
    } else {
      toastTimer = setTimeout(() => {
        t.classList.remove('show');
        t.textContent = '';
      }, 4000);
    }
  }

  function refresh() {
    document.body.dispatchEvent(new Event('refresh'));
  }

  async function copy(target) {
    let text = target;
    if (typeof target === 'string' && /^[.#]/.test(target)) {
      const node = document.querySelector(target);
      if (node) text = node.value !== undefined && node.value !== '' ? node.value : node.textContent;
    }
    try {
      await navigator.clipboard.writeText(text);
    } catch (_) {
      const ta = document.createElement('textarea');
      ta.value = text;
      ta.className = 'visually-hidden';
      document.body.appendChild(ta);
      ta.select();
      try { document.execCommand('copy'); } finally { ta.remove(); }
    }
    toast('success', 'Copied to clipboard');
  }

  // ── Shared overlay machinery (drawer + modal): focus trap, Esc/backdrop ──

  let overlayOpener = null;

  function drawerOpen() {
    const d = el('drawer');
    return !!(d && d.classList.contains('open'));
  }

  function visibleFocusables(root) {
    if (!root) return [];
    return Array.prototype.slice.call(root.querySelectorAll(
      'a[href], button:not([disabled]), input:not([disabled]):not([type="hidden"]), ' +
      'select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])'
    )).filter((n) => !n.closest('[hidden]') &&
      (n.offsetWidth || n.offsetHeight || n.getClientRects().length));
  }

  function showOverlay(html, modal) {
    const drawer = el('drawer');
    if (!drawer) return;
    closePopover();
    drawer.innerHTML = html;
    drawer.setAttribute('role', 'dialog');
    drawer.setAttribute('aria-modal', 'true');
    drawer.setAttribute('tabindex', '-1');
    drawer.classList.toggle('modal', !!modal);
    drawer.classList.add('open');
    const backdrop = el('backdrop');
    if (backdrop) backdrop.classList.add('open');
    overlayOpener = document.activeElement instanceof Element ? document.activeElement : null;
    const first = visibleFocusables(drawer)[0];
    (first || drawer).focus();
  }

  function closeDrawer() {
    const drawer = el('drawer');
    if (!drawer || !drawer.classList.contains('open')) return;
    drawer.classList.remove('open', 'modal');
    drawer.innerHTML = '';
    const backdrop = el('backdrop');
    if (backdrop) backdrop.classList.remove('open');
    if (overlayOpener && document.contains(overlayOpener)) overlayOpener.focus();
    overlayOpener = null;
  }

  async function openDrawer(urlOrHTML) {
    const html = typeof urlOrHTML === 'string' && urlOrHTML.charAt(0) === '<'
      ? urlOrHTML
      : await fetchHTML(urlOrHTML);
    showOverlay(html, false);
  }

  function openModal(html) {
    showOverlay(html, true);
  }

  // ── Popovers: one open at a time, viewport-clamped, aria-expanded mirroring ──

  let popAnchorEl = null;

  function popoverOpen() {
    const p = el('pop');
    return !!(p && !p.hidden);
  }

  function closePopover() {
    const p = el('pop');
    if (!p || p.hidden) return;
    p.hidden = true;
    p.innerHTML = '';
    p.style.left = '';
    p.style.top = '';
    if (popAnchorEl) {
      popAnchorEl.setAttribute('aria-expanded', 'false');
      popAnchorEl = null;
    }
  }

  function popover(anchor, source) {
    const pop = el('pop');
    if (!pop) return;
    closePopover();
    let html = source;
    if (typeof source === 'string' && source.charAt(0) === '#') {
      const tpl = document.querySelector(source);
      if (tpl && tpl.content) {
        const holder = document.createElement('div');
        holder.appendChild(tpl.content.cloneNode(true));
        html = holder.innerHTML;
      }
    }
    pop.innerHTML = html;
    pop.hidden = false;
    const rect = anchor && anchor.getBoundingClientRect ? anchor.getBoundingClientRect() : { left: 20, top: 20, bottom: 60 };
    const left = Math.min(Math.max(12, rect.left), Math.max(12, window.innerWidth - pop.offsetWidth - 12));
    let top = rect.bottom + 8;
    if (top + pop.offsetHeight > window.innerHeight - 12) top = Math.max(12, rect.top - pop.offsetHeight - 8);
    pop.style.left = left + 'px';
    pop.style.top = top + 'px';
    if (anchor && anchor.tagName === 'BUTTON') anchor.setAttribute('aria-expanded', 'true');
    popAnchorEl = anchor || null;
    const first = visibleFocusables(pop)[0];
    if (first) first.focus();
  }

  // ── Evidence: shared routing — chat's own column ≥1280 / sheet <760, the ──
  // shared #drawer for everything else (search included at every width).

  async function openEvidence(id, opts) {
    if (!id) return;
    const o = opts || {};
    const panel = o.container
      ? document.querySelector(o.container)
      : document.querySelector('.evidence-panel');
    const wide = matchMedia('(min-width:1280px)').matches;
    const narrow = matchMedia('(max-width:759px)').matches;
    const html = await fetchHTML('/dashboard/partials/evidence/' + encodeURIComponent(id));
    if (panel && (wide || narrow)) {
      panel.innerHTML = html;
    } else {
      showOverlay(html, false);
    }
  }

  function closeEvidence() {
    document.querySelectorAll('.evidence-panel').forEach((p) => { p.innerHTML = ''; });
    closeDrawer();
  }

  // ── Delegated data-action dispatch (shared actions; page modules namespace ──
  // their own listeners — evidence:* stays shared here for phase-10 producers).

  function armConfirm(trigger) {
    if (trigger.dataset.armed) {
      clearTimeout(Number(trigger.dataset.confirmTimer));
      delete trigger.dataset.armed;
      if (trigger.dataset.confirmLabel !== undefined) trigger.textContent = trigger.dataset.confirmLabel;
      const href = trigger.dataset.confirmHref || '';
      trigger.dispatchEvent(new CustomEvent('enclava:confirm', { bubbles: true, detail: { href: href } }));
      if (href) location.assign(href);
      return;
    }
    trigger.dataset.armed = '1';
    trigger.dataset.confirmLabel = trigger.textContent;
    trigger.textContent = trigger.dataset.confirm || 'Confirm?';
    trigger.dataset.confirmTimer = String(setTimeout(() => {
      if (!trigger.dataset.armed) return;
      delete trigger.dataset.armed;
      if (trigger.dataset.confirmLabel !== undefined) trigger.textContent = trigger.dataset.confirmLabel;
    }, 3000));
  }

  document.addEventListener('click', (e) => {
    if (!(e.target instanceof Element)) return;
    if (e.target.id === 'backdrop') { closeDrawer(); return; }
    if (e.target.closest('.mobile-chats')) {
      const list = document.querySelector('.mobile-chats-list');
      if (list) list.classList.toggle('open');
    }
    const pop = el('pop');
    if (pop && !pop.hidden && !pop.contains(e.target) &&
        !e.target.closest('[data-action="user-menu"],[data-action="conf-pop"]')) {
      closePopover();
    }
    const trigger = e.target.closest('[data-action]');
    if (!trigger) return;
    switch (trigger.dataset.action) {
      case 'open-drawer':
        openDrawer(trigger.dataset.drawerUrl || '');
        break;
      case 'close-drawer':
        closeDrawer();
        break;
      case 'toggle-nav': {
        document.body.classList.toggle('mobile-open');
        const menu = document.querySelector('.mobile-menu');
        if (menu) menu.setAttribute('aria-expanded',
          document.body.classList.contains('mobile-open') ? 'true' : 'false');
        break;
      }
      case 'desktop-collapse': {
        const collapsed = document.body.classList.toggle('collapsed');
        try { localStorage.setItem('enclava-nav-collapsed', collapsed ? '1' : '0'); } catch (_) {}
        break;
      }
      case 'user-menu':
      case 'conf-pop':
        if (popAnchorEl === trigger && popoverOpen()) closePopover();
        else popover(trigger, trigger.dataset.action === 'user-menu' ? '#user-menu-tpl' : '#conf-pop-tpl');
        break;
      case 'theme': {
        const light = document.body.classList.toggle('light');
        // The server reads this cookie to emit <body class="light"> on the next
        // render; app.js already flipped the class for the current page.
        document.cookie = 'enclava_theme=' + (light ? 'light' : 'dark') + ';path=/;SameSite=Lax';
        break;
      }
      case 'copy':
        copy(trigger.dataset.copy || '');
        break;
      case 'confirm':
        armConfirm(trigger);
        break;
      case 'evidence:open':
        openEvidence(trigger.dataset.evidenceId);
        break;
      case 'evidence:close':
        closeEvidence();
        break;
    }
  });

  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') {
      if (popoverOpen()) { closePopover(); return; }
      if (drawerOpen()) closeDrawer();
      return;
    }
    if (e.key !== 'Tab' || !drawerOpen()) return;
    const drawer = el('drawer');
    const focusables = visibleFocusables(drawer);
    if (!focusables.length) { e.preventDefault(); drawer.focus(); return; }
    const first = focusables[0];
    const last = focusables[focusables.length - 1];
    const active = document.activeElement;
    if (!drawer.contains(active)) { e.preventDefault(); first.focus(); }
    else if (e.shiftKey && active === first) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && active === last) { e.preventDefault(); first.focus(); }
  });

  // ── htmx wiring: document listeners only (D-20 — no hx-on, no inline) ──

  function htmxVerb(elt) {
    if (!elt || !elt.getAttribute) return 'get';
    for (const attr of ['hx-post', 'hx-put', 'hx-patch', 'hx-delete']) {
      if (elt.getAttribute(attr) !== null) return attr.slice(3);
    }
    return 'get';
  }

  document.addEventListener('htmx:configRequest', (e) => {
    if (htmxVerb(e.detail && e.detail.elt) === 'get') return;
    e.detail.headers = e.detail.headers || {};
    e.detail.headers['X-CSRF-Token'] = csrfToken();
  });

  document.addEventListener('htmx:responseError', (e) => {
    const xhr = e.detail && e.detail.xhr;
    if (!xhr) return;
    const redirect = xhr.getResponseHeader('HX-Redirect');
    if (redirect) { location.assign(redirect); return; }
    if (xhr.status === 401) { loginRedirect(); return; }
    if (xhr.status === 403) { toast('error', "You don't have access to do that"); return; }
    toast('error', 'That request failed');
  });
  document.addEventListener('htmx:sendError', () => toast('error', 'Network error — try again'));
  document.addEventListener('htmx:timeout', () => toast('error', 'Network error — try again'));

  document.addEventListener('htmx:afterSwap', (e) => {
    const target = e.detail && e.detail.target;
    if (!target || !target.matches) return;
    const thread = target.matches('.thread, [data-scroll-latest]')
      ? target
      : target.querySelector('.thread, [data-scroll-latest]');
    if (thread) thread.scrollTop = thread.scrollHeight;
  });

  // Restore the persisted desktop rail (≥760 only — <760 is the mobile drawer).
  try {
    if (localStorage.getItem('enclava-nav-collapsed') === '1' &&
        matchMedia('(min-width:760px)').matches) {
      document.body.classList.add('collapsed');
    }
  } catch (_) {}
  const initialPop = el('pop');
  if (initialPop) initialPop.hidden = true;

  window.Enclava = {
    api: api,
    toast: toast,
    openDrawer: openDrawer,
    closeDrawer: closeDrawer,
    openModal: openModal,
    closeModal: closeDrawer,
    popover: popover,
    refresh: refresh,
    copy: copy,
    evidence: { open: openEvidence, close: closeEvidence },
  };
  window.Enclava.version = 'oa';
})();
