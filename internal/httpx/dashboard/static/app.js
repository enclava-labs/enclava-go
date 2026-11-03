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
  // api(method, path, body, opts?) → Promise<parsed body>. FormData is sent
  // raw (the browser sets its multipart boundary); anything else goes out as
  // JSON. Every rejection carries err.request={method,path,body} so the error
  // map can offer an in-place replay; opts.onRetry (async () => void) rides
  // along and, when present, REPLACES the blind replay — the caller resumes
  // its own success continuation (create→sync, preview→poll, create→nav).
  async function api(method, path, body, opts) {
    const request = { method: method, path: path, body: body };
    const onRetry = opts && typeof opts.onRetry === 'function' ? opts.onRetry : null;
    const reject = (err) => { err.request = request; err.onRetry = onRetry; return err; };
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
      throw reject(Object.assign(err instanceof Error ? err : new Error('network'), { message: 'Network error — try again', code: 'network_error' }));
    }
    if (res.status === 401) {
      loginRedirect();
      throw reject({ message: 'Sign in required', code: 'unauthorized', status: 401 });
    }
    let data = null;
    if (res.status !== 204) data = await res.json().catch(() => null);
    if (!res.ok) {
      const err = errorShape(data, res.status);
      if (res.status === 403) toast('error', err.message);
      throw reject(err);
    }
    return data;
  }

  // ── Capability projection + error→next-step map (OA-35, CONTEXT 9/10) ──

  // Enclava.caps is the server-emitted capability projection, parsed once.
  // Both serializations are accepted (JSON booleans from dashboardCapsJSON,
  // or the space-separated name list) — absent/unparseable fails closed so a
  // member never gets a link that lands on a 403.
  const caps = (function () {
    const raw = document.body ? document.body.dataset.caps : '';
    if (!raw) return {};
    if (raw.charAt(0) === '{') {
      try { return JSON.parse(raw) || {}; } catch (_) { return {}; }
    }
    const out = {};
    raw.split(/\s+/).forEach((name) => { if (name) out[name] = true; });
    return out;
  })();

  // errorAction(errOrPayload, sourceID?) → {text, label?, href?} |
  // {text, label, retry, request?, onRetry?} | {text}. It accepts the
  // {message, code} envelope api() rejects with AND persisted failure
  // payloads from a 200 GET (run.error / job.error_message — callers pass
  // {message: run.error} or {message: job.error_message, status}). Gated
  // links require the matching capability; without it the row degrades to
  // the sentence plus the administrator fallback — never a 403-bound href.
  function errorAction(err, sourceID) {
    const e = err || {};
    let message = (typeof e.message === 'string' && e.message) ||
      (typeof e.error === 'string' && e.error) ||
      (typeof e.error_message === 'string' && e.error_message) || '';
    let code = typeof e.code === 'string' ? e.code : '';
    const lower = message.toLowerCase();
    if (!code) {
      if (lower.indexOf('required price missing') !== -1) code = 'price_missing';
      else if (lower.indexOf('budget exceeded') !== -1) code = 'budget_exceeded';
      else if (lower.indexOf('credential') !== -1 || lower.indexOf('authorization') !== -1 ||
               lower.indexOf('auth failed') !== -1) code = 'source_auth_failed';
    }
    const adminFallback = ' — your administrator can fix this.';
    switch (code) {
      case 'price_missing': {
        const text = "The model isn't priced for every unit this request bills";
        if (caps['models.manage']) {
          return { text: text + '.', label: 'Set price', href: '/dashboard/models/pricing' };
        }
        return { text: text + adminFallback };
      }
      case 'source_auth_failed': {
        const text = "The source's credentials were rejected — reconnect it to resume syncing";
        if (caps['sources.manage']) {
          const href = sourceID
            ? '/dashboard/sources?edit=' + encodeURIComponent(sourceID)
            : '/dashboard/sources';
          return { text: text + '.', label: 'Reconnect', href: href };
        }
        return { text: text + adminFallback };
      }
      case 'budget_exceeded': {
        const text = 'This request exceeds the configured budget';
        if (caps['billing.view']) {
          return { text: text + '.', label: 'View budgets', href: '/dashboard/usage/budgets' };
        }
        return { text: text + adminFallback };
      }
      case 'rate_limited':
      case 'overloaded':
      case 'gateway_timeout':
      case 'upstream_unavailable':
      case 'network_error': {
        const action = { text: "That request couldn't complete — try again.", label: 'Try again', retry: true };
        if (e.request) action.request = e.request;
        if (e.onRetry) action.onRetry = e.onRetry;
        return action;
      }
      case 'schema_changed':
      case 'conflict':
        return { text: "The tool's schema changed — refresh to see the latest before approving." };
      case 'forbidden':
      case 'agent_run_denied':
      case 'invalid_api_key':
      case 'unauthorized':
        return { text: message || "You don't have access to do that — check with your administrator if you should." };
      case 'not_found':
        return { text: message || 'That item is no longer available.' };
    }
    if (lower.indexOf('schema') !== -1) {
      return { text: "The tool's schema changed — refresh to see the latest before approving." };
    }
    return { text: message || 'That request failed' };
  }

  const POLL_MS = 3000; // the one client poll interval (htmx uses every 3s)

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
    if (kind === 'error') t.setAttribute('role', 'alert');
    else t.removeAttribute('role');
    const span = document.createElement('span');
    span.textContent = msg || '';
    t.appendChild(span);
    if (kind === 'error') {
      const o = opts || {};
      // opts.action accepts the legacy {label, href} link AND an
      // Enclava.errorAction result {label, href} | {label, retry, request?,
      // onRetry?} — retry renders a button that replays the captured request
      // (or runs the caller's onRetry continuation) then refreshes.
      const action = o.action || null;
      if (action && action.href && (action.label || action.text)) {
        const a = document.createElement('a');
        a.href = action.href;
        a.textContent = action.label || action.text;
        t.appendChild(a);
      } else if (action && action.retry) {
        const b = document.createElement('button');
        b.type = 'button';
        b.className = 'btn';
        b.textContent = action.label || 'Try again';
        b.addEventListener('click', async () => {
          b.disabled = true;
          try {
            if (typeof action.onRetry === 'function') await action.onRetry();
            else if (action.request) await api(action.request.method, action.request.path, action.request.body);
            t.classList.remove('show');
            t.textContent = '';
            refresh();
          } catch (_) { b.disabled = false; }
        });
        t.appendChild(b);
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

  // ── Shared overlay machinery (drawer + modal + popover + mobile nav/ ──
  // chats): focus trap, Esc/backdrop close, focus returns to the opener,
  let overlayOpener = null;

  // restoreFocus returns focus to the opener when the released overlay still
  // held it — a click that already moved focus elsewhere is never stolen.
  function restoreFocus(opener, hadFocus) {
    if (opener && document.contains(opener) &&
        (hadFocus || document.activeElement === document.body)) {
      opener.focus();
    }
  }

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
    closeMobileNav(false);
    closeMobileChats(false);
    drawer.innerHTML = html;
    // role=dialog + aria-modal exist only while the drawer is open (UX-H05):
    // a closed, off-canvas aside is not a dialog landmark.
    drawer.hidden = false;
    drawer.setAttribute('role', 'dialog');
    drawer.setAttribute('aria-modal', 'true');
    const title = drawer.querySelector('h2');
    if (title) {
      if (!title.id) title.id = 'drawer-title';
      drawer.setAttribute('aria-labelledby', title.id);
    } else {
      drawer.removeAttribute('aria-labelledby');
    }
    drawer.setAttribute('tabindex', '-1');
    drawer.classList.toggle('modal', !!modal);
    // A frame must pass before 'open' lands or the transform transition is
    // lost on the first paint after unhiding.
    requestAnimationFrame(() => { if (!drawer.hidden) drawer.classList.add('open'); });
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
    drawer.removeAttribute('role');
    drawer.removeAttribute('aria-modal');
    drawer.removeAttribute('aria-labelledby');
    // hidden lands after the slide-out transition; a reopen inside the
    // window un-hides the drawer and this guard leaves it alone.
    setTimeout(() => { if (!drawer.classList.contains('open')) drawer.hidden = true; }, 250);
    const backdrop = el('backdrop');
    if (backdrop) backdrop.classList.remove('open');
    if (overlayOpener && document.contains(overlayOpener)) overlayOpener.focus();
    overlayOpener = null;
  }

  async function openDrawer(urlOrHTML) {
    // Leading whitespace (templated markup) must not defeat the inline-HTML
    // sniff — otherwise the markup is fetched as a URL.
    const html = typeof urlOrHTML === 'string' && urlOrHTML.trim().charAt(0) === '<'
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

  function closePopover(restore) {
    const p = el('pop');
    if (!p || p.hidden) return;
    const hadFocus = p.contains(document.activeElement);
    p.hidden = true;
    p.innerHTML = '';
    p.style.left = '';
    p.style.top = '';
    if (popAnchorEl) {
      popAnchorEl.setAttribute('aria-expanded', 'false');
      if (restore !== false) restoreFocus(popAnchorEl, hadFocus);
      popAnchorEl = null;
    }
  }

  function popover(anchor, source) {
    const pop = el('pop');
    closePopover();
    closeDrawer();
    closeMobileNav(false);
    closeMobileChats(false);
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

  // ── Mobile overlays (<760): the sidebar-as-drawer nav and the chat ──
  // history sheet get the same trap/Esc/outside-close/return treatment.

  let mobileNavOpener = null;
  let mobileChatsOpener = null;

  function mobileNavOpen() {
    return document.body.classList.contains('mobile-open');
  }

  function openMobileNav(opener) {
    if (mobileNavOpen()) return;
    closePopover();
    closeDrawer();
    closeMobileChats(false);
    mobileNavOpener = opener || document.querySelector('.mobile-menu');
    document.body.classList.add('mobile-open');
    const menu = document.querySelector('.mobile-menu');
    if (menu) menu.setAttribute('aria-expanded', 'true');
    const first = visibleFocusables(document.querySelector('.sidebar'))[0];
    if (first) first.focus();
  }

  function closeMobileNav(restore) {
    if (!mobileNavOpen()) return;
    const hadFocus = !!document.querySelector('.sidebar') &&
      document.querySelector('.sidebar').contains(document.activeElement);
    document.body.classList.remove('mobile-open');
    const menu = document.querySelector('.mobile-menu');
    if (menu) menu.setAttribute('aria-expanded', 'false');
    if (restore !== false) restoreFocus(mobileNavOpener, hadFocus);
    mobileNavOpener = null;
  }

  function mobileChatsOpen() {
    const l = document.querySelector('.mobile-chats-list');
    return !!(l && l.classList.contains('open'));
  }

  function openMobileChats(opener) {
    const list = document.querySelector('.mobile-chats-list');
    if (!list || list.classList.contains('open')) return;
    closePopover();
    closeDrawer();
    closeMobileNav(false);
    mobileChatsOpener = opener || null;
    list.classList.add('open');
    if (mobileChatsOpener) mobileChatsOpener.setAttribute('aria-expanded', 'true');
    const first = visibleFocusables(list)[0];
    if (first) first.focus();
  }

  function closeMobileChats(restore) {
    const list = document.querySelector('.mobile-chats-list');
    if (!list || !list.classList.contains('open')) return;
    const hadFocus = list.contains(document.activeElement);
    list.classList.remove('open');
    if (mobileChatsOpener) mobileChatsOpener.setAttribute('aria-expanded', 'false');
    if (restore !== false) restoreFocus(mobileChatsOpener, hadFocus);
    mobileChatsOpener = null;
  }

  // trapTab cycles Tab/Shift+Tab inside an open overlay; extra = the opener
  // itself when it must stay inside the cycle (mobile menu/chat toggles
  // that live outside their panels). It lands in document order.
  function trapTab(e, container, extra) {
    if (!container) return;
    const focusables = visibleFocusables(container);
    if (extra && document.contains(extra)) {
      const pos = container.compareDocumentPosition(extra);
      if (pos & Node.DOCUMENT_POSITION_PRECEDING) focusables.unshift(extra);
      else if (pos & Node.DOCUMENT_POSITION_FOLLOWING) focusables.push(extra);
    }
    if (!focusables.length) {
      e.preventDefault();
      if (container.getAttribute('tabindex') !== null) container.focus();
      return;
    }
    const first = focusables[0];
    const last = focusables[focusables.length - 1];
    const active = document.activeElement;
    if (focusables.indexOf(active) === -1) { e.preventDefault(); first.focus(); }
    else if (e.shiftKey && active === first) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && active === last) { e.preventDefault(); first.focus(); }
  }

  // ── Evidence: shared routing — chat's own column ≥1280 / sheet <760, the ──
  // shared #drawer for everything else (search included at every width).

  let evidenceOpener = null;

  async function openEvidence(id, opts) {
    if (!id) return;
    const o = opts || {};
    const panel = o.container
      ? document.querySelector(o.container)
      : document.querySelector('.evidence-panel');
    const wide = matchMedia('(min-width:1280px)').matches;
    const narrow = matchMedia('(max-width:759px)').matches;
    evidenceOpener = document.activeElement instanceof Element ? document.activeElement : null;
    const html = await fetchHTML('/dashboard/partials/evidence/' + encodeURIComponent(id));
    if (panel && (wide || narrow)) {
      panel.innerHTML = html;
      panel.setAttribute('tabindex', '-1');
      const first = visibleFocusables(panel)[0];
      (first || panel).focus();
    } else {
      showOverlay(html, false);
    }
  }

  function closeEvidence() {
    let held = false;
    document.querySelectorAll('.evidence-panel').forEach((p) => {
      if (p.contains(document.activeElement)) held = true;
      p.innerHTML = '';
    });
    closeDrawer();
    restoreFocus(evidenceOpener, held);
    evidenceOpener = null;
  }


  // ── Delegated data-action dispatch (shared actions; page modules namespace ──
  // their own listeners — evidence:* stays shared here for phase-10 producers).

  // ── Destructive confirm group (P-08): a data-confirm click swaps the ──
  // trigger for an untimed consequence + Confirm/Cancel group — no label
  // swap, no timer. Confirm dispatches enclava:confirm on the trigger (the
  // contract every page module consumes) and honors data-confirm-href;
  // Cancel/Escape restore the trigger and its focus.
  let armedTrigger = null;

  function disarmConfirm(restore) {
    const trigger = armedTrigger;
    if (!trigger) return;
    armedTrigger = null;
    const group = trigger.nextElementSibling;
    const heldFocus = !!(group && group.contains(document.activeElement));
    if (group && group.classList.contains('confirm-group')) group.remove();
    trigger.removeAttribute('aria-expanded');
    trigger.removeAttribute('aria-controls');
    trigger.hidden = false;
    if (restore !== false && document.contains(trigger) &&
        (heldFocus || document.activeElement === document.body)) {
      trigger.focus();
    }
  }

  function commitConfirm() {
    const trigger = armedTrigger;
    if (!trigger) return;
    disarmConfirm(false);
    const href = trigger.dataset.confirmHref || '';
    trigger.dispatchEvent(new CustomEvent('enclava:confirm', { bubbles: true, detail: { href: href } }));
    if (href) location.assign(href);
    // Keyboard continuity: the trigger lost focus when the group armed and
    // nothing else claimed it — put it back. The dispatched listener may
    // already have moved focus (drawer close, navigation).
    if (document.contains(trigger) && document.activeElement === document.body) trigger.focus();
  }

  function armConfirm(trigger) {
    if (trigger === armedTrigger) { disarmConfirm(); return; }
    disarmConfirm();
    armedTrigger = trigger;
    const group = document.createElement('span');
    group.className = 'confirm-group';
    const text = trigger.dataset.confirm || 'Confirm?';
    group.id = 'confirm-group';
    group.setAttribute('role', 'group');
    group.setAttribute('aria-label', text);
    const consequence = document.createElement('span');
    consequence.className = 'confirm-text';
    consequence.textContent = text;
    const yes = document.createElement('button');
    yes.type = 'button';
    yes.className = 'btn danger';
    yes.dataset.action = 'confirm-commit';
    yes.textContent = 'Confirm';
    const no = document.createElement('button');
    no.type = 'button';
    no.className = 'btn';
    no.dataset.action = 'confirm-cancel';
    no.textContent = 'Cancel';
    group.appendChild(consequence);
    group.appendChild(yes);
    group.appendChild(no);
    trigger.hidden = true;
    trigger.setAttribute('aria-expanded', 'true');
    trigger.setAttribute('aria-controls', group.id);
    trigger.insertAdjacentElement('afterend', group);
    no.focus();
  }

  document.addEventListener('click', (e) => {
    if (!(e.target instanceof Element)) return;
    if (e.target.id === 'backdrop') { closeDrawer(); return; }
    if (e.target.closest('.mobile-chats')) {
      // The history-sheet toggle and its in-sheet Close button share the
      // class; a click on either releases the sheet the same way.
      if (mobileChatsOpen()) closeMobileChats();
      else openMobileChats(e.target.closest('.mobile-chats'));
      return;
    }
    const pop = el('pop');
    if (pop && !pop.hidden && !pop.contains(e.target) &&
        !e.target.closest('[data-action="user-menu"],[data-action="conf-pop"]')) {
      closePopover();
    }
    if (mobileChatsOpen() &&
        !e.target.closest('.mobile-chats-list')) {
      closeMobileChats(false);
    }
    if (mobileNavOpen() &&
        !e.target.closest('.sidebar') &&
        !e.target.closest('[data-action="toggle-nav"]')) {
      closeMobileNav(false);
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
      case 'toggle-nav':
        if (mobileNavOpen()) closeMobileNav();
        else openMobileNav(trigger);
        break;
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
      case 'confirm-commit':
        commitConfirm();
        break;
      case 'confirm-cancel':
        disarmConfirm();
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
      if (armedTrigger) { disarmConfirm(); return; }
      if (popoverOpen()) { closePopover(); return; }
      if (mobileChatsOpen()) { closeMobileChats(); return; }
      if (mobileNavOpen()) { closeMobileNav(); return; }
      if (drawerOpen()) closeDrawer();
      return;
    }
    if (e.key !== 'Tab') return;
    if (popoverOpen()) { trapTab(e, el('pop'), null); return; }
    if (mobileChatsOpen()) {
      trapTab(e, document.querySelector('.mobile-chats-list'), mobileChatsOpener);
      return;
    }
    if (mobileNavOpen()) {
      trapTab(e, document.querySelector('.sidebar'), document.querySelector('.mobile-menu'));
      return;
    }
    if (drawerOpen()) trapTab(e, el('drawer'), null);
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
    caps: caps,
    errorAction: errorAction,
    POLL_MS: POLL_MS,
  };
  window.Enclava.version = 'oa';
})();
