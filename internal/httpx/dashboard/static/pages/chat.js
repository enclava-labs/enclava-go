/* chat.js — the Chat page module (OA-32, plan 10-02).
 *
 * One delegated module on window.Enclava contracts (D-20): no inline
 * handlers, no hx-on, no page-local evidence fetching. Behaviors:
 *   chat:send   POST /api/v1/agent-runs — a 202's conversation_id is
 *               adopted into state + URL (history.replaceState) so later
 *               turns, reloads and deep links address the same conversation
 *   polling     GET /api/v1/agent-runs/{id} every Enclava.POLL_MS while
 *               queued|running — NO time cutoff; a 200 payload carrying
 *               status failed feeds run.error through Enclava.errorAction
 *   chat:stop   the trigger's shared data-confirm arms; chat.js consumes the
 *               enclava:confirm event and DELETEs the run
 *   citations   .bubble [n] text nodes become .citation buttons carrying
 *               data-action=evidence:open (phase-7's shared dispatcher opens
 *               the phase-10 evidence fragment; this module fetches nothing)
 *   chat:new    reset composer/thread/URL to /dashboard/chat
 *
 * Run-state survives reload through DOM markers (.message[data-run-id]) —
 * never globals. Terminal labels come from the server vocabulary via the
 * thread re-render, never client strings.
 */
