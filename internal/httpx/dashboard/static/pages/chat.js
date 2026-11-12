/* chat.js — the Chat page module (OA-32, plan 10-02).
 *
 * One delegated module on window.Enclava contracts (D-20): no inline
 * handlers, no hx-on, no page-local evidence fetching. Behaviors:
 *   chat:send   POST /api/v1/agent-runs — a 202's conversation_id is
 *               adopted into state + URL (history.replaceState) so later
 *               turns, reloads and deep links address the same conversation;
 *               inside an open conversation the thread always posts ITS OWN
 *               assistant (composer data-assistant), switching the picker
 *               starts a new conversation (AST-I02)
 *   polling     GET /api/v1/agent-runs/{id} every Enclava.POLL_MS while
 *               queued|running — NO time cutoff; a 200 payload carrying
 *               status failed feeds run.error through Enclava.errorAction
 *   chat:stop   the trigger's shared data-confirm arms; chat.js consumes the
 *               enclava:confirm event and DELETEs the run
 *   markdown    assistant .bubble text renders a SAFE subset (paragraphs,
 *               headings, bold/italic, inline + fenced code, lists, http(s)
 *               links) built from DOM nodes ONLY — createElement/textContent,
 *               never innerHTML with model text
 *   citations   [n] and evidence_<id> markers inside bubbles become
 *               .citation buttons carrying data-action=evidence:open (the
 *               shared dispatcher opens the evidence fragment); markers that
 *               resolve to no id in the turn's evidence set render a muted
 *               "source unavailable" chip
 *   history     conv:rename edits inline → PATCH /api/v1/conversations/{id};
 *               the row's data-confirm DELETEs the same id
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
  const state = { conversation: null, run: null, timer: null, busy: false, sending: false, stream: null };

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
    closeStream();
  }

  // ── Live run stream (best-effort) ───────────────────────────────────────
  // While polling runs, /events may stream deltas into ONE transient
  // assistant bubble. Every failure path is silent: polling + the
  // server-rendered thread remain the source of truth.
  function streamBubble() {
    const s = state.stream;
    if (s && document.contains(s.bubble)) return s.bubble;
    const thread = el('chatThread');
    if (!thread) return null;
    const article = document.createElement('article');
    article.className = 'message assistant';
    const author = document.createElement('div');
    author.className = 'author';
    author.textContent = 'Assistant';
    const bubble = document.createElement('div');
    bubble.className = 'bubble';
    // bindBubble must never re-process our own markdown output.
    bubble.dataset.citationsBound = '1';
    article.appendChild(author);
    article.appendChild(bubble);
    thread.appendChild(article);
    if (s) s.bubble = bubble;
    return bubble;
  }

  function renderStreamText() {
    const s = state.stream;
    if (!s || !s.bubble || !document.contains(s.bubble)) return;
    s.bubble.textContent = '';
    s.bubble.appendChild(renderMarkdown(s.text));
  }

  function closeStream() {
    const s = state.stream;
    state.stream = null;
    if (!s) return;
    if (s.ctrl) s.ctrl.abort();
    if (s.bubble && s.bubble.parentNode) s.bubble.parentNode.remove();
  }

  function openStream(runID) {
    closeStream();
    if (!window.fetch || typeof window.ReadableStream !== 'function') return;
    const ctrl = new AbortController();
    const s = { ctrl, bubble: null, text: '' };
    state.stream = s; // the bubble is created lazily on the first delta
    fetch('/api/v1/agent-runs/' + encodeURIComponent(runID) + '/events', {
      signal: ctrl.signal, headers: { accept: 'text/event-stream' },
    }).then((res) => {
      if (!res.ok || !res.body) throw new Error('stream unavailable');
      const reader = res.body.getReader();
      const dec = new TextDecoder();
      let buf = '';
      const pump = () => reader.read().then((r) => {
        if (r.done || state.stream !== s) return;
        buf += dec.decode(r.value, { stream: true });
        let cut;
        while ((cut = buf.indexOf('\n\n')) >= 0) {
          const frame = buf.slice(0, cut);
          buf = buf.slice(cut + 2);
          let kind = '', data = '';
          for (const line of frame.split('\n')) {
            if (line.startsWith('event:')) kind = line.slice(6).trim();
            else if (line.startsWith('data:')) data += line.slice(5).trim();
          }
          if (state.stream !== s) return;
          if (kind === 'delta' && data) {
            s.text += JSON.parse(data);
            streamBubble();
            renderStreamText();
          } else if (kind === 'reset') {
            s.text = '';
            renderStreamText();
          } else if (kind === 'done') {
            return;
          }
        }
        return pump();
      });
      return pump();
    }).catch(() => {});
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
    openStream(runID);
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
    const box = el('chatComposer');
    // An open conversation always posts its own assistant (server-rendered
    // data-assistant); the picker only chooses the NEXT conversation's.
    const assistantID = state.conversation && box && box.dataset.assistant
      ? box.dataset.assistant
      : (select ? select.value : '');
    const body = { assistant_id: assistantID, input: input };
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
        if (box) box.dataset.assistant = assistantID;
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
    if (box) {
      delete box.dataset.evidence;
      delete box.dataset.assistant;
    }
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

  // ── Safe Markdown subset (DOM nodes only) ──────────────────────────────
  // No innerHTML/insertAdjacentHTML with model text: every block is built
  // through createElement and text comes from textContent → createTextNode.

  const INLINE_RULES = [
    { re: /`([^`\n]+)`/, make: (m) => {
      const code = document.createElement('code');
      code.textContent = m[1];
      return code;
    } },
    { re: /\[([^\]\n]+)\]\((https?:\/\/[^\s)]+)\)/, make: (m) => {
      const a = document.createElement('a');
      a.href = m[2];
      a.target = '_blank';
      a.rel = 'noopener noreferrer';
      a.textContent = m[1];
      return a;
    } },
    { re: /\*\*([^*\n]+)\*\*|__([^_\n]+)__/, make: (m) => {
      const strong = document.createElement('strong');
      strong.textContent = m[1] !== undefined ? m[1] : m[2];
      return strong;
    } },
    // Italic needs a non-marker boundary so *em* mid-word stays literal;
    // pre holds the boundary group index emitted back as text.
    { re: /(^|[\s(])\*([^*\n]+)\*(?!\*)|(^|[\s(])_([^_\n]+)_(?!_)/, pre: 1, make: (m) => {
      const em = document.createElement('em');
      em.textContent = m[2] !== undefined ? m[2] : m[4];
      return em;
    } },
  ];

  // appendInline walks src for the EARLIEST rule match each round so
  // earlier rules (code spans, links) outrank emphasis at the same offset.
  function appendInline(src, parent) {
    let rest = src;
    for (;;) {
      let best = null;
      for (const rule of INLINE_RULES) {
        const m = rule.re.exec(rest);
        if (m && (best === null || m.index < best.m.index)) best = { m, rule };
      }
      if (!best) break;
      const { m, rule } = best;
      if (m.index > 0) parent.appendChild(document.createTextNode(rest.slice(0, m.index)));
      if (rule.pre && m[rule.pre]) parent.appendChild(document.createTextNode(m[rule.pre]));
      parent.appendChild(rule.make(m));
      rest = rest.slice(m.index + m[0].length);
    }
    if (rest) parent.appendChild(document.createTextNode(rest));
  }

  function renderMarkdown(text) {
    const frag = document.createDocumentFragment();
    for (const block of text.replace(/\r\n?/g, '\n').split(/\n{2,}/)) {
      const raw = block.replace(/^\n+|\n+$/g, '');
      if (!raw.trim()) continue;
      let m;
      if ((m = raw.match(/^```[^\n]*\n?([\s\S]*?)```$/))) {
        const pre = document.createElement('pre');
        const code = document.createElement('code');
        code.textContent = m[1].replace(/\n+$/, '');
        pre.appendChild(code);
        frag.appendChild(pre);
        continue;
      }
      if ((m = raw.match(/^(#{1,4})\s+(.+)$/s))) {
        const level = Math.min(m[1].length + 2, 6); // h1 page → h3..h6 in-thread
        const h = document.createElement('h' + level);
        appendInline(m[2], h);
        frag.appendChild(h);
        continue;
      }
      const lines = raw.split('\n');
      if (lines.every((l) => /^\s*(?:[-*+]\s+|\d+[.)]\s+)/.test(l))) {
        const ordered = /^\s*\d/.test(lines[0]);
        const list = document.createElement(ordered ? 'ol' : 'ul');
        for (const l of lines) {
          const li = document.createElement('li');
          appendInline(l.replace(/^\s*(?:[-*+]\s+|\d+[.)]\s+)/, ''), li);
          list.appendChild(li);
        }
        frag.appendChild(list);
        continue;
      }
      const p = document.createElement('p');
      appendInline(lines.join('\n'), p);
      frag.appendChild(p);
    }
    return frag;
  }

  // ── Citations ──────────────────────────────────────────────────────────
  // [n] and evidence_<id> (optionally bracketed) inside bubbles resolve
  // against the enclosing article's data-evidence-ids; markers with no id
  // render a muted "source unavailable" chip — never a button, never a
  // dead literal.

  // The last char must be id-ish so trailing sentence punctuation
  // ("... cited evidence_ev_beta.") never gloms onto the token.
  const CITE_RE = /(\[\d+\]|\[?evidence_[A-Za-z0-9_.:-]*[A-Za-z0-9_-]\]?)/;

  function chip(label) {
    const span = document.createElement('span');
    span.className = 'citation muted';
    span.title = 'Source unavailable';
    span.textContent = label;
    return span;
  }

  function citeButton(label, id) {
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'citation';
    btn.dataset.action = 'evidence:open';
    btn.dataset.evidenceId = id;
    btn.textContent = label;
    btn.setAttribute('aria-label', 'Evidence ' + label.replace(/[\[\]]/g, ''));
    return btn;
  }

  // bindBubble renders one .bubble: assistant bubbles get the Markdown
  // subset first; every bubble's text nodes then split on the citation
  // tokens. Text inside pre/code/a stays literal. `flag` prevents double
  // processing across htmx re-scans.
  function bindBubble(bubble, hasKnowledge) {
    if (bubble.dataset.citationsBound) return;
    bubble.dataset.citationsBound = '1';
    const article = bubble.closest('article.message');
    const isAssistant = !!article && !article.classList.contains('user');
    const owner = bubble.closest('[data-evidence-ids]');
    const ids = owner ? owner.dataset.evidenceIds.split(',').filter(Boolean) : [];
    if (isAssistant) {
      const src = bubble.textContent;
      bubble.textContent = '';
      bubble.appendChild(renderMarkdown(src));
    }
    const walker = document.createTreeWalker(bubble, NodeFilter.SHOW_TEXT, {
      acceptNode(n) {
        return n.parentElement && n.parentElement.closest('pre,code,a')
          ? NodeFilter.FILTER_REJECT
          : NodeFilter.FILTER_ACCEPT;
      },
    });
    const nodes = [];
    while (walker.nextNode()) nodes.push(walker.currentNode);
    for (const node of nodes) {
      const parts = node.textContent.split(CITE_RE);
      if (parts.length === 1) continue;
      const frag = document.createDocumentFragment();
      for (const part of parts) {
        if (!part) continue;
        const num = part.match(/^\[(\d+)\]$/);
        const ev = part.match(/^\[?(evidence_[A-Za-z0-9_.:-]*[A-Za-z0-9_-])\]?$/);
        if (num) {
          const id = ids[parseInt(num[1], 10) - 1];
          frag.appendChild(id ? citeButton('[' + num[1] + ']', id) : chip('source unavailable'));
        } else if (ev) {
          const idx = ids.indexOf(ev[1]);
          frag.appendChild(idx >= 0 ? citeButton('[' + (idx + 1) + ']', ev[1]) : chip('source unavailable'));
        } else {
          frag.appendChild(document.createTextNode(part));
        }
      }
      node.replaceWith(frag);
    }
    if (isAssistant && hasKnowledge && ids.length === 0 && !bubble.querySelector('.no-sources')) {
      const p = document.createElement('p');
      p.className = 'no-sources muted';
      p.textContent = 'No sources were returned for this answer.';
      bubble.appendChild(p);
    }
  }

  function bindAll(root) {
    (root || document).querySelectorAll('article.message .bubble').forEach((bubble) => {
      const thread = bubble.closest('.thread-body');
      bindBubble(bubble, !!(thread && thread.dataset.hasKnowledge === '1'));
    });
  }

  // Exposed for the headless renderer suite (tests drive .render/.bindAll
  // without a live backend). Runtime paths call bindAll directly.
  window.__enclavaChat = {
    render: (bubble, text, ids, hasKnowledge) => {
      bubble.textContent = text;
      if (ids && ids.length) bubble.dataset.evidenceIds = ids.join(',');
      bindBubble(bubble, !!hasKnowledge);
      return bubble;
    },
    bindAll,
  };

  // Shared data-confirm arm/disarm lives in app.js; this is the page-side
  // consumer named by the plan: DELETE the run, then let the thread
  // re-render the server's terminal vocabulary. History rows arm the same
  // trigger to DELETE the conversation itself.
  document.addEventListener('enclava:confirm', async (e) => {
    if (!(e.target instanceof Element)) return;
    const delRow = e.target.closest('[data-conv-id]');
    if (e.target.hasAttribute('data-conv-delete') && delRow) {
      const id = delRow.dataset.convId;
      try {
        await window.Enclava.api('DELETE', '/api/v1/conversations/' + encodeURIComponent(id));
      } catch (err) {
        if (err && err.status === 401) return;
        failAction(err);
        return;
      }
      // The row exists in BOTH the history nav and the mobile sheet; drop
      // every copy, then either reset the open thread or re-render the nav.
      document.querySelectorAll('.h-item[data-conv-id="' + CSS.escape(id) + '"]').forEach((row) => row.remove());
      if (state.conversation === id) newChat();
      else window.Enclava.refresh();
      return;
    }
    const trigger = e.target.closest('[data-run-id]');
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

  // Rename: swap the row's link for an inline input; Enter commits through
  // PATCH, Escape reverts, blur behaves like Enter. Only the history
  // re-render confirms the new title — no optimistic text.
  function startRename(row) {
    if (!row || row.querySelector('.h-rename')) return;
    const link = row.querySelector('a.history-item');
    const titleEl = row.querySelector('.h-title');
    if (!link || !titleEl) return;
    const input = document.createElement('input');
    input.type = 'text';
    input.className = 'h-rename';
    input.maxLength = 200;
    input.value = titleEl.textContent;
    input.setAttribute('aria-label', 'Conversation title');
    link.hidden = true;
    link.after(input);
    input.focus();
    input.select();
    let done = false;
    const cleanup = () => {
      if (done) return;
      done = true;
      input.remove();
      link.hidden = false;
    };
    const save = async () => {
      const title = input.value.trim();
      if (!title || title === titleEl.textContent) { cleanup(); return; }
      try {
        await window.Enclava.api('PATCH', '/api/v1/conversations/' + encodeURIComponent(row.dataset.convId), { title });
        cleanup();
        window.Enclava.refresh();
      } catch (err) {
        if (err && err.status === 401) return;
        cleanup();
        failAction(err);
      }
    };
    input.addEventListener('keydown', (ev) => {
      if (ev.key === 'Enter') { ev.preventDefault(); save(); }
      else if (ev.key === 'Escape') { ev.preventDefault(); cleanup(); }
    });
    input.addEventListener('blur', save);
  }

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
        case 'conv:rename':
          startRename(action.closest('[data-conv-id]'));
          return;
        case 'evidence:open':
          return; // id-carrying clicks are handled by app.js's dispatcher
      }
    }
  });

  document.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey && e.target instanceof Element && e.target.id === 'composer') {
      e.preventDefault();
      send();
    }
  });

  // A different assistant inside an open conversation never posts to it:
  // the picker's selection belongs to the NEXT conversation.
  document.addEventListener('change', (e) => {
    if (e.target instanceof Element && e.target.id === 'assistantSelect' && state.conversation) {
      newChat();
    }
  });

  // History search filters loaded rows client-side (phase-8 ships no q param).
  document.addEventListener('input', (e) => {
    if (!(e.target instanceof Element) || !e.target.matches('[data-chat-search]')) return;
    const q = e.target.value.toLowerCase();
    document.querySelectorAll('#chatHistory .h-item').forEach((item) => {
      item.style.display = item.textContent.toLowerCase().includes(q) ? '' : 'none';
    });
  });

  document.addEventListener('htmx:afterSwap', () => {
    // Always scan the document: the body-dispatched `refresh` event that
    // re-renders the thread leaves e.detail.target as the triggered node,
    // which can miss the swapped fragment's citation bubbles.
    bindAll(document);
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

  // On load: conversation state from the URL, bubbles bound, and any
  // server-rendered queued/running marker resumes polling mid-run.
  document.addEventListener('DOMContentLoaded', () => {
    state.conversation = conversationFromURL();
    bindAll(document);
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
