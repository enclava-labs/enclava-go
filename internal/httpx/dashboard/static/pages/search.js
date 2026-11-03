/* search.js — the Search page module (OA-33, plan 10-03).
 *
 * Delegated module on window.Enclava + htmx contracts (D-20): no inline
 * handlers, no page-local evidence code (app.js's shared dispatcher owns the
 * evidence:* namespace; Ask-about-this is a real <a> — the module adds
 * nothing for it). Behaviors:
 *   URL state   htmx issues the results GET; the module pushStates
 *               ?q=&mode=&collection= on htmx:afterRequest — only for
 *               user-initiated requests. popstate re-issues the fetch
 *               flagged as a restoration so Back/Forward never create
 *               history entries.
 *   mode        the segmented .chip control writes the hidden mode input
 *   facets      .chip[data-facet] filters rendered .result rows client-side
 *               (post-authorization counts come from the server payload)
 *   recents     last-10 queries per data-recents-key bucket in localStorage
 *               (per authenticated identity — another user on this browser
 *               reads a different bucket)
 */
(function () {
  if (window.__enclavaSearch) return;
  window.__enclavaSearch = true;

  const el = (id) => document.getElementById(id);
  const form = () => el('searchForm');
  let restoring = false; // one-shot flag: the next issued request is a popstate restoration

  function stateURL() {
    const f = form();
    if (!f) return null;
    const url = new URL('/dashboard/search', location.origin);
    url.searchParams.set('q', f.q.value.trim());
    url.searchParams.set('mode', el('searchMode').value || 'hybrid');
    const col = el('searchCollection');
    if (col && col.value) url.searchParams.set('collection', col.value);
    return url.pathname + url.search;
  }

  // Recents are namespaced by the server-emitted data-recents-key.
  function recentsKey() {
    const box = el('searchResults');
    return box && box.dataset.recentsKey ? box.dataset.recentsKey : '';
  }
  function readRecents() {
    try {
      const all = JSON.parse(localStorage.getItem('enclava_recent_searches') || '{}');
      return Array.isArray(all[recentsKey()]) ? all[recentsKey()] : [];
    } catch (_) { return []; }
  }
  function recordRecent(q, mode) {
    if (!q) return;
    try {
      const all = JSON.parse(localStorage.getItem('enclava_recent_searches') || '{}');
      const key = recentsKey();
      const list = (Array.isArray(all[key]) ? all[key] : [])
        .filter((r) => !(r && r.q === q && r.mode === mode));
      list.unshift({ q: q, mode: mode });
      all[key] = list.slice(0, 10);
      localStorage.setItem('enclava_recent_searches', JSON.stringify(all));
    } catch (_) {}
  }
  function renderRecents() {
    const box = el('searchRecents');
    if (!box) return;
    box.textContent = '';
    const list = readRecents();
    if (!list.length) return;
    const head = document.createElement('h2');
    head.textContent = 'Recent searches';
    const filters = document.createElement('div');
    filters.className = 'filters';
    for (const r of list) {
      const chip = document.createElement('button');
      chip.type = 'button';
      chip.className = 'chip';
      chip.dataset.action = 'search:recent';
      chip.dataset.recent = r.q;
      chip.dataset.mode = r.mode || 'hybrid';
      chip.textContent = r.q;
      filters.appendChild(chip);
    }
    box.appendChild(head);
    box.appendChild(filters);
  }

  // Mode chips mirror the hidden input + aria-pressed, then submit.
  function selectMode(chip) {
    const input = el('searchMode');
    if (input) input.value = chip.dataset.mode || 'hybrid';
    chip.parentElement.querySelectorAll('.chip[data-mode]').forEach((c) => {
      c.classList.toggle('active', c === chip);
      c.setAttribute('aria-pressed', c === chip ? 'true' : 'false');
    });
    issue();
  }

  // Facet chips filter the rendered rows client-side — never a re-query.
  function applyFacet(chip) {
    const kind = chip.dataset.facet || '';
    chip.parentElement.querySelectorAll('.chip[data-facet]').forEach((c) => {
      c.classList.toggle('active', c === chip);
      c.setAttribute('aria-pressed', c === chip ? 'true' : 'false');
    });
    document.querySelectorAll('#searchResults .result').forEach((row) => {
      row.style.display = !kind || row.dataset.sourceKind === kind ? '' : 'none';
    });
  }

  function issue(restore) {
    if (!window.htmx || !form()) return;
    if (restore) restoring = true;
    htmx.trigger(form(), 'submit');
  }

  document.addEventListener('click', (e) => {
    if (!(e.target instanceof Element)) return;
    const recent = e.target.closest('[data-action="search:recent"]');
    if (recent) {
      const f = form();
      if (!f) return;
      f.q.value = recent.dataset.recent || '';
      const mode = document.querySelector('.chip[data-mode][data-mode="' + (recent.dataset.mode || 'hybrid') + '"]');
      if (mode) selectMode(mode); // selects + submits
      else issue();
      return;
    }
    const mode = e.target.closest('.chip[data-mode]');
    if (mode && form()) { selectMode(mode); return; }
    const facet = e.target.closest('.chip[data-facet]');
    if (facet) { applyFacet(facet); return; }
  });

  document.addEventListener('change', (e) => {
    if (e.target instanceof Element && e.target.id === 'searchCollection') issue();
  });

  // pushState only for user-initiated, successful requests.
  document.addEventListener('htmx:afterRequest', (e) => {
    const elt = e.detail && e.detail.elt;
    // The "More results" page appended its next-page control inside
    // #searchResultRows already; the clicked control is spent either way.
    if (elt && elt.hasAttribute && elt.hasAttribute('data-search-more')) {
      if (e.detail.successful) elt.closest('#searchMoreRow').remove();
      return;
    }
    const f = form();
    if (!f || elt !== f) return;
    if (restoring) { restoring = false; return; }
    if (!e.detail.successful) return;
    const url = stateURL();
    if (url) history.pushState(null, '', url);
    const q = f.q.value.trim();
    if (q) recordRecent(q, el('searchMode').value || 'hybrid');
  });

  document.addEventListener('htmx:afterSwap', (e) => {
    const target = e.detail && e.detail.target;
    if (target && target.id === 'searchResults') renderRecents();
  });

  // Back/Forward: restore form state from the URL and re-issue the fetch —
  // flagged so the afterRequest handler skips pushState for the restoration.
  window.addEventListener('popstate', () => {
    const f = form();
    if (!f || location.pathname !== '/dashboard/search') return;
    const params = new URLSearchParams(location.search);
    f.q.value = params.get('q') || '';
    const input = el('searchMode');
    const mode = ['hybrid', 'lexical', 'semantic'].includes(params.get('mode')) ? params.get('mode') : 'hybrid';
    if (input) input.value = mode;
    document.querySelectorAll('.chip[data-mode]').forEach((c) => {
      const on = c.dataset.mode === mode;
      c.classList.toggle('active', on);
      c.setAttribute('aria-pressed', on ? 'true' : 'false');
    });
    const col = el('searchCollection');
    if (col) col.value = params.get('collection') || '';
    issue(true);
  });

  document.addEventListener('DOMContentLoaded', () => {
    renderRecents();
  });
})();