(function () {
  if (window.__enclavaChat) return;
  window.__enclavaChat = true;

  const TERMINAL = new Set(['succeeded', 'failed', 'cancelled', 'unknown']);
  const state = { conversation: null, run: null, timer: null, busy: false, sending: false };

  const el = (id) => document.getElementById(id);

  function conversationFromURL() {
    const match = location.pathname.match(/^\/dashboard\/chat\/([^/]+)\/?$/);
    return match ? decodeURIComponent(match[1]) : null;
  }

  function composerText() {
    const ta = el('composer');
    return ta ? ta.value.trim() : '';
  }

  function evidenceIDs() {
    const box = el('chatComposer');
    if (!box || !box.dataset.evidence) return [];
    return box.dataset.evidence.split(',').map((s) => s.trim()).filter(Boolean);
  }

  // Send disables while a run is in flight; buttons the server locked
  // (grantless picker) never re-enable.
  function setBusy(busy) {
    state.busy = busy;
    document.querySelectorAll('[data-action="chat:send"]').forEach((b) => {
      if (b.dataset.locked !== '1') b.disabled = busy;
    });
  }

  function loadThread() {
    if (!state.conversation || !window.htmx) return;
    htmx.ajax('GET', '/dashboard/partials/chat/thread?c=' + encodeURIComponent(state.conversation), {
      target: '#chatThread', swap: 'outerHTML',
    }).catch(() => {});
  }

  function stopPolling() {
    clearTimeout(state.timer);
    state.timer = null;
    state.run = null;
  }

  const pollMs = () => (window.Enclava && window.Enclava.POLL_MS) || 3000;

  // failAction renders an API failure through the shared error→action map:
  // sticky error toast carrying the capability-filtered fix link (Set
  // price / administrator fallback) or the built-in Try-again. retryFn
  // becomes the map's onRetry continuation (resumes our own success path);
  // without it the button replays the rejected request verbatim.
  function failAction(err, retryFn) {
    const act = window.Enclava.errorAction
      ? window.Enclava.errorAction(err)
      : null;
    const msg = (act && act.text) || (err && err.message) || 'Something went wrong';
    const opts = {};
    if (act && (act.href || act.retry)) {
      if (act.retry && typeof retryFn === 'function') act.onRetry = retryFn;
      opts.action = act;
    }
    window.Enclava.toast('error', msg, opts);
  }
  // Poll every Enclava.POLL_MS until terminal — no time-based cutoff:
  // queued+running can legitimately outlive the old ~2 min worker bound. A
  // poll-level failure never leaves a frozen runline: sticky toast +
  // composer restored. A 200 payload with status failed feeds the persisted
  // run.error through the shared error→action map; unknown renders the
  // unknown-outcome line via the re-render and stops polling like every
  // other terminal state.
  function startPolling(runID) {
    stopPolling();
    if (!runID) return;
    state.run = runID;
    const tick = async () => {
      if (state.run !== runID) return;
      let run;
      try {
        run = await window.Enclava.api('GET', '/api/v1/agent-runs/' + encodeURIComponent(runID));
      } catch (err) {
        state.run = null;
        setBusy(false);
        if (err && err.status === 401) return; // api() already redirected to login
        failAction(err);
        return;
      }
      if (run && run.status === 'failed') {
        stopPolling();
        setBusy(false);
        failAction({ message: run.error, status: 'failed' });
        window.Enclava.refresh(); // thread re-render carries the runline
        return;
      }
      if (run && TERMINAL.has(run.status)) {
        stopPolling();
        setBusy(false);
        window.Enclava.refresh();
        return;
      }
      state.timer = setTimeout(tick, pollMs());
    };
    state.timer = setTimeout(tick, pollMs());
  }

  async function send() {
    if (state.sending) return;
    const input = composerText();
    if (!input) return;
    const select = el('assistantSelect');
    const body = { assistant_id: select ? select.value : '', input: input };
    if (state.conversation) body.conversation_id = state.conversation;
    const evIDs = evidenceIDs();
    if (evIDs.length) body.evidence_ids = evIDs;
    state.sending = true;
    setBusy(true);
    try {
      const run = await window.Enclava.api('POST', '/api/v1/agent-runs', body);
      const ta = el('composer');
      if (ta) ta.value = '';
      // Adopt the authoritative conversation_id: state, URL and thread.
      const convID = run && run.conversation_id;
      if (convID && convID !== state.conversation) {
        state.conversation = convID;
        history.replaceState(null, '', '/dashboard/chat/' + encodeURIComponent(convID));
        const name = select && select.selectedOptions && select.selectedOptions[0];
        const label = el('chatAssistantName');
        if (name && name.textContent && label && !label.textContent) label.textContent = name.textContent;
        loadThread();
      }
      if (run && run.id) startPolling(run.id);
      window.Enclava.refresh(); // history gains the conversation
    } catch (err) {
      setBusy(false);
      failAction(err, send); // shared map: Set price / budgets / Try again
    } finally {
      state.sending = false;
    }
  }

  function newChat() {
    stopPolling();
    state.conversation = null;
    setBusy(false);
    const ta = el('composer');
    if (ta) ta.value = '';
    const box = el('chatComposer');
    if (box) delete box.dataset.evidence;
    const chip = el('evidenceChip');
    if (chip) chip.hidden = true;
    // User-initiated navigation: push, not replace.
    history.pushState(null, '', '/dashboard/chat');
    if (window.htmx) {
      htmx.ajax('GET', '/dashboard/partials/chat/thread', { target: '#chatThread', swap: 'outerHTML' }).catch(() => {});
    }
    window.Enclava.refresh(); // history drops the selected mark
    if (ta) ta.focus();
  }

  // Citations: split ONLY text nodes inside .bubble (never .toolstep, never
  // innerHTML of model text — XSS). Index resolves against the enclosing
  // article's data-evidence-ids; out-of-range citations toast on click via
  // the delegated handler below.
  function bindCitations(root) {
    (root || document).querySelectorAll('article.message[data-evidence-ids] .bubble').forEach((bubble) => {
      if (bubble.dataset.citationsBound) return;
      bubble.dataset.citationsBound = '1';
      const ids = bubble.closest('[data-evidence-ids]').dataset.evidenceIds.split(',').filter(Boolean);
      const walker = document.createTreeWalker(bubble, NodeFilter.SHOW_TEXT);
      const nodes = [];
      while (walker.nextNode()) nodes.push(walker.currentNode);
      nodes.forEach((node) => {
        const parts = node.textContent.split(/(\[\d+\])/);
        if (parts.length === 1) return;
        const frag = document.createDocumentFragment();
        parts.forEach((part) => {
          const m = part.match(/^\[(\d+)\]$/);
          if (m) {
            const btn = document.createElement('button');
            btn.type = 'button';
            btn.className = 'citation';
            btn.dataset.action = 'evidence:open';
            btn.textContent = '[' + m[1] + ']';
            btn.setAttribute('aria-label', 'Evidence ' + m[1]);
            const id = ids[parseInt(m[1], 10) - 1];
            if (id) btn.dataset.evidenceId = id;
            frag.appendChild(btn);
          } else if (part) {
            frag.appendChild(document.createTextNode(part));
          }
        });
        node.replaceWith(frag);
      });
    });
  }

  // Shared data-confirm arm/disarm lives in app.js; this is the page-side
  // consumer named by the plan: DELETE the run, then let the thread
  // re-render the server's terminal vocabulary.
  document.addEventListener('enclava:confirm', async (e) => {
    const trigger = e.target instanceof Element && e.target.closest('[data-run-id]');
    if (!trigger) return;
    try {
      await window.Enclava.api('DELETE', '/api/v1/agent-runs/' + encodeURIComponent(trigger.dataset.runId));
      stopPolling();
      setBusy(false);
      window.Enclava.refresh();
    } catch (err) {
      if (err && err.status === 401) return;
      failAction(err); // not_found → explanatory only (shared map)
    }
  });

  document.addEventListener('click', (e) => {
    if (!(e.target instanceof Element)) return;
    const action = e.target.closest('[data-action]');
    if (action) {
      switch (action.dataset.action) {
        case 'chat:send':
          send();
          return;
        case 'chat:new':
          newChat();
          return;
        case 'evidence:open':
          if (!action.dataset.evidenceId) {
            window.Enclava.toast('error', 'Evidence no longer available');
          }
          return; // the id-carrying click is handled by app.js's dispatcher
      }
    }
  });

  document.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey && e.target instanceof Element && e.target.id === 'composer') {
      e.preventDefault();
      send();
    }
  });

  // History search filters loaded rows client-side (phase-8 ships no q param).
  document.addEventListener('input', (e) => {
    if (!(e.target instanceof Element) || !e.target.matches('[data-chat-search]')) return;
    const q = e.target.value.toLowerCase();
    document.querySelectorAll('#chatHistory .history-item').forEach((item) => {
      item.style.display = item.textContent.toLowerCase().includes(q) ? '' : 'none';
    });
  });

  document.addEventListener('htmx:afterSwap', (e) => {
    const target = e.detail && e.detail.target;
    bindCitations(target && target.querySelectorAll ? target : document);
  });

  // The shared evidence router (app.js) drops <760 evidence into the
  // .evidence-panel column, whose bottom-sheet presentation is class-driven —
  // frozen app.js never toggles the class, so this module mirrors the
  // viewport onto the panel whenever its content changes.
  function syncEvidenceSheet() {
    const panel = document.querySelector('.evidence-panel');
    if (!panel) return;
    const narrow = matchMedia('(max-width:759px)').matches;
    panel.classList.toggle('sheet', narrow && panel.innerHTML.trim() !== '');
  }

  // On load: conversation state from the URL, citations bound, and any
  // server-rendered queued/running marker resumes polling mid-run.
  document.addEventListener('DOMContentLoaded', () => {
    state.conversation = conversationFromURL();
    bindCitations(document);
    const panel = document.querySelector('.evidence-panel');
    if (panel) {
      new MutationObserver(syncEvidenceSheet).observe(panel, { childList: true });
      matchMedia('(max-width:759px)').addEventListener('change', syncEvidenceSheet);
      syncEvidenceSheet();
    }
    document.querySelectorAll('article.message[data-run-id]').forEach((m) => {
      const status = m.dataset.runStatus;
      if (status === 'queued' || status === 'running') {
        setBusy(true);
        startPolling(m.dataset.runId);
      }
    });
  });
})();
